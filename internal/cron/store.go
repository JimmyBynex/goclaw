// internal/cron/store.go

package cron

import (
	"database/sql"
	"time"
)

type Store struct {
	db *sql.DB
}

func NewStore(db *sql.DB) (*Store, error) {
	s := &Store{db: db}
	return s, s.migrate()
}

func (s *Store) migrate() error {
	_, err := s.db.Exec(`
        CREATE TABLE IF NOT EXISTS cron_jobs (
            id          INTEGER PRIMARY KEY AUTOINCREMENT,
            agent_id    TEXT NOT NULL,
            channel_id  TEXT NOT NULL,
            account_id  TEXT NOT NULL,
            peer_id     TEXT NOT NULL,
            message     TEXT NOT NULL,
            schedule    TEXT NOT NULL,
            repeat      BOOLEAN NOT NULL DEFAULT 0,
            next_run_at DATETIME NOT NULL,
            created_at  DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP,
            done        BOOLEAN NOT NULL DEFAULT 0
        );
        CREATE INDEX IF NOT EXISTS idx_cron_next ON cron_jobs(next_run_at, done);
    `)
	return err
}

// Save 保存一个新 Job，填充 ID 和 CreatedAt
func (s *Store) Save(j *Job) error {
	now := time.Now()
	result, err := s.db.Exec(`
        INSERT INTO cron_jobs (agent_id, channel_id, account_id, peer_id, message, schedule, repeat, next_run_at, created_at)
        VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)
    `, j.AgentID, j.ChannelID, j.AccountID, j.PeerID, j.Message, j.Schedule, j.Repeat, j.NextRunAt, now)
	if err != nil {
		return err
	}
	j.ID, _ = result.LastInsertId()
	j.CreatedAt = now
	return nil
}

// Due 返回所有到期未完成的任务
func (s *Store) Due(now time.Time) ([]*Job, error) {
	rows, err := s.db.Query(`
        SELECT id, agent_id, channel_id, account_id, peer_id, message, schedule, repeat, next_run_at, created_at
        FROM cron_jobs
        WHERE next_run_at <= ? AND done = 0
    `, now)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var jobs []*Job
	for rows.Next() {
		j := &Job{}
		rows.Scan(&j.ID, &j.AgentID, &j.ChannelID, &j.AccountID, &j.PeerID,
			&j.Message, &j.Schedule, &j.Repeat, &j.NextRunAt, &j.CreatedAt)
		jobs = append(jobs, j)
	}
	return jobs, nil
}

// MarkDone 标记一次性任务完成
func (s *Store) MarkDone(id int64) error {
	_, err := s.db.Exec("UPDATE cron_jobs SET done = 1 WHERE id = ?", id)
	return err
}

// UpdateNextRun 更新重复任务的下次运行时间
func (s *Store) UpdateNextRun(id int64, next time.Time) error {
	_, err := s.db.Exec("UPDATE cron_jobs SET next_run_at = ? WHERE id = ?", next, id)
	return err
}

// Delete 删除任务
func (s *Store) Delete(id int64) error {
	_, err := s.db.Exec("DELETE FROM cron_jobs WHERE id = ?", id)
	return err
}

// List 列出某个 Agent 的所有未完成任务
func (s *Store) List(agentID string) ([]*Job, error) {
	rows, err := s.db.Query(`
        SELECT id, agent_id, channel_id, account_id, peer_id, message, schedule, repeat, next_run_at, created_at
        FROM cron_jobs WHERE agent_id = ? AND done = 0
        ORDER BY next_run_at ASC
    `, agentID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var jobs []*Job
	for rows.Next() {
		j := &Job{}
		rows.Scan(&j.ID, &j.AgentID, &j.ChannelID, &j.AccountID, &j.PeerID,
			&j.Message, &j.Schedule, &j.Repeat, &j.NextRunAt, &j.CreatedAt)
		jobs = append(jobs, j)
	}
	return jobs, nil
}
