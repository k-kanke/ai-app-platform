package store

import (
	"context"
	"errors"
	"testing"
	"time"
)

func newTask(t *testing.T, s *Store, id, key string) Task {
	t.Helper()
	tk, _, err := s.CreateTask(context.Background(), Task{ID: id, Repo: "k-kanke/genkan", Prompt: "p", IdempotencyKey: key})
	if err != nil {
		t.Fatal(err)
	}
	return tk
}

func TestTaskIdempotentCreate(t *testing.T) {
	ctx := context.Background()
	s, _ := Open(":memory:")
	a, created, err := s.CreateTask(ctx, Task{ID: "t1", Repo: "o/r", Prompt: "p", IdempotencyKey: "k"})
	if err != nil || !created {
		t.Fatalf("first: %v %v", created, err)
	}
	b, created, err := s.CreateTask(ctx, Task{ID: "t2", Repo: "o/r", Prompt: "p", IdempotencyKey: "k"})
	if err != nil || created || b.ID != a.ID {
		t.Fatalf("second should return the first: %+v created=%v err=%v", b, created, err)
	}
	if _, _, err := s.CreateTask(ctx, Task{ID: "t1", Repo: "o/r", Prompt: "p"}); !errors.Is(err, ErrConflict) {
		t.Fatalf("duplicate id: %v", err)
	}
}

func TestApproveNeedsTheExactPatch(t *testing.T) {
	ctx := context.Background()
	s, _ := Open(":memory:")
	newTask(t, s, "t1", "")
	if _, err := s.SaveTaskPatch(ctx, "t1", []byte("x"), 100); !errors.Is(err, ErrConflict) {
		t.Fatalf("a PENDING task must not accept a patch: %v", err)
	}
	s.SetTaskState(ctx, "t1", TaskRunning, "implementer")
	sha, err := s.SaveTaskPatch(ctx, "t1", []byte("diff one"), 100)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.ApproveTask(ctx, "t1", sha); !errors.Is(err, ErrConflict) {
		t.Fatalf("approving while RUNNING must fail: %v", err)
	}
	s.SetTaskState(ctx, "t1", TaskAwaitingApproval, "")
	if err := s.ApproveTask(ctx, "t1", "deadbeef"); !errors.Is(err, ErrConflict) {
		t.Fatalf("a wrong hash must be refused: %v", err)
	}
	if err := s.ApproveTask(ctx, "t1", sha); err != nil {
		t.Fatalf("approve: %v", err)
	}
	// After approval the patch can no longer be replaced.
	if _, err := s.SaveTaskPatch(ctx, "t1", []byte("sneaky"), 100); !errors.Is(err, ErrConflict) {
		t.Fatalf("patch swap after approval must fail: %v", err)
	}
	got, gotSha, _ := s.GetTaskPatch(ctx, "t1")
	if string(got) != "diff one" || gotSha != sha {
		t.Fatalf("stored patch changed: %q %s", got, gotSha)
	}
	if err := s.ApproveTask(ctx, "t1", sha); !errors.Is(err, ErrConflict) {
		t.Fatalf("approving twice: %v", err)
	}
	if err := s.ApproveTask(ctx, "nope", sha); !errors.Is(err, ErrNotFound) {
		t.Fatalf("unknown task: %v", err)
	}
}

func TestPatchSizeLimit(t *testing.T) {
	ctx := context.Background()
	s, _ := Open(":memory:")
	newTask(t, s, "t1", "")
	s.SetTaskState(ctx, "t1", TaskRunning, "")
	if _, err := s.SaveTaskPatch(ctx, "t1", make([]byte, 11), 10); !errors.Is(err, ErrTooLarge) {
		t.Fatalf("%v", err)
	}
}

func TestUnfinishedAndExpired(t *testing.T) {
	ctx := context.Background()
	s, _ := Open(":memory:")
	newTask(t, s, "pending", "")
	newTask(t, s, "running", "")
	newTask(t, s, "waiting", "")
	newTask(t, s, "failed", "")
	s.SetTaskState(ctx, "running", TaskRunning, "implementer")
	s.SetTaskState(ctx, "waiting", TaskAwaitingApproval, "")
	s.FailTask(ctx, "failed", "implementer", "boom", "うまくいきませんでした", "log")

	un, _ := s.UnfinishedTasks(ctx)
	if len(un) != 2 {
		t.Fatalf("unfinished = %d", len(un))
	}
	if n, _ := s.CountActiveTasks(ctx); n != 2 {
		t.Fatalf("active = %d", n)
	}
	// Nothing is old enough yet; once the cutoff is in the future, finished and abandoned tasks expire
	// but running/pending ones never do.
	if ex, _ := s.ExpiredTasks(ctx, time.Now().Add(-time.Hour), time.Now().Add(-time.Hour)); len(ex) != 0 {
		t.Fatalf("expired too early: %d", len(ex))
	}
	ex, _ := s.ExpiredTasks(ctx, time.Now().Add(time.Hour), time.Now().Add(time.Hour))
	if len(ex) != 2 {
		t.Fatalf("expired = %d, want waiting+failed", len(ex))
	}
	s.SetTaskState(ctx, "failed", TaskExpired, "")
	if ex, _ := s.ExpiredTasks(ctx, time.Now().Add(time.Hour), time.Now().Add(time.Hour)); len(ex) != 1 {
		t.Fatalf("an expired task must not be listed again: %d", len(ex))
	}
	f, _ := s.GetTask(ctx, "failed")
	if f.FinishedAt.IsZero() || f.UserMessage == "" {
		t.Fatalf("failure fields: %+v", f)
	}
}

func TestWatchedAndExpiryOfPublishedTasks(t *testing.T) {
	ctx := context.Background()
	s, _ := Open(":memory:")
	for _, id := range []string{"pub", "merged", "deploying", "done", "other"} {
		newTask(t, s, id, "")
	}
	s.SetTaskState(ctx, "pub", TaskPublished, "")
	s.SetTaskState(ctx, "merged", TaskMerged, "")
	s.SetTaskState(ctx, "deploying", TaskDeploying, "")
	s.SetTaskState(ctx, "done", TaskDeployed, "")
	s.SetTaskState(ctx, "other", TaskFailed, "")
	w, _ := s.WatchedTasks(ctx)
	if len(w) != 3 {
		t.Fatalf("watched = %d, want published+merged+deploying", len(w))
	}
	// Retention passed, but the watch window has not: a PUBLISHED task is still waiting for its merge.
	ex, _ := s.ExpiredTasks(ctx, time.Now().Add(time.Hour), time.Now().Add(-time.Hour))
	got := map[string]bool{}
	for _, e := range ex {
		got[e.ID] = true
	}
	if got["pub"] || got["merged"] || got["deploying"] || !got["done"] || !got["other"] {
		t.Fatalf("expired = %v: finished tasks expire, watched ones do not", got)
	}
	// Past the watch window an unmerged PR is let go; MERGED and DEPLOYING never expire.
	ex, _ = s.ExpiredTasks(ctx, time.Now().Add(time.Hour), time.Now().Add(time.Hour))
	got = map[string]bool{}
	for _, e := range ex {
		got[e.ID] = true
	}
	if !got["pub"] || got["merged"] || got["deploying"] {
		t.Fatalf("expired after the watch window = %v", got)
	}
}
