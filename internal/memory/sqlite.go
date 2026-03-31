package memory

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/go-ego/gse"
)

// SQLiteStore 使用 SQLite + FTS5 实现记忆存储
type SQLiteStore struct {
	db  *sql.DB
	seg gse.Segmenter // 中文分词器
}

func NewSQLiteStore(path string) (*SQLiteStore, error) {
	// DSN 参数：WAL 模式提高并发写入性能
	//读多写少，写入的时候写入wal，定期再合并到主文件
	dsn := fmt.Sprintf("file:%s?_journal_mode=WAL&_busy_timeout=5000", path)
	db, err := sql.Open("sqlite", dsn)

	if err != nil {
		return nil, err
	}

	var seg gse.Segmenter
	seg.LoadDict() // 加载内置词典
	s := &SQLiteStore{db: db, seg: seg}
	if err := s.migrate(); err != nil {
		db.Close()
		return nil, err
	}
	return s, nil
}

// tokenize 对文本分词，返回空格分隔的词序列
// "我在学Go并发编程" → "我 在 学 Go 并发 编程"
func (s *SQLiteStore) tokenize(text string) string {
	segments := s.seg.Slice(text, true)
	return strings.Join(segments, " ")
}

// migrate 创建数据库表结构
// 先建立主表，再建立索引（sqlite是B树），最后再建立fst5虚表
func (s *SQLiteStore) migrate() error {
	_, err := s.db.Exec(`
        -- 主表：存储记忆元数据（原始内容）
        CREATE TABLE IF NOT EXISTS memories (
            id         INTEGER PRIMARY KEY AUTOINCREMENT,
            agent_id   TEXT NOT NULL,
            session_id TEXT,
            content    TEXT NOT NULL,
            tags       TEXT,          -- JSON 数组，标注
            source     TEXT NOT NULL DEFAULT 'manual',
            created_at DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP,
            updated_at DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP
        );

        -- 索引：按 agent_id 查询
        CREATE INDEX IF NOT EXISTS idx_memories_agent ON memories(agent_id);

        -- FTS5 虚拟表：存分词后内容，用于全文检索（BM25 排序）
        -- 不使用 content=memories，由 Go 手动写入分词结果
        CREATE VIRTUAL TABLE IF NOT EXISTS memories_fts USING fts5(
            content,
            tags
        );
    `)
	return err
}

func (s *SQLiteStore) Save(e *Entry) error {
	tags, _ := json.Marshal(e.Tags) //sqlite只能存字符串，要将切片转换
	now := time.Now()

	//1.写主表（原始内容）
	result, err := s.db.Exec(`
        INSERT INTO memories (agent_id, session_id, content, tags, source, created_at, updated_at)
        VALUES (?, ?, ?, ?, ?, ?, ?)`, e.AgentID, e.SessionID, e.Content, string(tags), e.Source, now, now)
	if err != nil {
		return err
	}

	//回传表中的id和时间
	id, _ := result.LastInsertId()
	e.ID = id
	e.CreatedAt = now
	e.UpdatedAt = now

	//2.写FTS5（分词后内容）
	//先token化
	tokenizedContent := s.tokenize(e.Content)
	//先把数组合成成string
	tokenizedTags := s.tokenize(strings.Join(e.Tags, " "))
	_, err = s.db.Exec(`
        INSERT INTO memories_fts(rowid,content, tags)
        VALUES (?, ?, ?)`, id, tokenizedContent, tokenizedTags)
	return err
}

func (s *SQLiteStore) Search(q SearchQuery) ([]*Entry, error) {
	limit := q.Limit
	if limit <= 0 {
		limit = 5
	}

	//查询词先分词
	tokenizedQuery := s.tokenize(q.Query)

	sqlQuery := `
        SELECT
            m.id, m.agent_id, m.session_id, m.content, m.tags, m.source,
            m.created_at, m.updated_at,
            -bm25(memories_fts, 10, 1) AS bm25_score
        FROM memories m
        JOIN memories_fts ON memories_fts.rowid = m.id
        WHERE memories_fts MATCH ?
          AND m.agent_id = ?
    `

	//匹配算法是词级别的匹配，不是句子级别，所以说宽松条件的时候就直接前缀匹配就行，因为全部都被tokenize了
	//让搜索条件更加宽松，比如“学习”可以匹配到“学习资料”
	args := []any{fts5Query(tokenizedQuery), q.AgentID}

	//等于0是没有限制
	if q.MaxAgeDays > 0 {
		sqlQuery += " AND m.created_at > datetime('now',?)"
		args = append(args, fmt.Sprintf("-%d days", q.MaxAgeDays))
	}

	sqlQuery += " ORDER BY bm25_score DESC LIMIT ?"
	args = append(args, limit*3)

	//rows是一个流，一行一行的数据
	rows, err := s.db.Query(sqlQuery, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var entries []*Entry
	for rows.Next() {
		e := &Entry{}
		var tagsJson string
		var bm25Score float64
		//方便格式转化
		err = rows.Scan(&e.ID, &e.AgentID, &e.SessionID, &e.Content, &tagsJson, &e.Source, &e.CreatedAt, &e.UpdatedAt, &bm25Score)
		if err != nil {
			continue
		}
		json.Unmarshal([]byte(tagsJson), &e.Tags)
		e.Score = bm25Score
		entries = append(entries, e)
	}

	// 时间衰减重排序
	entries = applyTimeDecay(entries)

	// 截取最终数量
	if len(entries) > limit {
		entries = entries[:limit]
	}
	return entries, nil
}

// fts5Query 将分词后的查询文本转为 FTS5 查询语法
// 对每个词加前缀匹配（*），允许部分匹配
func fts5Query(tokenized string) string {
	//按空格分割，自动去除多余空格
	words := strings.Fields(tokenized)
	for i, word := range words {
		words[i] = word + "*"
	}
	return strings.Join(words, " OR ")
}

func (s *SQLiteStore) Delete(id int64) error {
	s.db.Exec(`DELETE FROM memories_fts WHERE rowid=?`, id)
	_, err := s.db.Exec(`DELETE FROM memories WHERE id=?`, id)
	return err
}

// 防止一次性加载几千条消息
func (s *SQLiteStore) List(agentID string, limit, offset int) ([]*Entry, error) {
	rows, err := s.db.Query(`
        SELECT id, agent_id, session_id, content, tags, source, created_at, updated_at
        FROM memories WHERE agent_id = ?
        ORDER BY created_at DESC LIMIT ? OFFSET ?
    `, agentID, limit, offset)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var entries []*Entry
	for rows.Next() {
		e := &Entry{}
		var tagsJSON string
		rows.Scan(&e.ID, &e.AgentID, &e.SessionID, &e.Content, &tagsJSON,
			&e.Source, &e.CreatedAt, &e.UpdatedAt)
		json.Unmarshal([]byte(tagsJSON), &e.Tags)
		entries = append(entries, e)
	}
	return entries, nil
}

func (s *SQLiteStore) Count(agentID string) (int64, error) {
	var count int64
	err := s.db.QueryRow("SELECT COUNT(*) FROM memories WHERE agent_id = ?", agentID).Scan(&count)
	return count, err
}

func (s *SQLiteStore) Close() error { return s.db.Close() }
