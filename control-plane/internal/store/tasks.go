package store

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"errors"
	"time"
)

// Task states (docs/multi-agent-execution-plan.md). A task is one "do this in that repository"
// request: a workspace PVC, an Implementer Job, then an approval and (Phase 4) a Publisher.
const (
	TaskPending          = "PENDING"           // accepted, not started
	TaskRunning          = "RUNNING"           // workspace / Implementer in progress
	TaskAwaitingApproval = "AWAITING_APPROVAL" // a patch exists and waits for a human
	TaskApproved         = "APPROVED"          // the human approved this exact patch (hash)
	TaskPublishing       = "PUBLISHING"        // Publisher is pushing / opening the PR
	TaskPublished        = "PUBLISHED"         // PR opened
	TaskNoChanges        = "NO_CHANGES"        // the Implementer finished without changing anything
	TaskRejected         = "REJECTED"          // the human said no
	TaskFailed           = "FAILED"
	TaskExpired          = "EXPIRED" // retention passed: workspace and patch were removed
)

// TaskKey is the event stream key of a task (events.app_id), so tasks reuse the events table and SSE broker.
func TaskKey(id string) string { return "task:" + id }

// Task is the Platform Intent of one repository task.
type Task struct {
	ID          string `json:"id"`
	Repo        string `json:"repo"` // "owner/name"
	Prompt      string `json:"prompt"`
	State       string `json:"state"`
	Step        string `json:"step,omitempty"`
	Error       string `json:"error,omitempty"`
	UserMessage string `json:"userMessage,omitempty"`
	Detail      string `json:"detail,omitempty"` // raw log tail for operators
	Trace       string `json:"trace,omitempty"`

	PatchSHA string `json:"patchSha256,omitempty"`
	PatchLen int    `json:"patchBytes,omitempty"`
	PRURL    string `json:"prUrl,omitempty"`
	PRNumber int    `json:"prNumber,omitempty"`

	IdempotencyKey string    `json:"-"`
	CreatedAt      time.Time `json:"createdAt"`
	UpdatedAt      time.Time `json:"updatedAt"`
	FinishedAt     time.Time `json:"finishedAt,omitzero"` // zero while active
}

const tasksSchema = `
CREATE TABLE IF NOT EXISTS tasks (
  id TEXT PRIMARY KEY, repo TEXT NOT NULL, prompt TEXT NOT NULL,
  state TEXT NOT NULL, step TEXT NOT NULL DEFAULT '', error TEXT NOT NULL DEFAULT '',
  user_message TEXT NOT NULL DEFAULT '', detail TEXT NOT NULL DEFAULT '', trace TEXT NOT NULL DEFAULT '',
  patch_sha TEXT NOT NULL DEFAULT '', patch_len INTEGER NOT NULL DEFAULT 0,
  pr_url TEXT NOT NULL DEFAULT '', pr_number INTEGER NOT NULL DEFAULT 0,
  idempotency_key TEXT,
  created_at TEXT NOT NULL, updated_at TEXT NOT NULL, finished_at TEXT NOT NULL DEFAULT '');
CREATE UNIQUE INDEX IF NOT EXISTS tasks_idem ON tasks(idempotency_key) WHERE idempotency_key IS NOT NULL AND idempotency_key <> '';
CREATE INDEX IF NOT EXISTS tasks_created ON tasks(created_at);
CREATE TABLE IF NOT EXISTS task_patches (
  task_id TEXT PRIMARY KEY, sha256 TEXT NOT NULL, data BLOB NOT NULL, created_at TEXT NOT NULL);
`

const taskCols = `id, repo, prompt, state, step, error, user_message, detail, trace, patch_sha, patch_len, pr_url, pr_number, idempotency_key, created_at, updated_at, finished_at`

func scanTask(r scanner) (Task, error) {
	var t Task
	var idem sql.NullString
	var c, u, f string
	if err := r.Scan(&t.ID, &t.Repo, &t.Prompt, &t.State, &t.Step, &t.Error, &t.UserMessage, &t.Detail, &t.Trace,
		&t.PatchSHA, &t.PatchLen, &t.PRURL, &t.PRNumber, &idem, &c, &u, &f); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return Task{}, ErrNotFound
		}
		return Task{}, err
	}
	t.IdempotencyKey = idem.String
	t.CreatedAt, t.UpdatedAt = pt(c), pt(u)
	if f != "" {
		t.FinishedAt = pt(f)
	}
	return t, nil
}

// IsActive reports whether the task still holds cluster resources it is working with.
func (t Task) IsActive() bool { return t.State == TaskPending || t.State == TaskRunning }

// CreateTask inserts a task. With a non-empty idempotency key, a repeated request returns the
// original task and created=false.
func (s *Store) CreateTask(ctx context.Context, t Task) (Task, bool, error) {
	now := time.Now().UTC()
	t.State, t.CreatedAt, t.UpdatedAt = TaskPending, now, now
	_, err := s.db.ExecContext(ctx, `INSERT INTO tasks (id, repo, prompt, state, idempotency_key, created_at, updated_at) VALUES (?,?,?,?,?,?,?)`,
		t.ID, t.Repo, t.Prompt, t.State, nullable(t.IdempotencyKey), ts(now), ts(now))
	if err != nil {
		if isUnique(err) && t.IdempotencyKey != "" {
			existing, gerr := scanTask(s.db.QueryRowContext(ctx, `SELECT `+taskCols+` FROM tasks WHERE idempotency_key=?`, t.IdempotencyKey))
			return existing, false, gerr
		}
		if isUnique(err) {
			return Task{}, false, ErrConflict
		}
		return Task{}, false, err
	}
	return t, true, nil
}

func (s *Store) GetTask(ctx context.Context, id string) (Task, error) {
	return scanTask(s.db.QueryRowContext(ctx, `SELECT `+taskCols+` FROM tasks WHERE id=?`, id))
}

// ListTasks returns the newest tasks first.
func (s *Store) ListTasks(ctx context.Context, limit int) ([]Task, error) {
	if limit <= 0 || limit > 200 {
		limit = 50
	}
	rows, err := s.db.QueryContext(ctx, `SELECT `+taskCols+` FROM tasks ORDER BY created_at DESC, id DESC LIMIT ?`, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return collectTasks(rows)
}

func collectTasks(rows *sql.Rows) ([]Task, error) {
	var out []Task
	for rows.Next() {
		t, err := scanTask(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, t)
	}
	return out, rows.Err()
}

// UnfinishedTasks are the tasks a Control Plane restart must pick up again (oldest first).
func (s *Store) UnfinishedTasks(ctx context.Context) ([]Task, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT `+taskCols+` FROM tasks WHERE state IN (?,?,?) ORDER BY created_at`, TaskPending, TaskRunning, TaskPublishing)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return collectTasks(rows)
}

// CountActiveTasks is how many tasks are PENDING or RUNNING (to cap concurrency).
func (s *Store) CountActiveTasks(ctx context.Context) (int, error) {
	var n int
	err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM tasks WHERE state IN (?,?)`, TaskPending, TaskRunning).Scan(&n)
	return n, err
}

// ExpiredTasks are finished (or abandoned while awaiting approval) tasks older than cutoff whose
// workspace and patch have not been removed yet.
func (s *Store) ExpiredTasks(ctx context.Context, cutoff time.Time) ([]Task, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT `+taskCols+` FROM tasks
	  WHERE state <> ? AND state NOT IN (?,?,?) AND updated_at < ? ORDER BY created_at`,
		TaskExpired, TaskPending, TaskRunning, TaskPublishing, ts(cutoff))
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return collectTasks(rows)
}

func isTerminal(state string) bool {
	switch state {
	case TaskPublished, TaskNoChanges, TaskRejected, TaskFailed, TaskExpired:
		return true
	}
	return false
}

// SetTaskState moves a task to state. step may be "" to keep it.
func (s *Store) SetTaskState(ctx context.Context, id, state, step string) error {
	fin := ""
	if isTerminal(state) {
		fin = ts(time.Now())
	}
	res, err := s.db.ExecContext(ctx, `UPDATE tasks SET state=?, step=CASE WHEN ?='' THEN step ELSE ? END,
	  finished_at=CASE WHEN ?='' THEN finished_at ELSE ? END, updated_at=? WHERE id=?`,
		state, step, step, fin, fin, ts(time.Now()), id)
	return affected(res, err)
}

// FailTask records a failure with a plain-language message and the raw detail for operators.
func (s *Store) FailTask(ctx context.Context, id, step, errMsg, userMsg, detail string) error {
	res, err := s.db.ExecContext(ctx, `UPDATE tasks SET state=?, step=?, error=?, user_message=?, detail=?, finished_at=?, updated_at=? WHERE id=?`,
		TaskFailed, step, errMsg, userMsg, detail, ts(time.Now()), ts(time.Now()), id)
	return affected(res, err)
}

func (s *Store) SetTaskTrace(ctx context.Context, id, trace string) error {
	res, err := s.db.ExecContext(ctx, `UPDATE tasks SET trace=?, updated_at=? WHERE id=?`, trace, ts(time.Now()), id)
	return affected(res, err)
}

// SetTaskPR records the opened pull request.
func (s *Store) SetTaskPR(ctx context.Context, id, url string, number int) error {
	res, err := s.db.ExecContext(ctx, `UPDATE tasks SET pr_url=?, pr_number=?, updated_at=? WHERE id=?`, url, number, ts(time.Now()), id)
	return affected(res, err)
}

func affected(res sql.Result, err error) error {
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	return nil
}

// ErrTooLarge is returned when a patch is bigger than the allowed size.
var ErrTooLarge = errors.New("patch too large")

// SaveTaskPatch stores the patch the Implementer uploaded (replacing an earlier one) and returns its
// sha256. The task must be RUNNING: a patch arriving after approval must not change what was approved.
func (s *Store) SaveTaskPatch(ctx context.Context, id string, data []byte, maxBytes int) (string, error) {
	if maxBytes > 0 && len(data) > maxBytes {
		return "", ErrTooLarge
	}
	sum := sha256.Sum256(data)
	sha := hex.EncodeToString(sum[:])
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return "", err
	}
	defer tx.Rollback()
	var state string
	if err := tx.QueryRowContext(ctx, `SELECT state FROM tasks WHERE id=?`, id).Scan(&state); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return "", ErrNotFound
		}
		return "", err
	}
	if state != TaskRunning {
		return "", ErrConflict
	}
	now := ts(time.Now())
	if _, err := tx.ExecContext(ctx, `INSERT INTO task_patches (task_id, sha256, data, created_at) VALUES (?,?,?,?)
	  ON CONFLICT(task_id) DO UPDATE SET sha256=excluded.sha256, data=excluded.data, created_at=excluded.created_at`, id, sha, data, now); err != nil {
		return "", err
	}
	if _, err := tx.ExecContext(ctx, `UPDATE tasks SET patch_sha=?, patch_len=?, updated_at=? WHERE id=?`, sha, len(data), now, id); err != nil {
		return "", err
	}
	return sha, tx.Commit()
}

// GetTaskPatch returns the stored patch and its sha256.
func (s *Store) GetTaskPatch(ctx context.Context, id string) ([]byte, string, error) {
	var data []byte
	var sha string
	err := s.db.QueryRowContext(ctx, `SELECT data, sha256 FROM task_patches WHERE task_id=?`, id).Scan(&data, &sha)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, "", ErrNotFound
	}
	return data, sha, err
}

// DeleteTaskPatch drops the patch (retention).
func (s *Store) DeleteTaskPatch(ctx context.Context, id string) error {
	_, err := s.db.ExecContext(ctx, `DELETE FROM task_patches WHERE task_id=?`, id)
	return err
}

// ApproveTask moves AWAITING_APPROVAL to APPROVED only if sha matches the stored patch. This is what
// ties the human's decision to the exact bytes they saw: a different patch hash is a conflict.
func (s *Store) ApproveTask(ctx context.Context, id, sha string) error {
	res, err := s.db.ExecContext(ctx, `UPDATE tasks SET state=?, updated_at=? WHERE id=? AND state=? AND patch_sha=? AND patch_sha<>''`,
		TaskApproved, ts(time.Now()), id, TaskAwaitingApproval, sha)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		if _, gerr := s.GetTask(ctx, id); gerr != nil {
			return gerr
		}
		return ErrConflict
	}
	return nil
}

// TransitionTask moves a task from one state to another only if it is currently in from
// (compare-and-set), so two requests racing on the same task cannot both win.
func (s *Store) TransitionTask(ctx context.Context, id, from, to string) error {
	fin := ""
	if isTerminal(to) {
		fin = ts(time.Now())
	}
	res, err := s.db.ExecContext(ctx, `UPDATE tasks SET state=?, finished_at=CASE WHEN ?='' THEN finished_at ELSE ? END, updated_at=? WHERE id=? AND state=?`,
		to, fin, fin, ts(time.Now()), id, from)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		if _, gerr := s.GetTask(ctx, id); gerr != nil {
			return gerr
		}
		return ErrConflict
	}
	return nil
}

// MarkPublished records the pull request and moves PUBLISHING to PUBLISHED in one step.
func (s *Store) MarkPublished(ctx context.Context, id, prURL string, prNumber int) error {
	now := ts(time.Now())
	res, err := s.db.ExecContext(ctx, `UPDATE tasks SET state=?, pr_url=?, pr_number=?, finished_at=?, updated_at=? WHERE id=? AND state=?`,
		TaskPublished, prURL, prNumber, now, now, id, TaskPublishing)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		if _, gerr := s.GetTask(ctx, id); gerr != nil {
			return gerr
		}
		return ErrConflict
	}
	return nil
}
