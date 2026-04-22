package structured

import (
	"database/sql"
	"time"
)

type Transaction struct {
	ID         int64
	AgentID    string
	Amount     float64 // 正数=收入，负数=支出
	Category   string
	Note       string
	HappenedAt time.Time
	CreatedAt  time.Time
}

type MonthlySummary struct {
	Month      string
	Total      float64
	ByCategory map[string]float64
}

type LedgerStore struct{ db *sql.DB }

func NewLedgerStore(db *sql.DB) (*LedgerStore, error) {
	s := &LedgerStore{db: db}
	return s, s.migrate()
}

func (s *LedgerStore) migrate() error {
	_, err := s.db.Exec(`
		CREATE TABLE IF NOT EXISTS transactions (
			id           INTEGER PRIMARY KEY AUTOINCREMENT,
			agent_id     TEXT NOT NULL,
			amount       REAL NOT NULL,
			category     TEXT NOT NULL DEFAULT 'other',
			note         TEXT,
			happened_at  DATETIME NOT NULL,
			created_at   DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP
		);
		CREATE INDEX IF NOT EXISTS idx_tx_agent_time ON transactions(agent_id, happened_at);
	`)
	return err
}

func (s *LedgerStore) Save(t *Transaction) error {
	now := time.Now()
	result, err := s.db.Exec(`
		INSERT INTO transactions (agent_id, amount, category, note, happened_at, created_at)
		VALUES (?, ?, ?, ?, ?, ?)
	`, t.AgentID, t.Amount, t.Category, t.Note, t.HappenedAt, now)
	if err != nil {
		return err
	}
	t.ID, _ = result.LastInsertId()
	t.CreatedAt = now
	return nil
}

func (s *LedgerStore) MonthlySummary(agentID, month string) (*MonthlySummary, error) {
	rows, err := s.db.Query(`
		SELECT category, SUM(amount)
		FROM transactions
		WHERE agent_id = ? AND strftime('%Y-%m', happened_at) = ?
		GROUP BY category
	`, agentID, month)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	summary := &MonthlySummary{
		Month:      month,
		ByCategory: make(map[string]float64),
	}
	for rows.Next() {
		var cat string
		var sum float64
		rows.Scan(&cat, &sum)
		summary.ByCategory[cat] = sum
		summary.Total += sum
	}
	return summary, nil
}
