package proxy

import (
	"bufio"
	"crypto/rand"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http/httputil"
	"net/http"
	"net/url"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/shyky/memory-proxy/internal/canvas"
	"github.com/shyky/memory-proxy/internal/config"
	"github.com/shyky/memory-proxy/internal/memory"
	"github.com/shyky/memory-proxy/internal/stats"
)

// toolCallCache provides session-isolated, thread-safe caching of tool call metadata.
type toolCallCache struct {
	sync.RWMutex
	calls map[string]map[string]toolCallInfo // sessionID → toolCallID → info
}

type toolCallInfo struct {
	name string
	args string
}

func newToolCallCache() *toolCallCache {
	return &toolCallCache{calls: make(map[string]map[string]toolCallInfo)}
}

func (c *toolCallCache) Set(sessionID, callID, name, args string) {
	c.Lock()
	defer c.Unlock()
	if _, ok := c.calls[sessionID]; !ok {
		c.calls[sessionID] = make(map[string]toolCallInfo)
	}
	c.calls[sessionID][callID] = toolCallInfo{name: name, args: args}
}

func (c *toolCallCache) Get(sessionID, callID string) (toolCallInfo, bool) {
	c.RLock()
	defer c.RUnlock()
	sess, ok := c.calls[sessionID]
	if !ok {
		return toolCallInfo{}, false
	}
	info, ok := sess[callID]
	return info, ok
}

func (c *toolCallCache) Cleanup(sessionID string) {
	c.Lock()
	defer c.Unlock()
	delete(c.calls, sessionID)
}

func (c *toolCallCache) Len() int {
	c.RLock()
	defer c.RUnlock()
	return len(c.calls)
}

type MemoryProxy struct {
	cfg       *config.Config
	canvas    *canvas.Engine
	stats     *stats.Collector
	memory    *memory.Store
	toolCache *toolCallCache
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
		toolCache: newToolCallCache(),
	}, nil
}

func (p *MemoryProxy) Start() error {
	mux := http.NewServeMux()
	// FastClaw actual routes: /api/chat (non-streaming) and /api/chat/stream (streaming)
	mux.HandleFunc("/api/chat", p.handleChat)
	mux.HandleFunc("/api/chat/stream", p.handleChatStream)
	mux.HandleFunc("/health", p.handleHealth)

	// REST API endpoints (memory-manager & external tools)
	mux.HandleFunc("POST /api/refs", p.handleCreateRef)
	mux.HandleFunc("GET /api/refs/{ref_id}", p.handleGetRef)
	mux.HandleFunc("GET /api/refs", p.handleListRefs)
	mux.HandleFunc("POST /api/memory", p.handleCreateMemory)
	mux.HandleFunc("GET /api/memory/search", p.handleSearchMemory)
	mux.HandleFunc("DELETE /api/memory/{memory_id}", p.handleDeleteMemory)
	mux.HandleFunc("GET /api/persona/{user_id}", p.handleGetPersona)
	mux.HandleFunc("PUT /api/persona/{user_id}", p.handleUpdatePersona)

	// Default catch-all: reverse proxy everything else to FastClaw
	targetURL, _ := url.Parse(p.cfg.Target)
	rp := httputil.NewSingleHostReverseProxy(targetURL)
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		slog.Debug("passthrough", "path", r.URL.Path, "method", r.Method)
		rp.ServeHTTP(w, r)
	})

	slog.Info("Memory Proxy starting", "listen", p.cfg.Listen, "target", p.cfg.Target)
	return http.ListenAndServe(p.cfg.Listen, mux)
}

func (p *MemoryProxy) handleHealth(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	w.Write([]byte(`{"status":"ok"}`))
}

// isManagedAgent checks if the agent is in the managed list.
// An empty list means all agents are managed.
func (p *MemoryProxy) isManagedAgent(agentID string) bool {
	if len(p.cfg.Agents) == 0 {
		return true
	}
	for _, a := range p.cfg.Agents {
		if a == agentID {
			return true
		}
	}
	return false
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

// handleChatStream proxies streaming chat requests and intercepts tool results.
func (p *MemoryProxy) handleChatStream(w http.ResponseWriter, r *http.Request) {
	// GET = EventSource reconnect — just reverse-proxy raw, no offloading
	if r.Method == "GET" {
		targetURL, _ := url.Parse(p.cfg.Target)
		httputil.NewSingleHostReverseProxy(targetURL).ServeHTTP(w, r)
		return
	}

	bodyBytes, err := io.ReadAll(r.Body)
	if err != nil {
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}
	r.Body.Close()

	agentID, modifiedBody := p.extractAgentAndInjectCanvas(bodyBytes)
	sessionID := fmt.Sprintf("session-%d", time.Now().UnixNano())

	managed := p.isManagedAgent(agentID)

	// Forward to FastClaw streaming endpoint
	targetURL := p.cfg.Target + "/api/chat/stream"
	proxyReq, err := http.NewRequest("POST", targetURL, strings.NewReader(string(modifiedBody)))
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

	var sess *stats.SessionStats
	if managed {
		sess = p.stats.NewSession(agentID, sessionID)
	}

	// Close upstream connection when client disconnects
	clientDone := make(chan struct{})
	go func() {
		select {
		case <-r.Context().Done():
			slog.Warn("client disconnected, closing upstream", "agent", agentID, "session", sessionID)
			resp.Body.Close()
		case <-clientDone:
		}
	}()

	reader := bufio.NewReader(resp.Body)

	for {
		lineBytes, err := reader.ReadBytes('\n')
		if err != nil {
			if err == io.EOF {
				break
			}
			slog.Warn("SSE stream closed unexpectedly", "error", err, "agent", agentID, "session", sessionID)
			break
		}

		line := strings.TrimSuffix(string(lineBytes), "\r\n")
		line = strings.TrimSuffix(line, "\n")

		// For unmanaged agents, pass through SSE lines unmodified
		if !managed {
			w.Write(lineBytes)
			flusher.Flush()
			continue
		}

		// SSE comment lines (starting with ":") — pass through
		if strings.HasPrefix(line, ":") {
			w.Write(lineBytes)
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
			w.Write(lineBytes)
		}
		flusher.Flush()
	}
	close(clientDone)

	if managed {
		p.canvas.Finalize(agentID, sessionID)
		p.stats.FinalizeSession(sess)
		p.memory.SaveSessionSummary(agentID, sessionID, sess)
		p.toolCache.Cleanup(sessionID)
	}

	slog.Info("stream session complete",
		"agent", agentID,
		"session", sessionID,
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
		// Cache id → name mapping for tool_result lookup (session-isolated)
		if data, ok := event["data"].(map[string]interface{}); ok {
			if id, ok := data["id"].(string); ok {
				name, _ := data["name"].(string)
				args, _ := data["arguments"].(string)
				if name != "" {
					p.toolCache.Set(sessionID, id, name, args)
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
					if info, ok := p.toolCache.Get(sessionID, id); ok {
						toolName = info.name
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

		toolEmoji := "🛠️"
		switch toolName {
		case "web_fetch":
			toolEmoji = "🌐"
		case "web_search":
			toolEmoji = "🔍"
		case "exec":
			toolEmoji = "💻"
		case "read_file":
			toolEmoji = "📄"
		case "write_file", "edit_file":
			toolEmoji = "✏️"
		}

		// Try to get tool call context (URL, command, etc.)
		toolContext := ""
		if data, ok := resultData.(map[string]interface{}); ok {
			if id, ok := data["id"].(string); ok {
				if info, ok := p.toolCache.Get(sessionID, id); ok && info.args != "" {
					var argsMap map[string]interface{}
					if err := json.Unmarshal([]byte(info.args), &argsMap); err == nil {
						if url, ok := argsMap["url"].(string); ok {
							toolContext = url
						} else if cmd, ok := argsMap["command"].(string); ok {
							if len(cmd) > 60 {
								cmd = cmd[:60] + "..."
							}
							toolContext = cmd
						} else if query, ok := argsMap["query"].(string); ok {
							if len(query) > 60 {
								query = query[:60] + "..."
							}
							toolContext = query
						}
					}
				}
			}
		}

		summaryLine := fmt.Sprintf("%s  %s  · 已完成  %d 字符", toolEmoji, toolName, len([]rune(resultStr)))
		if toolContext != "" {
			summaryLine = fmt.Sprintf("%s  %s · %s  · 已完成  %d 字符", toolEmoji, toolName, toolContext, len([]rune(resultStr)))
		}

		charCount := len([]rune(resultStr))
		summary := map[string]interface{}{
			"type": "tool_result",
			"data": map[string]interface{}{
				"summary":    fmt.Sprintf("%s", summaryLine),
				"ref":        refPath,
				"tool_name":  toolName,
				"char_count": charCount,
				"context":    toolContext,
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
		"timestamp":  time.Now().Format(time.RFC3339),
		"tool":       toolName,
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
	rand.Read(b)
	for i := range b {
		b[i] = letters[int(b[i])%len(letters)]
	}
	return string(b)
}

// injectCanvas reads the latest canvas Mermaid for this agent and injects it
// into the system message of the request body, so the LLM sees the task canvas.
func (p *MemoryProxy) injectCanvas(agentID string, bodyBytes []byte) []byte {
	if agentID == "" {
		return bodyBytes
	}

	canvasPath := fmt.Sprintf("%s/canvas/%s/latest.mmd", p.cfg.DataDir, agentID)
	mmd, err := os.ReadFile(canvasPath)
	if err != nil {
		return bodyBytes // No canvas yet — first turn
	}
	mmdStr := strings.TrimSpace(string(mmd))
	if mmdStr == "" {
		return bodyBytes
	}

	var reqBody map[string]interface{}
	if err := json.Unmarshal(bodyBytes, &reqBody); err != nil {
		return bodyBytes
	}

	messages, ok := reqBody["messages"].([]interface{})
	if !ok {
		return bodyBytes
	}

	// Determine which nodes are still running for the status summary
	var runningNodes, doneNodes int
	for _, line := range strings.Split(mmdStr, "\n") {
		line = strings.TrimSpace(line)
		if strings.Contains(line, "⏳") {
			runningNodes++
		} else if strings.Contains(line, "✅") {
			doneNodes++
		}
	}

	// Build the canvas section to inject
	mermaidBlock := "```mermaid\n" + mmdStr + "\n```"
	canvasSection := fmt.Sprintf("\n### 任务画布\n\n%s\n\n**当前状态**: %d 个节点已完成, %d 个节点执行中\n**注意**: 已完成节点无需重复执行，关注当前执行中的节点。\n",
		mermaidBlock, doneNodes, runningNodes)

	// Find system message and append canvas
	modified := false
	for i, msg := range messages {
		m, ok := msg.(map[string]interface{})
		if !ok {
			continue
		}
		role, _ := m["role"].(string)
		if role == "system" {
			content, _ := m["content"].(string)
			m["content"] = content + "\n" + canvasSection
			messages[i] = m
			modified = true
			break
		}
	}

	if !modified {
		// No system message — prepend one with the canvas
		canvasMsg := map[string]interface{}{
			"role":    "system",
			"content": canvasSection,
		}
		reqBody["messages"] = append([]interface{}{canvasMsg}, messages...)
	}

	newBody, err := json.Marshal(reqBody)
	if err != nil {
		return bodyBytes
	}
	slog.Debug("canvas injected into request", "agent", agentID)
	return newBody
}

// extractAgentAndInjectCanvas parses the request body, extracts agentID,
// and returns (agentID, possibly-modified-body).
func (p *MemoryProxy) extractAgentAndInjectCanvas(bodyBytes []byte) (string, []byte) {
	var reqBody map[string]interface{}
	if err := json.Unmarshal(bodyBytes, &reqBody); err != nil {
		return "", bodyBytes
	}
	agentID, _ := reqBody["agentId"].(string)

	modifiedBody := p.injectCanvas(agentID, bodyBytes)
	return agentID, modifiedBody
}
