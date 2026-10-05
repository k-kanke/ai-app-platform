package store

import (
	"context"
	"errors"
	"path/filepath"
	"testing"
)

func TestAppAndOperationLifecycle(t *testing.T) {
	ctx := context.Background()
	s, err := Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.CreateApp(ctx, "meal", "ご飯"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.CreateApp(ctx, "meal", "again"); !errors.Is(err, ErrConflict) {
		t.Fatalf("want ErrConflict, got %v", err)
	}
	op, created, err := s.CreateOperation(ctx, Operation{ID: "op1", AppID: "meal", Kind: KindCreate, Prompt: "p", IdempotencyKey: "k1"})
	if err != nil || !created {
		t.Fatalf("create op: %v %v", created, err)
	}
	// Same idempotency key -> same operation, not a duplicate.
	dup, created, err := s.CreateOperation(ctx, Operation{ID: "op2", AppID: "meal", Kind: KindCreate, Prompt: "p", IdempotencyKey: "k1"})
	if err != nil || created || dup.ID != op.ID {
		t.Fatalf("idempotency broken: %+v created=%v err=%v", dup, created, err)
	}
	un, _ := s.UnfinishedOperations(ctx)
	if len(un) != 1 {
		t.Fatalf("want 1 unfinished, got %d", len(un))
	}
	if err := s.UpdateOperation(ctx, "op1", OpSucceeded, "done", ""); err != nil {
		t.Fatal(err)
	}
	if _, err := s.ActiveOperation(ctx, "meal"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("no active op expected, got %v", err)
	}
}

func TestEventsAfter(t *testing.T) {
	ctx := context.Background()
	s, _ := Open(":memory:")
	e1, _ := s.AddEvent(ctx, "meal", "op1", PhaseQueued, "a")
	s.AddEvent(ctx, "meal", "op1", PhaseGenerating, "b")
	got, _ := s.EventsAfter(ctx, "meal", e1.ID)
	if len(got) != 1 || got[0].Message != "b" {
		t.Fatalf("unexpected: %+v", got)
	}
}

func TestPersistsAcrossReopen(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "cp.db")
	s, _ := Open(path)
	s.CreateApp(ctx, "meal", "ご飯")
	s.CreateOperation(ctx, Operation{ID: "op1", AppID: "meal", Kind: KindCreate, Prompt: "p"})
	s.Close()
	s2, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	un, _ := s2.UnfinishedOperations(ctx)
	if len(un) != 1 || un[0].ID != "op1" {
		t.Fatalf("operation lost across restart: %+v", un)
	}
}

func TestFailOperationStoresUserMessageAndMigratesOldDB(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "old.db")
	// A database created before user_message/detail existed.
	old, _ := Open(path)
	old.db.Exec(`ALTER TABLE operations DROP COLUMN user_message`)
	old.db.Exec(`ALTER TABLE operations DROP COLUMN detail`)
	old.Close()

	s, err := Open(path) // must add the columns again
	if err != nil {
		t.Fatalf("migration failed: %v", err)
	}
	s.CreateApp(ctx, "meal", "m")
	s.CreateOperation(ctx, Operation{ID: "op1", AppID: "meal", Kind: KindCreate, Prompt: "p"})
	if err := s.FailOperation(ctx, "op1", "failed", "boom", "AIの利用上限に達しています", "log tail"); err != nil {
		t.Fatal(err)
	}
	got, _ := s.GetOperation(ctx, "op1")
	if got.State != OpFailed || got.UserMessage != "AIの利用上限に達しています" || got.Detail != "log tail" || got.Error != "boom" {
		t.Fatalf("unexpected: %+v", got)
	}
}
