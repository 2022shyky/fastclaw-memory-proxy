package memory

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"time"

	_ "modernc.org/sqlite"

	"github.com/shyky/memory-proxy/internal/stats"
)

type Store struct {
	db *sql.DB
}

func New(dataDir string) (*Store, error) {
	dbPath := dataDir + "/memory.db"
	os.MkdirAll(dataDir, 0755)

	db, err := sql.Open("sqlite", dbPath)
	if err != nil {
		return nil, fmt.Errorf("open sqlite: %w", err)
	}

	db.Exec("PRAGMA journal_mode=WAL")

	if err := migrate(db); err != nil {
		return nil, fmt.Errorf("migrate: %w", err)
	}

	slog.Info("memory store ready", "path", dbPath)
	return &Store{db: db}, nil
}

func migrate(db *sql.DB) error {
	queries := []string{
		`CREATE TABLE IF NOT EXISTS sessions (
			id TEXT PRIMARY KEY,
			agent_id TEXT NOT NULL,
			started_at DATETIME,
			ended_at DATETIME,
			summary TEXT,
			participant TEXT,
			metadata TEXT
		)`,
		`CREATE TABLE IF NOT EXISTS memories (
			id TEXT PRIMARY KEY,
			agent_id TEXT NOT NULL,
			session_id TEXT,
			type TEXT NOT NULL,
			content TEXT NOT NULL,
			priority INTEGER DEFAULT 0,
			source_message_ids TEXT,
			created_at DATETIME,
			updated_at DATETIME,
			metadata TEXT
		)`,
		`CREATE TABLE IF NOT EXISTS refs (
			ref_id TEXT PRIMARY KEY,
			agent_id TEXT NOT NULL DEFAULT '',
			session_id TEXT DEFAULT '',
			tool_name TEXT NOT NULL DEFAULT '',
			node_id TEXT DEFAULT '',
			content TEXT NOT NULL,
			summary TEXT DEFAULT '',
			tags TEXT DEFAULT '[]',
			size INTEGER DEFAULT 0,
			created_at DATETIME
		)`,
		`CREATE TABLE IF NOT EXISTS personas (
			user_id TEXT PRIMARY KEY,
			persona TEXT NOT NULL DEFAULT '{}',
			version INTEGER DEFAULT 1,
			created_at DATETIME,
			updated_at DATETIME
		)`,
		`CREATE INDEX IF NOT EXISTS idx_memories_agent ON memories(agent_id)`,
		`CREATE INDEX IF NOT EXISTS idx_memories_type ON memories(agent_id, type)`,
		`CREATE INDEX IF NOT EXISTS idx_sessions_agent ON sessions(agent_id)`,
		`CREATE INDEX IF NOT EXISTS idx_refs_agent ON refs(agent_id)`,
		`CREATE INDEX IF NOT EXISTS idx_refs_tool ON refs(tool_name)`,
	}

	for _, q := range queries {
		if _, err := db.Exec(q); err != nil {
			return fmt.Errorf("exec %q: %w", q, err)
		}
	}
	return nil
}

func (s *Store) SaveSessionSummary(agentID, sessionID string, sess *stats.SessionStats) error {
	now := time.Now()
	metadata, _ := json.Marshal(map[string]interface{}{
		"tools_intercepted": sess.ToolResultsIntercepted,
		"chars_offloaded":   sess.TotalCharsOffloaded,
		"tokens_saved":      sess.EstimatedTokensSaved,
	})

	_, err := s.db.Exec(
		`INSERT OR REPLACE INTO sessions (id, agent_id, started_at, ended_at, summary, metadata)
		 VALUES (?, ?, ?, ?, ?, ?)`,
		sessionID, agentID, sess.StartedAt, now,
		fmt.Sprintf("会话 %s: %d 次工具调用, 节省 %d token",
			sessionID, sess.ToolResultsIntercepted, sess.EstimatedTokensSaved),
		string(metadata),
	)
	return err
}

func (s *Store) SaveMemory(agentID, sessionID, memType, content string, priority int) error {
	id := fmt.Sprintf("mem-%d", time.Now().UnixNano())
	now := time.Now()

	_, err := s.db.Exec(
		`INSERT INTO memories (id, agent_id, session_id, type, content, priority, created_at, updated_at)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?)`,
		id, agentID, sessionID, memType, content, priority, now, now,
	)
	return err
}

func (s *Store) SearchMemories(agentID, participant string, limit int) ([]map[string]interface{}, error) {
	if limit <= 0 {
		limit = 10
	}

	rows, err := s.db.Query(
		`SELECT id, type, content, priority, created_at FROM memories
		 WHERE agent_id = ?
		 ORDER BY priority DESC, created_at DESC
		 LIMIT ?`,
		agentID, limit,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var results []map[string]interface{}
	for rows.Next() {
		var id, memType, content string
		var priority int
		var createdAt time.Time
		if err := rows.Scan(&id, &memType, &content, &priority, &createdAt); err != nil {
			continue
		}
		results = append(results, map[string]interface{}{
			"id":         id,
			"type":       memType,
			"content":    content,
			"priority":   priority,
			"created_at": createdAt.Format(time.RFC3339),
		})
	}
	return results, nil
}

func (s *Store) Close() error {
	return s.db.Close()
}

// Exec 执行 SQL 语句（供 REST API 使用）
func (s *Store) Exec(query string, args ...interface{}) error {
	_, err := s.db.Exec(query, args...)
	return err
}

// Query 执行 SQL 查询（供 REST API 使用）
func (s *Store) Query(query string, args ...interface{}) (*sql.Rows, error) {
	return s.db.Query(query, args...)
}

// QueryRow 执行单行查询（供 REST API 使用）
func (s *Store) QueryRow(query string, args ...interface{}) *sql.Row {
	return s.db.QueryRow(query, args...)
}