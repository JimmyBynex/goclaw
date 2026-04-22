package structured

import (
	"database/sql"
	"time"
)

type Event struct {
	ID        int64
	AgentID   string
	Title     string
	Type      string // "recurring" | "task" | "one_time"
	StartAt   time.Time
	EndAt     *time.Time
	Location  string
	Note      string
	CreatedAt time.Time
}

type EventStore struct{ db *sql.DB }

func NewEventStore(db *sql.DB) (*EventStore, error) {
	s := &EventStore{db: db}
	return s, s.migrate()
}

func (s *EventStore) migrate() error {
	_, err := s.db.Exec(`
		CREATE TABLE IF NOT EXISTS events (
			id         INTEGER PRIMARY KEY AUTOINCREMENT,
			agent_id   TEXT NOT NULL,
			title      TEXT NOT NULL,
			type       TEXT NOT NULL DEFAULT 'one_time',
			start_at   DATETIME NOT NULL,
			end_at     DATETIME,
			location   TEXT,
			note       TEXT,
			created_at DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP
		);
		CREATE INDEX IF NOT EXISTS idx_events_agent_start ON events(agent_id, start_at);
	`)
	return err
}

func (s *EventStore) Save(e *Event) error {
	now := time.Now()
	result, err := s.db.Exec(`
		INSERT INTO events (agent_id, title, type, start_at, end_at, location, note, created_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?)
	`, e.AgentID, e.Title, e.Type, e.StartAt, e.EndAt, e.Location, e.Note, now)
	if err != nil {
		return err
	}
	e.ID, _ = result.LastInsertId()
	e.CreatedAt = now
	return nil
}

func (s *EventStore) ListByRange(agentID string, from, to time.Time) ([]*Event, error) {
	rows, err := s.db.Query(`
		SELECT id, agent_id, title, type, start_at, end_at, location, note, created_at
		FROM events
		WHERE agent_id = ? AND start_at >= ? AND start_at <= ?
		ORDER BY start_at ASC
	`, agentID, from, to)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var events []*Event
	for rows.Next() {
		e := &Event{}
		rows.Scan(&e.ID, &e.AgentID, &e.Title, &e.Type, &e.StartAt, &e.EndAt,
			&e.Location, &e.Note, &e.CreatedAt)
		events = append(events, e)
	}
	return events, nil
}

func (s *EventStore) Delete(id int64) error {
	_, err := s.db.Exec("DELETE FROM events WHERE id = ?", id)
	return err
}
