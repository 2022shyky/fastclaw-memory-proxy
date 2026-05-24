package canvas

import (
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"strings"
	"sync"
	"time"
)

type Node struct {
	ID       string `json:"id"`
	Label    string `json:"label"`
	ToolName string `json:"tool_name"`
	Status   string `json:"status"`
	ParentID string `json:"parent_id,omitempty"`
}

type Session struct {
	Nodes     []Node `json:"nodes"`
	UpdatedAt string `json:"updated_at"`
}

type Engine struct {
	mu       sync.RWMutex
	dataDir  string
	sessions map[string]*Session
}

func New(dataDir string) *Engine {
	e := &Engine{
		dataDir:  dataDir,
		sessions: make(map[string]*Session),
	}
	os.MkdirAll(dataDir+"/canvas", 0755)
	return e
}

func (e *Engine) key(agentID, sessionID string) string {
	return agentID + "/" + sessionID
}

func (e *Engine) getSession(agentID, sessionID string) *Session {
	e.mu.Lock()
	defer e.mu.Unlock()

	key := e.key(agentID, sessionID)
	sess, ok := e.sessions[key]
	if !ok {
		sess = &Session{Nodes: []Node{}}
		e.sessions[key] = sess
	}
	sess.UpdatedAt = time.Now().Format(time.RFC3339)
	return sess
}

func (e *Engine) AddNode(agentID, sessionID string, event map[string]interface{}) {
	sess := e.getSession(agentID, sessionID)

	data, _ := event["data"].(map[string]interface{})
	toolName, _ := data["name"].(string)
	if toolName == "" {
		return
	}

	node := Node{
		ID:       fmt.Sprintf("N%d", len(sess.Nodes)+1),
		Label:    toolName,
		ToolName: toolName,
		Status:   "running",
	}

	if len(sess.Nodes) > 0 {
		node.ParentID = sess.Nodes[len(sess.Nodes)-1].ID
	}

	sess.Nodes = append(sess.Nodes, node)
	slog.Debug("canvas: added node", "agent", agentID, "node", node.ID, "tool", toolName)
}

func (e *Engine) MarkDone(agentID, sessionID, toolName string) {
	e.mu.Lock()
	defer e.mu.Unlock()

	key := e.key(agentID, sessionID)
	sess, ok := e.sessions[key]
	if !ok {
		return
	}

	// Mark ALL matching running nodes as done (same tool may be called multiple times)
	for i := range sess.Nodes {
		if sess.Nodes[i].ToolName == toolName && sess.Nodes[i].Status == "running" {
			sess.Nodes[i].Status = "done"
		}
	}
}

func (e *Engine) GenerateMermaid(agentID, sessionID string) string {
	e.mu.RLock()
	defer e.mu.RUnlock()

	key := e.key(agentID, sessionID)
	sess, ok := e.sessions[key]
	if !ok || len(sess.Nodes) == 0 {
		return ""
	}

	var sb strings.Builder
	sb.WriteString("graph TD\n")
	sb.WriteString(fmt.Sprintf("    subgraph \"会话: %s\"\n", sessionID))

	for _, node := range sess.Nodes {
		status := ""
		switch node.Status {
		case "running":
			status = " ⏳"
		case "done":
			status = " ✅"
		case "failed":
			status = " ❌"
		}
		sb.WriteString(fmt.Sprintf("        %s[\"%s%s\"]\n", node.ID, node.Label, status))
	}

	sb.WriteString("    end\n")

	for _, node := range sess.Nodes {
		if node.ParentID != "" {
			sb.WriteString(fmt.Sprintf("    %s --> %s\n", node.ParentID, node.ID))
		}
	}

	for _, node := range sess.Nodes {
		switch node.Status {
		case "done":
			sb.WriteString(fmt.Sprintf("    style %s fill:#90EE90\n", node.ID))
		case "failed":
			sb.WriteString(fmt.Sprintf("    style %s fill:#FFB6C1\n", node.ID))
		case "running":
			sb.WriteString(fmt.Sprintf("    style %s fill:#FFD700\n", node.ID))
		}
	}

	return sb.String()
}

func (e *Engine) Finalize(agentID, sessionID string) {
	e.mu.Lock()
	defer e.mu.Unlock()

	key := e.key(agentID, sessionID)
	sess, ok := e.sessions[key]
	if !ok {
		return
	}

	mmd := e.GenerateMermaid(agentID, sessionID)
	dir := fmt.Sprintf("%s/canvas/%s", e.dataDir, agentID)
	os.MkdirAll(dir, 0755)

	mmdPath := fmt.Sprintf("%s/%s.mmd", dir, sessionID)
	os.WriteFile(mmdPath, []byte(mmd), 0644)
	// Also write latest.mmd so the request handler can easily find it
	os.WriteFile(fmt.Sprintf("%s/latest.mmd", dir), []byte(mmd), 0644)

	jsonPath := fmt.Sprintf("%s/%s.json", dir, sessionID)
	data, _ := json.MarshalIndent(sess, "", "  ")
	os.WriteFile(jsonPath, data, 0644)
	// Also write latest.json
	os.WriteFile(fmt.Sprintf("%s/latest.json", dir), data, 0644)

	slog.Info("canvas saved", "agent", agentID, "session", sessionID, "nodes", len(sess.Nodes))
}