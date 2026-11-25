package api

import (
	"errors"
	"io"
	"net/http"
	"strings"
	"unicode/utf8"

	"github.com/k-kanke/ai-app-platform/control-plane/internal/kube"
	"github.com/k-kanke/ai-app-platform/control-plane/internal/patch"
	"github.com/k-kanke/ai-app-platform/control-plane/internal/store"
)

const maxTaskPromptRunes = 4000

// registerTasks adds the repository-task routes (only when the feature is enabled).
func (s *Server) registerTasks(m *http.ServeMux) {
	m.HandleFunc("POST /api/v1/tasks", s.createTask)
	m.HandleFunc("GET /api/v1/tasks", s.listTasks)
	m.HandleFunc("GET /api/v1/tasks/{id}", s.getTask)
	m.HandleFunc("GET /api/v1/tasks/{id}/patch", s.getTaskPatch)
	m.HandleFunc("POST /api/v1/tasks/{id}/approve", s.approveTask)
	m.HandleFunc("POST /api/v1/tasks/{id}/reject", s.rejectTask)
	m.HandleFunc("GET /api/v1/tasks/{id}/events", s.taskSSE)
	m.HandleFunc("POST /internal/v1/tasks/{id}/events", s.taskAgentEvent)
	m.HandleFunc("POST /internal/v1/tasks/{id}/patch", s.taskAgentPatch)
	m.HandleFunc("GET /internal/v1/tasks/{id}/patch", s.publisherPatch)
	m.HandleFunc("POST /internal/v1/tasks/{id}/published", s.publisherPublished)
}

type taskView struct {
	store.Task
	Summary *patch.Summary `json:"summary,omitempty"` // what the patch changes, once there is one
}

func (s *Server) taskView(r *http.Request, t store.Task) taskView {
	v := taskView{Task: t}
	if t.PatchSHA != "" && t.State != store.TaskExpired {
		if data, _, err := s.St.GetTaskPatch(r.Context(), t.ID); err == nil {
			sum := patch.Summarize(data)
			v.Summary = &sum
		}
	}
	return v
}

func (s *Server) createTask(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Repo   string `json:"repo"`
		Prompt string `json:"prompt"`
	}
	if err := decode(r, &in); err != nil {
		writeErr(w, 400, "invalid JSON")
		return
	}
	in.Repo, in.Prompt = strings.TrimSpace(in.Repo), strings.TrimSpace(in.Prompt)
	if !kube.ValidRepo(in.Repo) || (s.TaskRepoRe != nil && !s.TaskRepoRe.MatchString(in.Repo)) {
		writeErr(w, 400, "repo must be an allowed owner/name")
		return
	}
	if in.Prompt == "" || utf8.RuneCountInString(in.Prompt) > maxTaskPromptRunes {
		writeErr(w, 400, "prompt must be 1-%d characters", maxTaskPromptRunes)
		return
	}
	key := r.Header.Get("Idempotency-Key")
	if n, err := s.St.CountActiveTasks(r.Context()); err == nil && n >= s.TaskMaxActive && s.TaskMaxActive > 0 {
		// A repeated request for an existing task is answered below; only brand-new work is refused.
		if key == "" {
			writeErr(w, 429, "too many tasks are running; try again later")
			return
		}
	}
	t, created, err := s.St.CreateTask(r.Context(), store.Task{ID: newID(), Repo: in.Repo, Prompt: in.Prompt, IdempotencyKey: key})
	if err != nil {
		writeErr(w, 500, "could not create the task")
		return
	}
	if !created {
		writeJSON(w, 200, s.taskView(r, t))
		return
	}
	s.Orch.EmitTask(r.Context(), t.ID, store.TaskPending, "受け付けました")
	s.Orch.SubmitTask(s.BaseCtx, t)
	writeJSON(w, 202, s.taskView(r, t))
}

func (s *Server) listTasks(w http.ResponseWriter, r *http.Request) {
	ts, err := s.St.ListTasks(r.Context(), 50)
	if err != nil {
		writeErr(w, 500, "could not list tasks")
		return
	}
	out := make([]taskView, 0, len(ts))
	for _, t := range ts {
		out = append(out, taskView{Task: t}) // the list stays light: no patch parsing
	}
	writeJSON(w, 200, out)
}

func (s *Server) loadTask(w http.ResponseWriter, r *http.Request) (store.Task, bool) {
	t, err := s.St.GetTask(r.Context(), r.PathValue("id"))
	if errors.Is(err, store.ErrNotFound) {
		writeErr(w, 404, "task not found")
		return t, false
	}
	if err != nil {
		writeErr(w, 500, "could not load the task")
		return t, false
	}
	return t, true
}

func (s *Server) getTask(w http.ResponseWriter, r *http.Request) {
	if t, ok := s.loadTask(w, r); ok {
		writeJSON(w, 200, s.taskView(r, t))
	}
}

func (s *Server) getTaskPatch(w http.ResponseWriter, r *http.Request) {
	t, ok := s.loadTask(w, r)
	if !ok {
		return
	}
	data, sha, err := s.St.GetTaskPatch(r.Context(), t.ID)
	if err != nil {
		writeErr(w, 404, "no patch")
		return
	}
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.Header().Set("X-Patch-SHA256", sha)
	w.Header().Set("X-Content-Type-Options", "nosniff")
	_, _ = w.Write(data)
}

// approveTask records the human's decision for exactly the patch whose hash they send.
func (s *Server) approveTask(w http.ResponseWriter, r *http.Request) {
	t, ok := s.loadTask(w, r)
	if !ok {
		return
	}
	var in struct {
		PatchSHA256 string `json:"patchSha256"`
	}
	if err := decode(r, &in); err != nil || in.PatchSHA256 == "" {
		writeErr(w, 400, "patchSha256 is required")
		return
	}
	if data, _, err := s.St.GetTaskPatch(r.Context(), t.ID); err == nil && patch.Summarize(data).Blocked {
		writeErr(w, 409, "this patch cannot be published")
		return
	}
	switch err := s.St.ApproveTask(r.Context(), t.ID, in.PatchSHA256); {
	case errors.Is(err, store.ErrConflict):
		writeErr(w, 409, "the task is not waiting for approval, or the patch changed")
		return
	case err != nil:
		writeErr(w, 500, "could not approve")
		return
	}
	s.Orch.EmitTask(r.Context(), t.ID, store.TaskApproved, "承認しました")
	if s.Orch.PublishEnabled() {
		// Approved: hand over to the Publisher. The state change is a compare-and-set, so a double click
		// cannot start two publishers.
		if err := s.St.TransitionTask(r.Context(), t.ID, store.TaskApproved, store.TaskPublishing); err == nil {
			cur, _ := s.St.GetTask(r.Context(), t.ID)
			s.Orch.SubmitPublish(s.BaseCtx, cur)
		}
	}
	t, _ = s.St.GetTask(r.Context(), t.ID)
	writeJSON(w, 200, s.taskView(r, t))
}

func (s *Server) rejectTask(w http.ResponseWriter, r *http.Request) {
	t, ok := s.loadTask(w, r)
	if !ok {
		return
	}
	if err := s.St.TransitionTask(r.Context(), t.ID, store.TaskAwaitingApproval, store.TaskRejected); err != nil {
		if errors.Is(err, store.ErrConflict) {
			writeErr(w, 409, "the task is not waiting for approval")
			return
		}
		writeErr(w, 500, "could not reject")
		return
	}
	s.Orch.EmitTask(r.Context(), t.ID, store.TaskRejected, "却下しました")
	t, _ = s.St.GetTask(r.Context(), t.ID)
	writeJSON(w, 200, s.taskView(r, t))
}

func (s *Server) taskSSE(w http.ResponseWriter, r *http.Request) {
	if _, ok := s.loadTask(w, r); !ok {
		return
	}
	s.streamEvents(w, r, store.TaskKey(r.PathValue("id")))
}

// ---- Implementer-facing (per-task bearer token) ----

func (s *Server) taskAuth(w http.ResponseWriter, r *http.Request) (store.Task, bool) {
	id := r.PathValue("id")
	tok := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
	if !s.Orch.ValidTaskToken(id, tok) {
		writeErr(w, 401, "unauthorized")
		return store.Task{}, false
	}
	t, err := s.St.GetTask(r.Context(), id)
	if err != nil || t.State != store.TaskRunning {
		writeErr(w, 409, "the task is not accepting input")
		return t, false
	}
	return t, true
}

func (s *Server) taskAgentEvent(w http.ResponseWriter, r *http.Request) {
	t, ok := s.taskAuth(w, r)
	if !ok {
		return
	}
	var in struct{ Phase, Message string }
	if err := decode(r, &in); err != nil {
		writeErr(w, 400, "invalid JSON")
		return
	}
	msg := in.Message
	if len(msg) > 500 {
		msg = msg[:500]
	}
	s.Orch.EmitTask(r.Context(), t.ID, "PROGRESS", msg) // the agent cannot invent task states
	w.WriteHeader(http.StatusNoContent)
}

// taskAgentPatch receives the Implementer's `git diff --binary`. The body is capped, the task must be
// RUNNING, and the stored bytes are what the human later approves (by hash).
func (s *Server) taskAgentPatch(w http.ResponseWriter, r *http.Request) {
	t, ok := s.taskAuth(w, r)
	if !ok {
		return
	}
	limit := s.TaskMaxPatch
	if limit <= 0 {
		limit = 1 << 20
	}
	data, err := io.ReadAll(http.MaxBytesReader(w, r.Body, int64(limit)+1))
	if err != nil || len(data) > limit {
		writeErr(w, 413, "patch is larger than %d bytes", limit)
		return
	}
	sha, err := s.St.SaveTaskPatch(r.Context(), t.ID, data, limit)
	switch {
	case errors.Is(err, store.ErrTooLarge):
		writeErr(w, 413, "patch is larger than %d bytes", limit)
		return
	case errors.Is(err, store.ErrConflict):
		writeErr(w, 409, "the task is not accepting a patch")
		return
	case err != nil:
		writeErr(w, 500, "could not store the patch")
		return
	}
	writeJSON(w, 200, map[string]any{"sha256": sha, "bytes": len(data)})
}

// ---- Publisher-facing (a different per-task bearer token from the Implementer's) ----

func (s *Server) publisherAuth(w http.ResponseWriter, r *http.Request) (store.Task, bool) {
	id := r.PathValue("id")
	tok := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
	if !s.Orch.ValidPublisherToken(id, tok) {
		writeErr(w, 401, "unauthorized")
		return store.Task{}, false
	}
	t, err := s.St.GetTask(r.Context(), id)
	if err != nil || t.State != store.TaskPublishing {
		writeErr(w, 409, "the task is not being published")
		return t, false
	}
	return t, true
}

// publisherPatch serves the approved patch to the Publisher (only while the task is PUBLISHING).
func (s *Server) publisherPatch(w http.ResponseWriter, r *http.Request) {
	t, ok := s.publisherAuth(w, r)
	if !ok {
		return
	}
	data, sha, err := s.St.GetTaskPatch(r.Context(), t.ID)
	if err != nil || sha != t.PatchSHA {
		writeErr(w, 404, "no approved patch")
		return
	}
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.Header().Set("X-Patch-SHA256", sha)
	_, _ = w.Write(data)
}

// publisherPublished records the opened pull request. The URL must point at a pull request of the task's repository.
func (s *Server) publisherPublished(w http.ResponseWriter, r *http.Request) {
	t, ok := s.publisherAuth(w, r)
	if !ok {
		return
	}
	var in struct {
		Branch   string `json:"branch"`
		PRURL    string `json:"prUrl"`
		PRNumber int    `json:"prNumber"`
	}
	if err := decode(r, &in); err != nil {
		writeErr(w, 400, "invalid JSON")
		return
	}
	if !strings.HasPrefix(in.PRURL, "https://github.com/"+t.Repo+"/pull/") || in.PRNumber <= 0 || in.Branch != "agent/"+t.ID {
		writeErr(w, 400, "not a pull request of this task")
		return
	}
	if err := s.St.MarkPublished(r.Context(), t.ID, in.PRURL, in.PRNumber); err != nil {
		writeErr(w, 409, "the task is not being published")
		return
	}
	s.Orch.EmitTask(r.Context(), t.ID, store.TaskPublished, "PR を作りました: "+in.PRURL)
	w.WriteHeader(http.StatusNoContent)
}
