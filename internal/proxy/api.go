package proxy

import (
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

// ── ID 生成 ──────────────────────────────────────────

func randID(prefix string, n int) string {
	b := make([]byte, n)
	rand.Read(b)
	return prefix + "_" + hex.EncodeToString(b)
}

// ── 工具结果 (Refs) ──────────────────────────────────

// POST /api/refs
func (p *MemoryProxy) handleCreateRef(w http.ResponseWriter, r *http.Request) {
	var req struct {
		AgentID   string   `json:"agent_id"`
		SessionID string   `json:"session_id"`
		ToolName  string   `json:"tool_name"`
		NodeID    string   `json:"node_id,omitempty"`
		Content   string   `json:"content"`
		Summary   string   `json:"summary,omitempty"`
		Tags      []string `json:"tags,omitempty"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, `{"error":"invalid JSON"}`, http.StatusBadRequest)
		return
	}
	if req.Content == "" {
		http.Error(w, `{"error":"content is required"}`, http.StatusBadRequest)
		return
	}

	refID := randID("ref", 8)
	size := len(req.Content)
	now := time.Now().UTC()

	// 写入文件
	dir := filepath.Join(p.cfg.DataDir, "refs", req.AgentID)
	os.MkdirAll(dir, 0755)
	filename := fmt.Sprintf("%s-%s.md", now.Format("20060102-150405"), refID)
	filePath := filepath.Join(dir, filename)

	md := fmt.Sprintf("# %s 工具结果\n\n> 时间: %s\n> 工具: %s\n> 摘要: %s\n\n```\n%s\n```\n",
		req.ToolName, now.Format("2006-01-02 15:04:05"), req.ToolName, req.Summary, req.Content)
	os.WriteFile(filePath, []byte(md), 0644)

	// 写入 SQLite（可选，保留索引）
	if p.memory != nil {
		tagsJSON, _ := json.Marshal(req.Tags)
		p.memory.Exec(`
			INSERT OR IGNORE INTO refs (ref_id, agent_id, session_id, tool_name, node_id, content, summary, tags, size, created_at)
			VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
			refID, req.AgentID, req.SessionID, req.ToolName, req.NodeID,
			req.Content, req.Summary, string(tagsJSON), size, now,
		)
	}

	resp := map[string]interface{}{
		"ref_id":     refID,
		"path":       fmt.Sprintf("refs/%s/%s", req.AgentID, filename),
		"size":       size,
		"created_at": now.Format(time.RFC3339),
	}
	writeJSON(w, http.StatusOK, resp)
}

// GET /api/refs/{ref_id}
func (p *MemoryProxy) handleGetRef(w http.ResponseWriter, r *http.Request) {
	refID := r.PathValue("ref_id")
	if refID == "" {
		http.Error(w, `{"error":"ref_id required"}`, http.StatusBadRequest)
		return
	}

	// 先从 SQLite 查找
	if p.memory != nil {
		var content, summary, toolName, createdAt string
		err := p.memory.QueryRow(
			`SELECT content, summary, tool_name, created_at FROM refs WHERE ref_id = ?`, refID,
		).Scan(&content, &summary, &toolName, &createdAt)
		if err == nil {
			writeJSON(w, http.StatusOK, map[string]interface{}{
				"ref_id":     refID,
				"content":    content,
				"summary":    summary,
				"tool_name":  toolName,
				"created_at": createdAt,
			})
			return
		}
	}

	// 从文件系统查找
	refsDir := filepath.Join(p.cfg.DataDir, "refs")
	found := false
	filepath.Walk(refsDir, func(path string, info os.FileInfo, err error) error {
		if err != nil || info.IsDir() {
			return nil
		}
		if strings.Contains(info.Name(), refID) {
			data, _ := os.ReadFile(path)
			writeJSON(w, http.StatusOK, map[string]interface{}{
				"ref_id":    refID,
				"content":   string(data),
				"source":    "file",
				"file_path": path,
			})
			found = true
			return fmt.Errorf("found") // 终止 Walk
		}
		return nil
	})

	if !found {
		http.Error(w, `{"error":"not found"}`, http.StatusNotFound)
	}
}

// GET /api/refs
func (p *MemoryProxy) handleListRefs(w http.ResponseWriter, r *http.Request) {
	agentID := r.URL.Query().Get("agent_id")
	toolName := r.URL.Query().Get("tool_name")
	limit, _ := strconv.Atoi(r.URL.Query().Get("limit"))
	if limit <= 0 || limit > 100 {
		limit = 20
	}

	if p.memory != nil {
		query := "SELECT ref_id, tool_name, summary, size, created_at FROM refs WHERE 1=1"
		args := []interface{}{}
		if agentID != "" {
			query += " AND agent_id = ?"
			args = append(args, agentID)
		}
		if toolName != "" {
			query += " AND tool_name = ?"
			args = append(args, toolName)
		}
		query += " ORDER BY created_at DESC LIMIT ?"
		args = append(args, limit)

		rows, err := p.memory.Query(query, args...)
		if err == nil {
			defer rows.Close()
			refs := []map[string]interface{}{}
			for rows.Next() {
				var refID, toolName, summary, createdAt string
				var size int
				if err := rows.Scan(&refID, &toolName, &summary, &size, &createdAt); err != nil {
					continue
				}
				refs = append(refs, map[string]interface{}{
					"ref_id":     refID,
					"tool_name":  toolName,
					"summary":    summary,
					"size":       size,
					"created_at": createdAt,
				})
			}
			writeJSON(w, http.StatusOK, map[string]interface{}{
				"refs":  refs,
				"total": len(refs),
			})
			return
		}
	}

	// 回退：从文件系统列出
	refsDir := filepath.Join(p.cfg.DataDir, "refs", agentID)
	entries, err := os.ReadDir(refsDir)
	if err != nil {
		writeJSON(w, http.StatusOK, map[string]interface{}{"refs": []interface{}{}, "total": 0})
		return
	}
	refs := []map[string]interface{}{}
	for _, e := range entries {
		if !e.IsDir() {
			info, _ := e.Info()
			refs = append(refs, map[string]interface{}{
				"ref_id":     e.Name(),
				"file_name":  e.Name(),
				"size":       info.Size(),
				"created_at": info.ModTime().Format(time.RFC3339),
			})
		}
	}
	writeJSON(w, http.StatusOK, map[string]interface{}{"refs": refs, "total": len(refs)})
}

// ── 记忆存储 ─────────────────────────────────────────

// POST /api/memory
func (p *MemoryProxy) handleCreateMemory(w http.ResponseWriter, r *http.Request) {
	var req struct {
		AgentID  string   `json:"agent_id"`
		UserID   string   `json:"user_id"`
		Content  string   `json:"content"`
		Type     string   `json:"type"`
		Source   string   `json:"source"`
		Tags     []string `json:"tags"`
		Priority int      `json:"priority"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, `{"error":"invalid JSON"}`, http.StatusBadRequest)
		return
	}
	if req.Content == "" {
		http.Error(w, `{"error":"content is required"}`, http.StatusBadRequest)
		return
	}
	if req.Type == "" {
		req.Type = "fact"
	}
	if req.Priority <= 0 {
		req.Priority = 1
	}

	memID := randID("mem", 8)
	now := time.Now()
	metadata, _ := json.Marshal(map[string]interface{}{
		"tags":    req.Tags,
		"source":  req.Source,
		"user_id": req.UserID,
	})

	err := p.memory.Exec(`
		INSERT INTO memories (id, agent_id, session_id, type, content, priority, created_at, updated_at, metadata)
		VALUES (?, ?, '', ?, ?, ?, ?, ?, ?)`,
		memID, req.AgentID, req.Type, req.Content, req.Priority, now, now, string(metadata),
	)
	if err != nil {
		slog.Error("save memory", "error", err)
		http.Error(w, fmt.Sprintf(`{"error":"%s"}`, err.Error()), http.StatusInternalServerError)
		return
	}

	// 写入可读的 Markdown 备份
	personaDir := filepath.Join(p.cfg.DataDir, "personas")
	os.MkdirAll(personaDir, 0755)
	memLog := filepath.Join(personaDir, "memory-log.md")
	f, _ := os.OpenFile(memLog, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0644)
	if f != nil {
		fmt.Fprintf(f, "- [%s] %s | %s\n", now.Format("2006-01-02 15:04"), req.Type, req.Content)
		f.Close()
	}

	writeJSON(w, http.StatusOK, map[string]interface{}{
		"memory_id":  memID,
		"created_at": now.Format(time.RFC3339),
	})
}

// GET /api/memory/search?q=xxx&limit=5
func (p *MemoryProxy) handleSearchMemory(w http.ResponseWriter, r *http.Request) {
	query := r.URL.Query().Get("q")
	agentID := r.URL.Query().Get("agent_id")
	limit, _ := strconv.Atoi(r.URL.Query().Get("limit"))
	if limit <= 0 || limit > 50 {
		limit = 10
	}
	threshold, _ := strconv.ParseFloat(r.URL.Query().Get("threshold"), 64)
	if threshold <= 0 {
		threshold = 0.3
	}

	if query == "" {
		http.Error(w, `{"error":"q (query) is required"}`, http.StatusBadRequest)
		return
	}

	like := "%" + query + "%"
	sqlQuery := `
		SELECT id, type, content, priority, created_at, metadata
		FROM memories
		WHERE (content LIKE ? OR metadata LIKE ?)`
	args := []interface{}{like, like}

	if agentID != "" {
		sqlQuery += " AND agent_id = ?"
		args = append(args, agentID)
	}
	sqlQuery += " ORDER BY priority DESC, created_at DESC LIMIT ?"
	args = append(args, limit)

	rows, err := p.memory.Query(sqlQuery, args...)
	if err != nil {
		slog.Error("search memory", "error", err)
		http.Error(w, `{"error":"search failed"}`, http.StatusInternalServerError)
		return
	}
	defer rows.Close()

	results := []map[string]interface{}{}
	for rows.Next() {
		var id, memType, content, createdAt string
		var priority int
		var metadata sql.NullString
		if err := rows.Scan(&id, &memType, &content, &priority, &createdAt, &metadata); err != nil {
			continue
		}
		tags := []string{}
		if metadata.Valid {
			var metaMap map[string]interface{}
			if err := json.Unmarshal([]byte(metadata.String), &metaMap); err == nil {
				if t, ok := metaMap["tags"].([]interface{}); ok {
					for _, tag := range t {
						if s, ok := tag.(string); ok {
							tags = append(tags, s)
						}
					}
				}
			}
		}
		results = append(results, map[string]interface{}{
			"memory_id":  id,
			"content":    content,
			"type":       memType,
			"tags":       tags,
			"score":      0.8,
			"created_at": createdAt,
		})
	}

	writeJSON(w, http.StatusOK, map[string]interface{}{
		"results":  results,
		"strategy": "keyword_like",
		"total":    len(results),
	})
}

// DELETE /api/memory/{memory_id}
func (p *MemoryProxy) handleDeleteMemory(w http.ResponseWriter, r *http.Request) {
	memID := r.PathValue("memory_id")
	if memID == "" {
		http.Error(w, `{"error":"memory_id required"}`, http.StatusBadRequest)
		return
	}
	err := p.memory.Exec(`DELETE FROM memories WHERE id = ?`, memID)
	if err != nil {
		http.Error(w, fmt.Sprintf(`{"error":"%s"}`, err.Error()), http.StatusInternalServerError)
		return
	}
	writeJSON(w, http.StatusOK, map[string]interface{}{"status": "deleted", "memory_id": memID})
}

// ── 用户画像 ─────────────────────────────────────────

// GET /api/persona/{user_id}
func (p *MemoryProxy) handleGetPersona(w http.ResponseWriter, r *http.Request) {
	userID := r.PathValue("user_id")
	if userID == "" {
		userID = "shyky"
	}

	// 从 SQLite 查找
	var personaStr, updatedAt string
	err := p.memory.QueryRow(
		`SELECT persona, updated_at FROM personas WHERE user_id = ?`, userID,
	).Scan(&personaStr, &updatedAt)

	if err == nil {
		var personaData interface{}
		json.Unmarshal([]byte(personaStr), &personaData)
		writeJSON(w, http.StatusOK, map[string]interface{}{
			"user_id":    userID,
			"persona":    personaData,
			"source":     "db",
			"updated_at": updatedAt,
		})
		return
	}

	// 从 Markdown 文件读取
	personaPath := filepath.Join(p.cfg.DataDir, "personas", fmt.Sprintf("%s.md", userID))
	if data, err := os.ReadFile(personaPath); err == nil {
		writeJSON(w, http.StatusOK, map[string]interface{}{
			"user_id": userID,
			"persona": string(data),
			"source":  "file",
		})
		return
	}

	writeJSON(w, http.StatusOK, map[string]interface{}{
		"user_id": userID,
		"persona": nil,
		"source":  "none",
	})
}

// PUT /api/persona/{user_id}
func (p *MemoryProxy) handleUpdatePersona(w http.ResponseWriter, r *http.Request) {
	userID := r.PathValue("user_id")
	if userID == "" {
		userID = "shyky"
	}

	var req struct {
		Persona interface{} `json:"persona"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, `{"error":"invalid JSON"}`, http.StatusBadRequest)
		return
	}

	personaJSON, _ := json.Marshal(req.Persona)
	now := time.Now()

	err := p.memory.Exec(`
		INSERT INTO personas (user_id, persona, version, updated_at, created_at)
		VALUES (?, ?, 1, ?, ?)
		ON CONFLICT(user_id) DO UPDATE SET persona = ?, version = version + 1, updated_at = ?`,
		userID, string(personaJSON), now, now, string(personaJSON), now,
	)
	if err != nil {
		http.Error(w, fmt.Sprintf(`{"error":"%s"}`, err.Error()), http.StatusInternalServerError)
		return
	}

	// 同时写入可读 Markdown
	dir := filepath.Join(p.cfg.DataDir, "personas")
	os.MkdirAll(dir, 0755)
	mdPath := filepath.Join(dir, fmt.Sprintf("%s.md", userID))
	mdContent := fmt.Sprintf("# 用户画像: %s\n\n> 更新于: %s\n\n```json\n%s\n```\n",
		userID, now.Format("2006-01-02 15:04:05"), string(personaJSON))
	os.WriteFile(mdPath, []byte(mdContent), 0644)

	writeJSON(w, http.StatusOK, map[string]interface{}{
		"status":     "updated",
		"user_id":    userID,
		"updated_at": now.Format(time.RFC3339),
	})
}

// ── 状态 ─────────────────────────────────────────────

// GET /api/status
func (p *MemoryProxy) handleStatus(w http.ResponseWriter, r *http.Request) {
	// 统计 counts
	var refCount, memCount int
	p.memory.QueryRow(`SELECT COUNT(*) FROM refs`).Scan(&refCount)
	p.memory.QueryRow(`SELECT COUNT(*) FROM memories`).Scan(&memCount)

	writeJSON(w, http.StatusOK, map[string]interface{}{
		"status":  "ok",
		"uptime":  time.Since(startTime).String(),
		"version": "0.2.0",
		"stats": map[string]interface{}{
			"refs_count":     refCount,
			"memory_count":   memCount,
			"active_sessions": p.toolCache.Len(),
		},
		"config": map[string]interface{}{
			"listen": p.cfg.Listen,
			"target": p.cfg.Target,
			"agents": p.cfg.Agents,
		},
	})
}

// ── 工具 ─────────────────────────────────────────────

func writeJSON(w http.ResponseWriter, status int, data interface{}) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(data)
}

// 全局启动时间
var startTime = time.Now()
