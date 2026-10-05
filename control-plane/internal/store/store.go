// Package store persists Platform Intent (apps, operations, events) in SQLite.
//
// SQLite is the source of truth for what the user asked for and what the
// Control Plane intended to do. Runtime actual state lives in Kubernetes.
package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"

	_ "modernc.org/sqlite"
)

// App phases shown to the user (docs: plan-2 §8.1).
const (
	PhaseQueued        = "QUEUED"
	PhaseAgentStarting = "AGENT_STARTING"
	PhaseGenerating    = "GENERATING"
	PhaseTesting       = "TESTING"
	PhaseStarting      = "STARTING"
	PhaseReady         = "READY"
	PhaseFailed        = "FAILED"
	PhaseDeleting      = "DELETING"
	PhaseDeleted       = "DELETED"
)

// Operation kinds and states.
const (
	KindCreate = "create"
	KindModify = "modify"
	KindDelete = "delete"

	OpPending   = "PENDING"
	OpRunning   = "RUNNING"
	OpSucceeded = "SUCCEEDED"
	OpFailed    = "FAILED"
)

var (
	ErrNotFound = errors.New("not found")
	ErrConflict = errors.New("conflict")
)

type App struct {
	ID        string    `json:"id"`
	Name      string    `json:"name"`
	Phase     string    `json:"phase"`
	CreatedAt time.Time `json:"createdAt"`
	UpdatedAt time.Time `json:"updatedAt"`
}

type Operation struct {
	ID             string    `json:"id"`
	AppID          string    `json:"appId"`
	Kind           string    `json:"kind"`
	Prompt         string    `json:"prompt"`
	State          string    `json:"state"`
	Step           string    `json:"step"`
	Error          string    `json:"error,omitempty"`
	IdempotencyKey string    `json:"-"`
	Purge          bool      `json:"purge,omitempty"`       // delete: also remove PVCs. create (retry): start from an empty source
	UserMessage    string    `json:"userMessage,omitempty"` // failure reason in plain Japanese
	Trace          string    `json:"trace,omitempty"`       // raw AAP_TRACE JSON from the Agent (latency measurement)
	Detail         string    `json:"detail,omitempty"`      // raw log tail for operators (not shown to users)
	CreatedAt      time.Time `json:"createdAt"`
	UpdatedAt      time.Time `json:"updatedAt"`
}

type Event struct {
	ID        int64     `json:"id"`
	AppID     string    `json:"appId"`
	OpID      string    `json:"opId"`
	Phase     string    `json:"phase"`
	Message   string    `json:"message"`
	CreatedAt time.Time `json:"createdAt"`
}

type Store struct{ db *sql.DB }

const schema = `
CREATE TABLE IF NOT EXISTS apps (
  id TEXT PRIMARY KEY, name TEXT NOT NULL, phase TEXT NOT NULL,
  created_at TEXT NOT NULL, updated_at TEXT NOT NULL);
CREATE TABLE IF NOT EXISTS operations (
  id TEXT PRIMARY KEY, app_id TEXT NOT NULL, kind TEXT NOT NULL, prompt TEXT NOT NULL,
  state TEXT NOT NULL, step TEXT NOT NULL DEFAULT '', error TEXT NOT NULL DEFAULT '',
  idempotency_key TEXT, purge INTEGER NOT NULL DEFAULT 0,
  user_message TEXT NOT NULL DEFAULT '', detail TEXT NOT NULL DEFAULT '', trace TEXT NOT NULL DEFAULT '',
  created_at TEXT NOT NULL, updated_at TEXT NOT NULL);
CREATE UNIQUE INDEX IF NOT EXISTS ops_idem ON operations(app_id, idempotency_key) WHERE idempotency_key IS NOT NULL AND idempotency_key <> '';
CREATE INDEX IF NOT EXISTS ops_app ON operations(app_id, created_at);
CREATE TABLE IF NOT EXISTS events (
  id INTEGER PRIMARY KEY AUTOINCREMENT, app_id TEXT NOT NULL, op_id TEXT NOT NULL,
  phase TEXT NOT NULL, message TEXT NOT NULL, created_at TEXT NOT NULL);
CREATE INDEX IF NOT EXISTS events_app ON events(app_id, id);
`

// Open opens (and migrates) the database at path. Use ":memory:" in tests.
func Open(path string) (*Store, error) {
	dsn := path
	if path != ":memory:" {
		dsn = fmt.Sprintf("file:%s?_pragma=busy_timeout(5000)&_pragma=journal_mode(WAL)&_pragma=foreign_keys(1)", path)
	}
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(1) // single writer; keeps :memory: consistent too
	if _, err := db.Exec(schema); err != nil {
		return nil, err
	}
	// Columns added after the first release: ignore "duplicate column" on databases that have them.
	for _, col := range []string{
		`ALTER TABLE operations ADD COLUMN user_message TEXT NOT NULL DEFAULT ''`,
		`ALTER TABLE operations ADD COLUMN detail TEXT NOT NULL DEFAULT ''`,
		`ALTER TABLE operations ADD COLUMN trace TEXT NOT NULL DEFAULT ''`,
	} {
		if _, err := db.Exec(col); err != nil && !strings.Contains(err.Error(), "duplicate column") {
			return nil, err
		}
	}
	return &Store{db: db}, nil
}

func (s *Store) Close() error { return s.db.Close() }

func ts(t time.Time) string { return t.UTC().Format(time.RFC3339Nano) }
func pt(s string) time.Time { t, _ := time.Parse(time.RFC3339Nano, s); return t }

// CreateApp inserts an app, or returns ErrConflict if the id is taken.
func (s *Store) CreateApp(ctx context.Context, id, name string) (App, error) {
	now := time.Now()
	_, err := s.db.ExecContext(ctx,
		`INSERT INTO apps(id,name,phase,created_at,updated_at) VALUES(?,?,?,?,?)`,
		id, name, PhaseQueued, ts(now), ts(now))
	if err != nil {
		if isUnique(err) {
			return App{}, ErrConflict
		}
		return App{}, err
	}
	return App{ID: id, Name: name, Phase: PhaseQueued, CreatedAt: now, UpdatedAt: now}, nil
}

func (s *Store) GetApp(ctx context.Context, id string) (App, error) {
	var a App
	var c, u string
	err := s.db.QueryRowContext(ctx, `SELECT id,name,phase,created_at,updated_at FROM apps WHERE id=?`, id).
		Scan(&a.ID, &a.Name, &a.Phase, &c, &u)
	if errors.Is(err, sql.ErrNoRows) {
		return App{}, ErrNotFound
	}
	a.CreatedAt, a.UpdatedAt = pt(c), pt(u)
	return a, err
}

// ListApps returns non-deleted apps, oldest first.
func (s *Store) ListApps(ctx context.Context) ([]App, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT id,name,phase,created_at,updated_at FROM apps WHERE phase<>? ORDER BY created_at`, PhaseDeleted)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []App
	for rows.Next() {
		var a App
		var c, u string
		if err := rows.Scan(&a.ID, &a.Name, &a.Phase, &c, &u); err != nil {
			return nil, err
		}
		a.CreatedAt, a.UpdatedAt = pt(c), pt(u)
		out = append(out, a)
	}
	return out, rows.Err()
}

func (s *Store) SetAppPhase(ctx context.Context, id, phase string) error {
	res, err := s.db.ExecContext(ctx, `UPDATE apps SET phase=?, updated_at=? WHERE id=?`, phase, ts(time.Now()), id)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	return nil
}

// RecreateApp resets a deleted app so its id can be reused.
func (s *Store) RecreateApp(ctx context.Context, id, name string) error {
	_, err := s.db.ExecContext(ctx, `UPDATE apps SET name=?, phase=?, updated_at=? WHERE id=? AND phase=?`,
		name, PhaseQueued, ts(time.Now()), id, PhaseDeleted)
	return err
}

// CreateOperation persists an operation BEFORE any Kubernetes call (write-ahead).
// If idempotencyKey was already used for this app, the existing operation is
// returned with created=false.
func (s *Store) CreateOperation(ctx context.Context, op Operation) (Operation, bool, error) {
	now := time.Now()
	op.State, op.CreatedAt, op.UpdatedAt = OpPending, now, now
	_, err := s.db.ExecContext(ctx,
		`INSERT INTO operations(id,app_id,kind,prompt,state,idempotency_key,purge,created_at,updated_at)
		 VALUES(?,?,?,?,?,?,?,?,?)`,
		op.ID, op.AppID, op.Kind, op.Prompt, op.State, nullable(op.IdempotencyKey), b2i(op.Purge), ts(now), ts(now))
	if err != nil {
		if isUnique(err) && op.IdempotencyKey != "" {
			existing, gerr := s.OperationByKey(ctx, op.AppID, op.IdempotencyKey)
			return existing, false, gerr
		}
		return Operation{}, false, err
	}
	return op, true, nil
}

func (s *Store) OperationByKey(ctx context.Context, appID, key string) (Operation, error) {
	return s.scanOp(s.db.QueryRowContext(ctx, opSelect+` WHERE app_id=? AND idempotency_key=?`, appID, key))
}

func (s *Store) GetOperation(ctx context.Context, id string) (Operation, error) {
	return s.scanOp(s.db.QueryRowContext(ctx, opSelect+` WHERE id=?`, id))
}

// ActiveOperation returns the unfinished operation of an app, if any.
func (s *Store) ActiveOperation(ctx context.Context, appID string) (Operation, error) {
	return s.scanOp(s.db.QueryRowContext(ctx,
		opSelect+` WHERE app_id=? AND state IN (?,?) ORDER BY created_at LIMIT 1`, appID, OpPending, OpRunning))
}

func (s *Store) ListOperations(ctx context.Context, appID string) ([]Operation, error) {
	rows, err := s.db.QueryContext(ctx, opSelect+` WHERE app_id=? ORDER BY created_at DESC`, appID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Operation
	for rows.Next() {
		op, err := s.scanOp(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, op)
	}
	return out, rows.Err()
}

// UnfinishedOperations is used on startup to resume work after a restart.
func (s *Store) UnfinishedOperations(ctx context.Context) ([]Operation, error) {
	rows, err := s.db.QueryContext(ctx, opSelect+` WHERE state IN (?,?) ORDER BY created_at`, OpPending, OpRunning)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Operation
	for rows.Next() {
		op, err := s.scanOp(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, op)
	}
	return out, rows.Err()
}

func (s *Store) UpdateOperation(ctx context.Context, id, state, step, errMsg string) error {
	_, err := s.db.ExecContext(ctx, `UPDATE operations SET state=?, step=?, error=?, updated_at=? WHERE id=?`,
		state, step, errMsg, ts(time.Now()), id)
	return err
}

// SetOperationTrace stores the Agent's latency trace (a JSON object) on the operation.
func (s *Store) SetOperationTrace(ctx context.Context, id, trace string) error {
	_, err := s.db.ExecContext(ctx, `UPDATE operations SET trace=?, updated_at=updated_at WHERE id=?`, trace, id)
	return err
}

// FailOperation marks an operation failed with a user-facing message and operator detail.
func (s *Store) FailOperation(ctx context.Context, id, step, errMsg, userMsg, detail string) error {
	_, err := s.db.ExecContext(ctx,
		`UPDATE operations SET state=?, step=?, error=?, user_message=?, detail=?, updated_at=? WHERE id=?`,
		OpFailed, step, errMsg, userMsg, detail, ts(time.Now()), id)
	return err
}

const opSelect = `SELECT id,app_id,kind,prompt,state,step,error,COALESCE(idempotency_key,''),purge,user_message,detail,trace,created_at,updated_at FROM operations`

type scanner interface{ Scan(...any) error }

func (s *Store) scanOp(r scanner) (Operation, error) {
	var op Operation
	var c, u string
	var purge int
	err := r.Scan(&op.ID, &op.AppID, &op.Kind, &op.Prompt, &op.State, &op.Step, &op.Error, &op.IdempotencyKey, &purge, &op.UserMessage, &op.Detail, &op.Trace, &c, &u)
	if errors.Is(err, sql.ErrNoRows) {
		return Operation{}, ErrNotFound
	}
	op.Purge = purge == 1
	op.CreatedAt, op.UpdatedAt = pt(c), pt(u)
	return op, err
}

func (s *Store) AddEvent(ctx context.Context, appID, opID, phase, msg string) (Event, error) {
	now := time.Now()
	res, err := s.db.ExecContext(ctx, `INSERT INTO events(app_id,op_id,phase,message,created_at) VALUES(?,?,?,?,?)`,
		appID, opID, phase, msg, ts(now))
	if err != nil {
		return Event{}, err
	}
	id, _ := res.LastInsertId()
	return Event{ID: id, AppID: appID, OpID: opID, Phase: phase, Message: msg, CreatedAt: now}, nil
}

// EventsAfter returns events of an app with id > after, oldest first.
func (s *Store) EventsAfter(ctx context.Context, appID string, after int64) ([]Event, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT id,app_id,op_id,phase,message,created_at FROM events WHERE app_id=? AND id>? ORDER BY id`, appID, after)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Event
	for rows.Next() {
		var e Event
		var c string
		if err := rows.Scan(&e.ID, &e.AppID, &e.OpID, &e.Phase, &e.Message, &c); err != nil {
			return nil, err
		}
		e.CreatedAt = pt(c)
		out = append(out, e)
	}
	return out, rows.Err()
}

func nullable(s string) any {
	if s == "" {
		return nil
	}
	return s
}
func b2i(b bool) int {
	if b {
		return 1
	}
	return 0
}
func isUnique(err error) bool {
	return err != nil && (strings.Contains(err.Error(), "UNIQUE constraint failed"))
}
