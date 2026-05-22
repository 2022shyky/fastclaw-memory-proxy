package proxy

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/shyky/memory-proxy/internal/canvas"
	"github.com/shyky/memory-proxy/internal/config"
	"github.com/shyky/memory-proxy/internal/memory"
	"github.com/shyky/memory-proxy/internal/stats"
)

type MemoryProxy struct {
	cfg       *config.Config
	canvas    *canvas.Engine
	stats     *stats.Collector
	memory    *memory.Store
	toolNames map[string]string // tool_call id → name mapping
}

func New(cfg *config.Config) (*MemoryProxy, error) {
	store, err := memory.New(cfg.DataDir)
	if err != nil {
		return nil, fmt.Errorf("init memory store: %w", err)
	}

	return &MemoryProxy{
		cfg:       cfg,
		canvas:    canvas.New(cfg.DataDir),
		stats:     stats.New(cfg),
		memory:    store,
		toolNames: make(map[string]string),
	}, nil
}

func (p *MemoryProxy) Start() error {
	mux := http.NewServeMux()
	// FastClaw actual routes: /api/chat (non-streaming) and /api/chat/stream (streaming)
	mux.HandleFunc("/api/chat", p.handleChat)
	mux.HandleFunc("/api/chat/stream", p.handleChatStream)
	mux.HandleFunc("/health", p.handleHealth)

	slog.Info("Memory Proxy starting", "listen", p.cfg.Listen, "target", p.cfg.Target)
	return http.ListenAndServe(p.cfg.Listen, mux)
}

func (p *MemoryProxy) handleHealth(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	w.Write([]byte(`{"status":"ok"}`))
}

// handleChat proxies non-streaming chat requests
func (p *MemoryProxy) handleChat(w http.ResponseWriter, r *http.Request) {
	bodyBytes, err := io.ReadAll(r.Body)
	if err != nil {
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}
	r.Body.Close()

	var reqBody map[string]interface{}
	json.Unmarshal(bodyBytes, &reqBody)
	agentID, _ := reqBody["agentId"].(string)
	sessionID := fmt.Sprintf("session-%d", time.Now().UnixNano())

	// Forward to FastClaw
	targetURL := p.cfg.Target + "/api/chat"
	proxyReq, err := http.NewRequest("POST", targetURL, strings.NewReader(string(bodyBytes)))
	if err != nil {
		http.Error(w, "proxy error", http.StatusInternalServerError)
		return
	}
	proxyReq.Header.Set("Content-Type", "application/json")
	if p.cfg.APIKey != "" {
		proxyReq.Header.Set("Authorization", "Bearer "+p.cfg.APIKey)
	}

	client := &http.Client{Timeout: 10 * time.Minute}
	resp, err := client.Do(proxyReq)
	if err != nil {
		slog.Error("proxy request failed", "error", err)
		http.Error(w, "upstream error", http.StatusBadGateway)
		return
	}
	defer resp.Body.Close()

	// Pass through response
	w.Header().Set("Content-Type", "application/json")
	io.Copy(w, resp.Body)

	slog.Info("non-stream session complete", "agent", agentID, "session", sessionID)
}

// handleChatStream proxies streaming chat requests and intercepts tool results
func (p *MemoryProxy) handleChatStream(w http.ResponseWriter, r *http.Request) {
	bodyBytes, err := io.ReadAll(r.Body)
	if err != nil {
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}
	r.Body.Close()

	var reqBody map[string]interface{}
	json.Unmarshal(bodyBytes, &reqBody)
	agentID, _ := reqBody["agentId"].(string)
	sessionID := fmt.Sprintf("session-%d", time.Now().UnixNano())

	// Forward to FastClaw streaming endpoint
	targetURL := p.cfg.Target + "/api/chat/stream"
	proxyReq, err := http.NewRequest("POST", targetURL, strings.NewReader(string(bodyBytes)))
	if err != nil {
		http.Error(w, "proxy error", http.StatusInternalServerError)
		return
	}
	proxyReq.Header.Set("Content-Type", "application/json")
	if p.cfg.APIKey != "" {
		proxyReq.Header.Set("Authorization", "Bearer "+p.cfg.APIKey)
	}

	client := &http.Client{Timeout: 10 * time.Minute}
	resp, err := client.Do(proxyReq)
	if err != nil {
		slog.Error("proxy request failed", "error", err)
		http.Error(w, "upstream error", http.StatusBadGateway)
		return
	}
	defer resp.Body.Close()

	// Check if response is streaming (SSE) or JSON
	contentType := resp.Header.Get("Content-Type")
	isStream := strings.Contains(contentType, "text/event-stream") || strings.Contains(contentType, "application/x-ndjson")

	if !isStream {
		// Non-streaming response — pass through
		w.Header().Set("Content-Type", contentType)
		io.Copy(w, resp.Body)
		return
	}

	// Streaming response — set SSE headers
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.Header().Set("X-Accel-Buffering", "no")

	flusher, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "streaming unsupported", http.StatusInternalServerError)
		return
	}

	sess := p.stats.NewSession(agentID, sessionID)

	scanner := bufio.NewScanner(resp.Body)
	scanner.Buffer(make([]byte, 1024*1024), 10*1024*1024)

	for scanner.Scan() {
		line := scanner.Text()

		// SSE comment lines (starting with ":") — pass through
		if strings.HasPrefix(line, ":") {
			fmt.Fprintf(w, "%s\n", line)
			flusher.Flush()
			continue
		}

		// Empty lines — pass through (SSE event boundary)
		if line == "" {
			fmt.Fprintf(w, "\n")
			flusher.Flush()
			continue
		}

		// SSE data: prefix — extract the JSON payload
		dataPayload := line
		if strings.HasPrefix(line, "data: ") {
			dataPayload = strings.TrimPrefix(line, "data: ")
		}

		// Try intercepting tool results from the JSON payload
		modified := p.processLine(dataPayload, agentID, sessionID, sess)
		if modified != "" {
			fmt.Fprintf(w, "data: %s\n\n", modified)
		} else {
			fmt.Fprintf(w, "%s\n", line)
		}
		flusher.Flush()
	}

	p.canvas.Finalize(agentID, sessionID)
	p.stats.FinalizeSession(sess)
	p.memory.SaveSessionSummary(agentID, sessionID, sess)

	slog.Info("stream session complete",
		"agent", agentID,
		"session", sessionID,
		"tools_intercepted", sess.ToolResultsIntercepted,
		"chars_saved", sess.TotalCharsOffloaded,
		"tokens_saved", sess.EstimatedTokensSaved,
	)
}

// processLine inspects a single JSON line from the stream.
// Returns modified JSON if interception happened, empty string to pass through.
func (p *MemoryProxy) processLine(line, agentID, sessionID string, sess *stats.SessionStats) string {
	var event map[string]interface{}
	if err := json.Unmarshal([]byte(line), &event); err != nil {
		return "" // pass through non-JSON lines
	}

	eventType, _ := event["type"].(string)

	switch eventType {
	case "tool_call":
		p.canvas.AddNode(agentID, sessionID, event)
		sess.CanvasUpdates++
		// Cache id → name mapping for tool_result lookup
		if data, ok := event["data"].(map[string]interface{}); ok {
			if id, ok := data["id"].(string); ok {
				if name, ok := data["name"].(string); ok {
					p.toolNames[id] = name
				}
			}
		}

	case "tool_result":
		resultData, ok := event["data"]
		if !ok {
			return ""
		}
		resultBytes, _ := json.Marshal(resultData)
		resultStr := string(resultBytes)

		// Skip small results (less than 500 chars — not worth offloading)
		if len(resultStr) < 500 {
			return ""
		}

		toolName := "unknown"
		if data, ok := resultData.(map[string]interface{}); ok {
			// FastClaw tool_result uses data.name
			if tn, ok := data["name"].(string); ok {
				toolName = tn
			} else if tn, ok := data["tool_name"].(string); ok {
				toolName = tn
			}
			// Fallback: look up by id from cached tool_calls
			if toolName == "unknown" {
				if id, ok := data["id"].(string); ok {
					if name, ok := p.toolNames[id]; ok {
						toolName = name
					}
				}
			}
		}
		filename := fmt.Sprintf("%s-%s-%s.md",
			time.Now().Format("20060102-150405"),
			sanitizeName(toolName),
			randomSuffix(4),
		)

		refPath := p.writeRef(agentID, filename, toolName, resultStr)
		p.writeTaskLog(agentID, toolName, resultStr)

		p.canvas.MarkDone(agentID, sessionID, toolName)

		sess.ToolResultsIntercepted++
		sess.TotalCharsOffloaded += len(resultStr)
		sess.EstimatedTokensSaved += len(resultStr) / 4

		summary := map[string]interface{}{
			"type": "tool_result",
			"data": map[string]interface{}{
				"summary": fmt.Sprintf("[工具结果已保存至 %s，共 %d 字符]",
					refPath, len([]rune(resultStr))),
				"ref": refPath,
			},
		}
		modified, _ := json.Marshal(summary)
		return string(modified)

	case "done":
		if data, ok := event["data"].(map[string]interface{}); ok {
			if usage, ok := data["usage"].(map[string]interface{}); ok {
				if pt, ok := usage["prompt_tokens"].(float64); ok {
					sess.TotalPromptTokens += int(pt)
				}
				if ct, ok := usage["completion_tokens"].(float64); ok {
					sess.TotalCompletionTokens += int(ct)
				}
			}
		}
	}

	return ""
}

func (p *MemoryProxy) writeRef(agentID, filename, toolName, content string) string {
	dir := fmt.Sprintf("%s/refs/%s", p.cfg.DataDir, agentID)
	os.MkdirAll(dir, 0755)

	filePath := fmt.Sprintf("%s/%s", dir, filename)
	md := fmt.Sprintf("# %s 工具结果\n\n> 时间: %s\n> 工具: %s\n\n```json\n%s\n```",
		toolName, time.Now().Format("2006-01-02 15:04:05"), toolName, content)

	os.WriteFile(filePath, []byte(md), 0644)
	return fmt.Sprintf("refs/%s/%s", agentID, filename)
}

func (p *MemoryProxy) writeTaskLog(agentID, toolName, result string) {
	dir := fmt.Sprintf("%s/tasks/%s", p.cfg.DataDir, agentID)
	os.MkdirAll(dir, 0755)

	date := time.Now().Format("2006-01-02")
	filePath := fmt.Sprintf("%s/%s.jsonl", dir, date)

	entry := map[string]interface{}{
		"timestamp": time.Now().Format(time.RFC3339),
		"tool":      toolName,
		"result_len": len(result),
	}
	line, _ := json.Marshal(entry)

	f, err := os.OpenFile(filePath, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0644)
	if err != nil {
		slog.Error("write task log", "error", err)
		return
	}
	defer f.Close()
	f.Write(line)
	f.Write([]byte("\n"))
}

func sanitizeName(name string) string {
	// Replace unsafe filename characters
	name = strings.ReplaceAll(name, "/", "-")
	name = strings.ReplaceAll(name, "\\", "-")
	name = strings.ReplaceAll(name, " ", "_")
	if len(name) > 30 {
		name = name[:30]
	}
	return name
}

func randomSuffix(n int) string {
	const letters = "abcdefghijklmnopqrstuvwxyz0123456789"
	b := make([]byte, n)
	for i := range b {
		b[i] = letters[time.Now().UnixNano()%int64(len(letters))]
		time.Sleep(time.Microsecond)
	}
	return string(b)
}