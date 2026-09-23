package localfile_test

import (
	"context"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/ItIsCuthNotCup/MetaCog-Agent/harness/operation"
	"github.com/ItIsCuthNotCup/MetaCog-Agent/harness/session"
	"github.com/ItIsCuthNotCup/MetaCog-Agent/harness/sessionstore"
	"github.com/ItIsCuthNotCup/MetaCog-Agent/harness/tool"
)

func TestListSessions(t *testing.T) {
	directory := t.TempDir()
	store := newStore(t, directory)
	listed, err := store.ListSessions(t.Context())
	if err != nil || listed == nil || len(listed) != 0 {
		t.Fatalf("empty list = %#v, %v", listed, err)
	}

	want := []sessionstore.SessionInfo{
		{ID: "a", LastUpdatedAt: time.Unix(0, 0).UTC()},
		{ID: "a-b", LastUpdatedAt: time.Date(2026, 8, 27, 12, 0, 0, 0, time.UTC)},
		{ID: "z", LastUpdatedAt: time.Date(2026, 8, 27, 13, 0, 0, 0, time.UTC)},
	}
	for _, info := range want {
		if _, err := store.Create(t.Context(), info.ID); err != nil {
			t.Fatal(err)
		}
		path := filepath.Join(directory, string(info.ID)+".session.jsonl")
		if err := os.Chtimes(path, info.LastUpdatedAt, info.LastUpdatedAt); err != nil {
			t.Fatal(err)
		}
	}
	for _, name := range []string{"README", ".session-pending.tmp", "old.session.json"} {
		if err := os.WriteFile(filepath.Join(directory, name), []byte("unrelated"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.Mkdir(filepath.Join(directory, "nested.session.jsonl"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("a.session.jsonl", filepath.Join(directory, "link.session.jsonl")); err != nil {
		t.Fatal(err)
	}
	listed, err = newStore(t, directory).ListSessions(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if len(listed) != len(want) {
		t.Fatalf("list = %#v, want %#v", listed, want)
	}
	for index, info := range listed {
		if info.ID != want[index].ID || !info.LastUpdatedAt.Equal(want[index].LastUpdatedAt) {
			t.Fatalf("list entry %d = %#v, want %#v", index, info, want[index])
		}
	}
}

func TestListSessionsTracksHistoryAndOperationWrites(t *testing.T) {
	directory := t.TempDir()
	store := newStore(t, directory)
	if _, err := store.Create(t.Context(), "session-1"); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(directory, "session-1.session.jsonl")
	future := time.Date(2100, 1, 1, 0, 0, 0, 0, time.UTC)
	for _, write := range []struct {
		name string
		run  func() error
	}{
		{"turn", func() error {
			return store.AppendTurn(t.Context(), "session-1", session.Turn{
				ID: "turn-1", Type: session.TurnRegular,
			})
		}},
		{"tool status", func() error {
			return store.AppendToolCallStatus(t.Context(), "session-1", sessionstore.ToolCallStatus{
				TurnID: "turn-1", CallID: "call-1",
				Status: tool.CallStatus{WaitingFor: []operation.ID{"operation-1"}},
				Operations: []operation.Operation{{
					ID: "operation-1", Type: "test", Version: 1, Status: operation.StatusReady,
				}},
			})
		}},
		{"operation", func() error {
			return store.SaveOperation(t.Context(), "session-1", operation.Operation{
				ID: "operation-1", Type: "test", Version: 1, Status: operation.StatusCompleted,
			})
		}},
	} {
		if err := os.Chtimes(path, future, future); err != nil {
			t.Fatal(err)
		}
		if err := write.run(); err != nil {
			t.Fatalf("%s: %v", write.name, err)
		}
		listed, err := store.ListSessions(t.Context())
		if err != nil {
			t.Fatal(err)
		}
		metadata, err := os.Stat(path)
		if err != nil {
			t.Fatal(err)
		}
		if len(listed) != 1 || listed[0].ID != "session-1" ||
			!listed[0].LastUpdatedAt.Equal(metadata.ModTime()) || !listed[0].LastUpdatedAt.Before(future) {
			t.Fatalf("list after %s = %#v", write.name, listed)
		}
	}
}

func TestListSessionsIncludesForksAndReplacements(t *testing.T) {
	directory := t.TempDir()
	store := newStore(t, directory)
	createSessionWithTurn(t, store, "parent", "turn-1", "")
	if _, err := store.Fork(t.Context(), "child", "parent", "turn-1"); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Create(t.Context(), "parent"); err != nil {
		t.Fatal(err)
	}
	listed, err := newStore(t, directory).ListSessions(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if len(listed) != 2 {
		t.Fatalf("list = %#v", listed)
	}
	for index, id := range []session.ID{"child", "parent"} {
		metadata, err := os.Stat(filepath.Join(directory, string(id)+".session.jsonl"))
		if err != nil {
			t.Fatal(err)
		}
		if listed[index].ID != id || !listed[index].LastUpdatedAt.Equal(metadata.ModTime()) {
			t.Fatalf("list entry %d = %#v, want file modification time for %q", index, listed[index], id)
		}
	}
}

func TestListSessionsDoesNotReadFiles(t *testing.T) {
	for _, test := range []struct {
		name    string
		content string
	}{
		{"empty", ""},
		{"incomplete", `{"type":"session"}`},
		{"invalid JSON", "invalid\n"},
		{"different header metadata", `{"type":"session","data":{"Version":2,"Session":{"ID":"other","CreatedAt":"2000-01-01T00:00:00Z"}}}` + "\n"},
	} {
		t.Run(test.name, func(t *testing.T) {
			directory := t.TempDir()
			store := newStore(t, directory)
			path := filepath.Join(directory, "session-1.session.jsonl")
			if err := os.WriteFile(path, []byte(test.content), 0o600); err != nil {
				t.Fatal(err)
			}
			if err := os.Chmod(path, 0); err != nil {
				t.Fatal(err)
			}
			listed, err := store.ListSessions(t.Context())
			if err != nil {
				t.Fatal(err)
			}
			if len(listed) != 1 || listed[0].ID != "session-1" {
				t.Fatalf("list = %#v, want metadata for session-1", listed)
			}
		})
	}
}

func TestListSessionsIgnoresInvalidFilenames(t *testing.T) {
	directory := t.TempDir()
	store := newStore(t, directory)
	for _, name := range []string{
		".session.jsonl", "invalid_id.session.jsonl", "with space.session.jsonl", "unicode-ø.session.jsonl",
	} {
		if err := os.WriteFile(filepath.Join(directory, name), nil, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	listed, err := store.ListSessions(t.Context())
	if err != nil || listed == nil || len(listed) != 0 {
		t.Fatalf("list with only invalid filenames = %#v, %v", listed, err)
	}
	if _, err := store.Create(t.Context(), "session-1"); err != nil {
		t.Fatal(err)
	}
	listed, err = store.ListSessions(t.Context())
	if err != nil || len(listed) != 1 || listed[0].ID != "session-1" {
		t.Fatalf("list with mixed filenames = %#v, %v", listed, err)
	}
}

func TestListSessionsReportsCancellationAndDirectoryErrors(t *testing.T) {
	directory := filepath.Join(t.TempDir(), "sessions")
	store := newStore(t, directory)
	ctx, cancel := context.WithCancelCause(t.Context())
	cause := errors.New("stop listing")
	cancel(cause)
	if _, err := store.ListSessions(ctx); !errors.Is(err, cause) {
		t.Fatalf("canceled list error = %v, want %v", err, cause)
	}
	if err := os.Remove(directory); err != nil {
		t.Fatal(err)
	}
	if _, err := store.ListSessions(t.Context()); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("missing directory error = %v, want fs.ErrNotExist", err)
	}
}
