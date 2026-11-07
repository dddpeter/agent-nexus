package repo

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/pressly/goose/v3"
	_ "modernc.org/sqlite" // 纯 Go SQLite 驱动（免 CGO）

	"github.com/dddpeter/agent-nexus/internal/bus"
	"github.com/dddpeter/agent-nexus/migrations"
)

// sqliteStore 是全部聚合 repo 的 SQLite 实现（WAL + busy_timeout=5s，F6.2）。
type sqliteStore struct {
	db       *sql.DB
	agents   *sqliteAgentRepo
	sessions *sqliteSessionRepo
	messages *sqliteMessageRepo
	groups   *sqliteGroupRepo
	tasks    *sqliteTaskRepo
	events   *sqliteEventRepo
}

// OpenSQLite 打开（必要时创建）SQLite 数据库并执行 goose 迁移。
// dsn 形如 "data/nexus.db"；父目录自动创建；WAL/busy_timeout/foreign_keys 经 DSN pragma 启用。
func OpenSQLite(ctx context.Context, dsn string) (Store, error) {
	path := strings.TrimPrefix(dsn, "file:")
	if path != dsn || !strings.Contains(path, ":") { // 纯文件路径或 file: 路径
		if dir := filepath.Dir(path); dir != "" && dir != "." {
			if err := os.MkdirAll(dir, 0o755); err != nil {
				return nil, fmt.Errorf("repo: create data dir: %w", err)
			}
		}
	}
	// WAL 持久化于库文件；busy_timeout/foreign_keys 为 per-connection，必须在 DSN 指定。
	openDSN := "file:" + path + "?_pragma=busy_timeout(5000)&_pragma=journal_mode(WAL)&_pragma=foreign_keys(1)"
	db, err := sql.Open("sqlite", openDSN)
	if err != nil {
		return nil, fmt.Errorf("repo: open sqlite: %w", err)
	}
	if err := db.PingContext(ctx); err != nil {
		db.Close()
		return nil, fmt.Errorf("repo: ping sqlite (%s): %w", path, err)
	}

	s := &sqliteStore{db: db}
	if err := s.migrate(); err != nil {
		db.Close()
		return nil, err
	}
	s.agents = &sqliteAgentRepo{db}
	s.sessions = &sqliteSessionRepo{db}
	s.messages = &sqliteMessageRepo{db}
	s.groups = &sqliteGroupRepo{db}
	s.tasks = &sqliteTaskRepo{db}
	s.events = &sqliteEventRepo{db}
	return s, nil
}

func (s *sqliteStore) migrate() error {
	goose.SetBaseFS(migrations.FS)
	goose.SetLogger(goose.NopLogger())
	if err := goose.SetDialect("sqlite3"); err != nil {
		return fmt.Errorf("repo: goose dialect: %w", err)
	}
	if err := goose.Up(s.db, "."); err != nil {
		return fmt.Errorf("repo: goose up: %w", err)
	}
	return nil
}

func (s *sqliteStore) Agents() AgentRepo     { return s.agents }
func (s *sqliteStore) Sessions() SessionRepo { return s.sessions }
func (s *sqliteStore) Messages() MessageRepo { return s.messages }
func (s *sqliteStore) Groups() GroupRepo     { return s.groups }
func (s *sqliteStore) Tasks() TaskRepo       { return s.tasks }
func (s *sqliteStore) Events() EventRepo     { return s.events }
func (s *sqliteStore) Close() error          { return s.db.Close() }

// ---------- 时间 <-> 文本 ----------

const timeLayout = time.RFC3339Nano

func fmtTime(t time.Time) string { return t.Format(timeLayout) }
func fmtTimePtr(t *time.Time) sql.NullString {
	if t == nil {
		return sql.NullString{}
	}
	return sql.NullString{String: fmtTime(*t), Valid: true}
}
func parseTime(s string) (time.Time, error) { return time.Parse(timeLayout, s) }
func parseTimePtr(ns sql.NullString) (*time.Time, error) {
	if !ns.Valid || ns.String == "" {
		return nil, nil
	}
	t, err := parseTime(ns.String)
	if err != nil {
		return nil, fmt.Errorf("repo: parse datetime %q: %w", ns.String, err)
	}
	return &t, nil
}

// ---------- agents ----------

type sqliteAgentRepo struct{ db *sql.DB }

const agentCols = "id, name, adapter, config_json, capabilities_json, status, last_heartbeat_at, disabled"

func (r *sqliteAgentRepo) Create(ctx context.Context, a Agent) error {
	_, err := r.db.ExecContext(ctx,
		`INSERT INTO agents (id, name, adapter, config_json, capabilities_json, status, last_heartbeat_at, disabled)
		 VALUES (?,?,?,?,?,?,?,?)`,
		a.ID, a.Name, a.Adapter, a.ConfigJSON, a.CapabilitiesJSON, a.Status, fmtTimePtr(a.LastHeartbeatAt), boolInt(a.Disabled))
	if err != nil {
		if isUniqueViolation(err, "agents.name") {
			return fmt.Errorf("%w: %q", ErrDuplicateName, a.Name)
		}
		return fmt.Errorf("repo: insert agent: %w", err)
	}
	return nil
}

func (r *sqliteAgentRepo) scan(row interface{ Scan(...any) error }) (Agent, error) {
	var a Agent
	var hb sql.NullString
	var disabled int
	if err := row.Scan(&a.ID, &a.Name, &a.Adapter, &a.ConfigJSON, &a.CapabilitiesJSON, &a.Status, &hb, &disabled); err != nil {
		return Agent{}, err
	}
	a.Disabled = disabled != 0
	var err error
	if a.LastHeartbeatAt, err = parseTimePtr(hb); err != nil {
		return Agent{}, err
	}
	return a, nil
}

func (r *sqliteAgentRepo) Get(ctx context.Context, id string) (Agent, error) {
	a, err := r.scan(r.db.QueryRowContext(ctx, `SELECT `+agentCols+` FROM agents WHERE id = ?`, id))
	if errors.Is(err, sql.ErrNoRows) {
		return Agent{}, fmt.Errorf("%w: agent %s", ErrNotFound, id)
	}
	return a, err
}

func (r *sqliteAgentRepo) GetByName(ctx context.Context, name string) (Agent, error) {
	a, err := r.scan(r.db.QueryRowContext(ctx, `SELECT `+agentCols+` FROM agents WHERE name = ?`, name))
	if errors.Is(err, sql.ErrNoRows) {
		return Agent{}, fmt.Errorf("%w: agent name %q", ErrNotFound, name)
	}
	return a, err
}

func (r *sqliteAgentRepo) List(ctx context.Context, f AgentFilter) ([]Agent, error) {
	q := `SELECT ` + agentCols + ` FROM agents WHERE 1=1`
	var args []any
	if f.Status != "" {
		q += ` AND status = ?`
		args = append(args, f.Status)
	}
	if f.Adapter != "" {
		q += ` AND adapter = ?`
		args = append(args, f.Adapter)
	}
	if !f.IncludeRemoved {
		q += ` AND status != ?`
		args = append(args, StatusRemoved)
	}
	// agents 表无 created_at 列（严格按 DDL）；用 rowid 稳定排序。
	q += ` ORDER BY rowid DESC`

	rows, err := r.db.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, fmt.Errorf("repo: list agents: %w", err)
	}
	defer rows.Close()
	var out []Agent
	for rows.Next() {
		a, err := r.scan(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, a)
	}
	return out, rows.Err()
}

func (r *sqliteAgentRepo) Update(ctx context.Context, a Agent) error {
	res, err := r.db.ExecContext(ctx,
		`UPDATE agents SET adapter=?, config_json=?, capabilities_json=?, status=?, last_heartbeat_at=?, disabled=?
		 WHERE id=?`,
		a.Adapter, a.ConfigJSON, a.CapabilitiesJSON, a.Status, fmtTimePtr(a.LastHeartbeatAt), boolInt(a.Disabled), a.ID)
	if err != nil {
		return fmt.Errorf("repo: update agent: %w", err)
	}
	return requireUpdated(res, "agent", a.ID)
}

// boolInt SQLite 无原生布尔，DDL 用 INTEGER 0/1。
func boolInt(b bool) int {
	if b {
		return 1
	}
	return 0
}

func requireUpdated(res sql.Result, what, id string) error {
	n, err := res.RowsAffected()
	if err != nil {
		return fmt.Errorf("repo: rows affected: %w", err)
	}
	if n == 0 {
		return fmt.Errorf("%w: %s %s", ErrNotFound, what, id)
	}
	return nil
}

func isUniqueViolation(err error, _ string) bool {
	if err == nil {
		return false
	}
	msg := err.Error()
	return strings.Contains(msg, "UNIQUE constraint failed") || strings.Contains(msg, "constraint failed: UNIQUE")
}

// ---------- sessions ----------

type sqliteSessionRepo struct{ db *sql.DB }

const sessionCols = "id, agent_id, kind, external_ref, topic_id, created_at, closed_at"

func (r *sqliteSessionRepo) Create(ctx context.Context, s Session) error {
	_, err := r.db.ExecContext(ctx,
		`INSERT INTO sessions (id, agent_id, kind, external_ref, topic_id, created_at, closed_at)
		 VALUES (?,?,?,?,?,?,?)`,
		s.ID, nullStr(s.AgentID), s.Kind, nullStr(s.ExternalRef), nullStr(s.TopicID),
		fmtTime(s.CreatedAt), fmtTimePtr(s.ClosedAt))
	if err != nil {
		return fmt.Errorf("repo: insert session: %w", err)
	}
	return nil
}

func nullStr(p *string) sql.NullString {
	if p == nil {
		return sql.NullString{}
	}
	return sql.NullString{String: *p, Valid: true}
}

func strPtr(ns sql.NullString) *string {
	if !ns.Valid {
		return nil
	}
	v := ns.String
	return &v
}

func (r *sqliteSessionRepo) scan(row interface{ Scan(...any) error }) (Session, error) {
	var s Session
	var agentID, extRef, topicID, closedAt sql.NullString
	var createdAt string
	if err := row.Scan(&s.ID, &agentID, &s.Kind, &extRef, &topicID, &createdAt, &closedAt); err != nil {
		return Session{}, err
	}
	s.AgentID = strPtr(agentID)
	s.ExternalRef = strPtr(extRef)
	s.TopicID = strPtr(topicID)
	t, err := parseTime(createdAt)
	if err != nil {
		return Session{}, err
	}
	s.CreatedAt = t
	if s.ClosedAt, err = parseTimePtr(closedAt); err != nil {
		return Session{}, err
	}
	return s, nil
}

func (r *sqliteSessionRepo) Get(ctx context.Context, id string) (Session, error) {
	s, err := r.scan(r.db.QueryRowContext(ctx, `SELECT `+sessionCols+` FROM sessions WHERE id = ?`, id))
	if errors.Is(err, sql.ErrNoRows) {
		return Session{}, fmt.Errorf("%w: session %s", ErrNotFound, id)
	}
	return s, err
}

func (r *sqliteSessionRepo) Update(ctx context.Context, s Session) error {
	res, err := r.db.ExecContext(ctx,
		`UPDATE sessions SET agent_id=?, kind=?, external_ref=?, topic_id=?, created_at=?, closed_at=? WHERE id=?`,
		nullStr(s.AgentID), s.Kind, nullStr(s.ExternalRef), nullStr(s.TopicID),
		fmtTime(s.CreatedAt), fmtTimePtr(s.ClosedAt), s.ID)
	if err != nil {
		return fmt.Errorf("repo: update session: %w", err)
	}
	return requireUpdated(res, "session", s.ID)
}

func (r *sqliteSessionRepo) ListRecent(ctx context.Context, limit int) ([]Session, error) {
	rows, err := r.db.QueryContext(ctx,
		`SELECT `+sessionCols+` FROM sessions ORDER BY created_at DESC LIMIT ?`, limit)
	if err != nil {
		return nil, fmt.Errorf("repo: list recent sessions: %w", err)
	}
	defer rows.Close()
	var out []Session
	for rows.Next() {
		s, err := r.scan(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, s)
	}
	return out, rows.Err()
}

func (r *sqliteSessionRepo) ListByAgent(ctx context.Context, agentID string) ([]Session, error) {
	rows, err := r.db.QueryContext(ctx, `SELECT `+sessionCols+` FROM sessions WHERE agent_id = ? ORDER BY created_at DESC`, agentID)
	if err != nil {
		return nil, fmt.Errorf("repo: list sessions: %w", err)
	}
	defer rows.Close()
	var out []Session
	for rows.Next() {
		s, err := r.scan(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, s)
	}
	return out, rows.Err()
}

// CloseIdleBefore 闲置回收（FR2.5）：最后活动 = 末条消息 created_at（无消息回退会话 created_at）。
func (r *sqliteSessionRepo) CloseIdleBefore(ctx context.Context, idleBefore, closedAt time.Time) ([]string, error) {
	rows, err := r.db.QueryContext(ctx,
		`SELECT s.id FROM sessions s
		 WHERE s.closed_at IS NULL
		   AND COALESCE((SELECT MAX(m.created_at) FROM messages m WHERE m.session_id = s.id), s.created_at) < ?`,
		fmtTime(idleBefore))
	if err != nil {
		return nil, fmt.Errorf("repo: find idle sessions: %w", err)
	}
	var ids []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			rows.Close()
			return nil, fmt.Errorf("repo: scan idle session: %w", err)
		}
		ids = append(ids, id)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("repo: list idle sessions: %w", err)
	}
	for _, id := range ids {
		// closed_at IS NULL 复核：并发下不重复归档
		if _, err := r.db.ExecContext(ctx,
			`UPDATE sessions SET closed_at = ? WHERE id = ? AND closed_at IS NULL`, fmtTime(closedAt), id); err != nil {
			return ids, fmt.Errorf("repo: close idle session %s: %w", id, err)
		}
	}
	return ids, nil
}

// ---------- messages ----------

type sqliteMessageRepo struct{ db *sql.DB }

const messageCols = "id, session_id, sender_type, sender_id, content, reply_to, created_at"

func (r *sqliteMessageRepo) Append(ctx context.Context, m *Message) error {
	res, err := r.db.ExecContext(ctx,
		`INSERT INTO messages (session_id, sender_type, sender_id, content, reply_to, created_at)
		 VALUES (?,?,?,?,?,?)`,
		m.SessionID, m.SenderType, m.SenderID, m.Content, nullStr(m.ReplyTo), fmtTime(m.CreatedAt))
	if err != nil {
		return fmt.Errorf("repo: insert message: %w", err)
	}
	id, err := res.LastInsertId()
	if err != nil {
		return fmt.Errorf("repo: message last insert id: %w", err)
	}
	m.ID = id
	return nil
}

func (r *sqliteMessageRepo) scanList(rows *sql.Rows) ([]Message, error) {
	defer rows.Close()
	var out []Message
	for rows.Next() {
		var m Message
		var replyTo sql.NullString
		var createdAt string
		if err := rows.Scan(&m.ID, &m.SessionID, &m.SenderType, &m.SenderID, &m.Content, &replyTo, &createdAt); err != nil {
			return nil, err
		}
		m.ReplyTo = strPtr(replyTo)
		t, err := parseTime(createdAt)
		if err != nil {
			return nil, err
		}
		m.CreatedAt = t
		out = append(out, m)
	}
	return out, rows.Err()
}

func (r *sqliteMessageRepo) ListBySession(ctx context.Context, sessionID string) ([]Message, error) {
	rows, err := r.db.QueryContext(ctx,
		`SELECT `+messageCols+` FROM messages WHERE session_id = ? ORDER BY id ASC`, sessionID)
	if err != nil {
		return nil, fmt.Errorf("repo: list messages: %w", err)
	}
	return r.scanList(rows)
}

func (r *sqliteMessageRepo) RecentBySession(ctx context.Context, sessionID string, limit int) ([]Message, error) {
	rows, err := r.db.QueryContext(ctx,
		`SELECT `+messageCols+` FROM messages WHERE session_id = ? ORDER BY created_at DESC, id DESC LIMIT ?`,
		sessionID, limit)
	if err != nil {
		return nil, fmt.Errorf("repo: recent messages: %w", err)
	}
	msgs, err := r.scanList(rows)
	if err != nil {
		return nil, err
	}
	// 返回时间升序（倒序查询后翻转）
	for i, j := 0, len(msgs)-1; i < j; i, j = i+1, j-1 {
		msgs[i], msgs[j] = msgs[j], msgs[i]
	}
	return msgs, nil
}

// ---------- groups ----------

type sqliteGroupRepo struct{ db *sql.DB }

func (r *sqliteGroupRepo) Put(ctx context.Context, g Group) error {
	_, err := r.db.ExecContext(ctx,
		`INSERT INTO groups (id, name, members_json, arbiter_config_json) VALUES (?,?,?,?)
		 ON CONFLICT(id) DO UPDATE SET name=excluded.name, members_json=excluded.members_json,
		   arbiter_config_json=excluded.arbiter_config_json`,
		g.ID, g.Name, g.MembersJSON, g.ArbiterConfigJSON)
	if err != nil {
		return fmt.Errorf("repo: put group: %w", err)
	}
	return nil
}

func (r *sqliteGroupRepo) Get(ctx context.Context, id string) (Group, error) {
	var g Group
	err := r.db.QueryRowContext(ctx,
		`SELECT id, name, members_json, arbiter_config_json FROM groups WHERE id = ?`, id).
		Scan(&g.ID, &g.Name, &g.MembersJSON, &g.ArbiterConfigJSON)
	if errors.Is(err, sql.ErrNoRows) {
		return Group{}, fmt.Errorf("%w: group %s", ErrNotFound, id)
	}
	if err != nil {
		return Group{}, fmt.Errorf("repo: get group: %w", err)
	}
	return g, nil
}

func (r *sqliteGroupRepo) List(ctx context.Context) ([]Group, error) {
	rows, err := r.db.QueryContext(ctx, `SELECT id, name, members_json, arbiter_config_json FROM groups ORDER BY id`)
	if err != nil {
		return nil, fmt.Errorf("repo: list groups: %w", err)
	}
	defer rows.Close()
	var out []Group
	for rows.Next() {
		var g Group
		if err := rows.Scan(&g.ID, &g.Name, &g.MembersJSON, &g.ArbiterConfigJSON); err != nil {
			return nil, err
		}
		out = append(out, g)
	}
	return out, rows.Err()
}

// ---------- tasks ----------

type sqliteTaskRepo struct{ db *sql.DB }

const taskCols = "id, workflow_id, agent_id, input, status, result, error, nodes_json, timeout_seconds, retries, created_at, finished_at"

func (r *sqliteTaskRepo) scan(row interface{ Scan(...any) error }) (Task, error) {
	var t Task
	var wf, agentID, input, result, errStr, nodes, created, finished sql.NullString
	var timeout, retries sql.NullInt64
	if err := row.Scan(&t.ID, &wf, &agentID, &input, &t.Status, &result, &errStr, &nodes, &timeout, &retries, &created, &finished); err != nil {
		return Task{}, err
	}
	t.WorkflowID = strPtr(wf)
	t.AgentID = strPtr(agentID)
	t.Input = strPtr(input)
	t.Result = strPtr(result)
	t.Error = strPtr(errStr)
	t.NodesJSON = strPtr(nodes)
	if timeout.Valid {
		v := int(timeout.Int64)
		t.TimeoutSeconds = &v
	}
	if retries.Valid {
		v := int(retries.Int64)
		t.Retries = &v
	}
	var err error
	if t.CreatedAt, err = parseTimePtr(created); err != nil {
		return Task{}, err
	}
	if t.FinishedAt, err = parseTimePtr(finished); err != nil {
		return Task{}, err
	}
	return t, nil
}

func (r *sqliteTaskRepo) Create(ctx context.Context, t Task) error {
	_, err := r.db.ExecContext(ctx,
		`INSERT INTO tasks (id, workflow_id, agent_id, input, status, result, error, nodes_json, timeout_seconds, retries, created_at, finished_at)
		 VALUES (?,?,?,?,?,?,?,?,?,?,?,?)`,
		t.ID, nullStr(t.WorkflowID), nullStr(t.AgentID), nullStr(t.Input), t.Status,
		nullStr(t.Result), nullStr(t.Error), nullStr(t.NodesJSON),
		nullInt(t.TimeoutSeconds), nullInt(t.Retries), fmtTimePtr(t.CreatedAt), fmtTimePtr(t.FinishedAt))
	if err != nil {
		return fmt.Errorf("repo: insert task: %w", err)
	}
	return nil
}

func nullInt(p *int) sql.NullInt64 {
	if p == nil {
		return sql.NullInt64{}
	}
	return sql.NullInt64{Int64: int64(*p), Valid: true}
}

func (r *sqliteTaskRepo) Get(ctx context.Context, id string) (Task, error) {
	t, err := r.scan(r.db.QueryRowContext(ctx, `SELECT `+taskCols+` FROM tasks WHERE id = ?`, id))
	if errors.Is(err, sql.ErrNoRows) {
		return Task{}, fmt.Errorf("%w: task %s", ErrNotFound, id)
	}
	return t, err
}

func (r *sqliteTaskRepo) Update(ctx context.Context, t Task) error {
	res, err := r.db.ExecContext(ctx,
		`UPDATE tasks SET workflow_id=?, agent_id=?, input=?, status=?, result=?, error=?, nodes_json=?,
		   timeout_seconds=?, retries=?, created_at=?, finished_at=? WHERE id=?`,
		nullStr(t.WorkflowID), nullStr(t.AgentID), nullStr(t.Input), t.Status,
		nullStr(t.Result), nullStr(t.Error), nullStr(t.NodesJSON),
		nullInt(t.TimeoutSeconds), nullInt(t.Retries), fmtTimePtr(t.CreatedAt), fmtTimePtr(t.FinishedAt), t.ID)
	if err != nil {
		return fmt.Errorf("repo: update task: %w", err)
	}
	return requireUpdated(res, "task", t.ID)
}

func (r *sqliteTaskRepo) List(ctx context.Context) ([]Task, error) {
	rows, err := r.db.QueryContext(ctx, `SELECT `+taskCols+` FROM tasks ORDER BY created_at DESC`)
	if err != nil {
		return nil, fmt.Errorf("repo: list tasks: %w", err)
	}
	defer rows.Close()
	var out []Task
	for rows.Next() {
		t, err := r.scan(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, t)
	}
	return out, rows.Err()
}

// ---------- event_log ----------

type sqliteEventRepo struct{ db *sql.DB }

// AppendEvents 批量追加；seq 显式写入（与 bus 发布时分配的序号一致）。
func (r *sqliteEventRepo) AppendEvents(ctx context.Context, events []bus.Event) error {
	if len(events) == 0 {
		return nil
	}
	tx, err := r.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("repo: event tx: %w", err)
	}
	defer tx.Rollback()
	stmt, err := tx.PrepareContext(ctx,
		`INSERT INTO event_log (seq, type, payload_json, created_at) VALUES (?,?,?,?)`)
	if err != nil {
		return fmt.Errorf("repo: event stmt: %w", err)
	}
	defer stmt.Close()
	for _, e := range events {
		payload := e.Payload
		if len(payload) == 0 {
			payload = json.RawMessage("{}")
		}
		if _, err := stmt.ExecContext(ctx, e.Seq, e.Type, string(payload), fmtTime(e.CreatedAt)); err != nil {
			return fmt.Errorf("repo: append event seq=%d: %w", e.Seq, err)
		}
	}
	return tx.Commit()
}

func (r *sqliteEventRepo) MaxEventSeq(ctx context.Context) (int64, error) {
	var max sql.NullInt64
	err := r.db.QueryRowContext(ctx, `SELECT MAX(seq) FROM event_log`).Scan(&max)
	if err != nil {
		return 0, fmt.Errorf("repo: max event seq: %w", err)
	}
	return max.Int64, nil
}

func (r *sqliteEventRepo) EventsAfter(ctx context.Context, afterSeq int64, limit int) ([]bus.Event, error) {
	rows, err := r.db.QueryContext(ctx,
		`SELECT seq, type, payload_json, created_at FROM event_log WHERE seq > ? ORDER BY seq ASC LIMIT ?`,
		afterSeq, limit)
	if err != nil {
		return nil, fmt.Errorf("repo: events after: %w", err)
	}
	defer rows.Close()
	var out []bus.Event
	for rows.Next() {
		var e bus.Event
		var payload, createdAt string
		if err := rows.Scan(&e.Seq, &e.Type, &payload, &createdAt); err != nil {
			return nil, err
		}
		e.Payload = json.RawMessage(payload)
		if e.CreatedAt, err = parseTime(createdAt); err != nil {
			return nil, err
		}
		out = append(out, e)
	}
	return out, rows.Err()
}
