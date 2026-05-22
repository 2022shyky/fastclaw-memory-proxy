package stats

import (
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"time"

	"github.com/shyky/memory-proxy/internal/config"
)

type SessionStats struct {
	AgentID                string    `json:"agent_id"`
	SessionID              string    `json:"session_id"`
	StartedAt              time.Time `json:"started_at"`
	EndedAt                time.Time `json:"ended_at"`

	TotalPromptTokens      int `json:"total_prompt_tokens"`
	TotalCompletionTokens  int `json:"total_completion_tokens"`

	ToolResultsIntercepted int `json:"tool_results_intercepted"`
	TotalCharsOffloaded    int `json:"total_chars_offloaded"`
	EstimatedTokensSaved   int `json:"estimated_tokens_saved"`

	RefReadsCount int `json:"ref_reads_count"`
	RefReadsChars int `json:"ref_reads_chars"`

	CanvasNodes   int `json:"canvas_nodes"`
	CanvasUpdates int `json:"canvas_updates"`
}

type Collector struct {
	cfg      *config.Config
	sessions map[string]*SessionStats
}

func New(cfg *config.Config) *Collector {
	return &Collector{
		cfg:      cfg,
		sessions: make(map[string]*SessionStats),
	}
}

func (c *Collector) NewSession(agentID, sessionID string) *SessionStats {
	sess := &SessionStats{
		AgentID:   agentID,
		SessionID: sessionID,
		StartedAt: time.Now(),
	}
	c.sessions[sessionID] = sess
	return sess
}

func (c *Collector) FinalizeSession(sess *SessionStats) {
	sess.EndedAt = time.Now()

	if !c.cfg.Stats.Enabled {
		return
	}

	dir := fmt.Sprintf("%s/%s", c.cfg.Stats.Output, sess.AgentID)
	os.MkdirAll(dir, 0755)

	date := sess.StartedAt.Format("2006-01-02")
	filePath := fmt.Sprintf("%s/%s.json", dir, date)

	data, err := json.Marshal(sess)
	if err != nil {
		slog.Error("marshal stats", "error", err)
		return
	}

	f, err := os.OpenFile(filePath, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0644)
	if err != nil {
		slog.Error("write stats", "error", err)
		return
	}
	defer f.Close()
	f.Write(data)
	f.Write([]byte("\n"))
}

func (c *Collector) Report(agentID string) string {
	var totalSessions int
	var totalSaved int
	var totalIntercepted int

	for _, sess := range c.sessions {
		if sess.AgentID == agentID {
			totalSessions++
			totalSaved += sess.EstimatedTokensSaved
			totalIntercepted += sess.ToolResultsIntercepted
		}
	}

	return fmt.Sprintf("📊 Memory Proxy 统计报告\n━━━━━━━━━━━━━━━━━━━━━━━━\nAgent: %s\n会话数: %d\n拦截工具结果: %d 次\n估算节省 Token: %d (~%.1f%%)\n",
		agentID, totalSessions, totalIntercepted, totalSaved,
		float64(totalSaved)/float64(totalSaved+totalIntercepted*100)*100)
}