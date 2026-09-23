package localfile

import (
	"errors"
	"os"
	"path/filepath"
	"slices"
	"syscall"
	"testing"

	"github.com/ItIsCuthNotCup/MetaCog-Agent/harness/session"
)

func TestListSessionEntriesSkipsRemovedFiles(t *testing.T) {
	for _, test := range []struct {
		name    string
		removed []session.ID
		want    []session.ID
	}{
		{"one removed", []session.ID{"middle"}, []session.ID{"first", "last"}},
		{"all removed", []session.ID{"first", "middle", "last"}, []session.ID{}},
	} {
		t.Run(test.name, func(t *testing.T) {
			directory := t.TempDir()
			store, err := New(directory)
			if err != nil {
				t.Fatal(err)
			}
			for _, id := range []session.ID{"first", "middle", "last"} {
				if _, err := store.Create(t.Context(), id); err != nil {
					t.Fatal(err)
				}
			}
			entries, err := os.ReadDir(directory)
			if err != nil {
				t.Fatal(err)
			}
			for _, id := range test.removed {
				if err := os.Remove(store.sessionPath(id)); err != nil {
					t.Fatal(err)
				}
			}
			listed, err := store.listSessionEntries(t.Context(), entries)
			if err != nil {
				t.Fatal(err)
			}
			if listed == nil {
				t.Fatal("successful list returned a nil slice")
			}
			ids := make([]session.ID, len(listed))
			for index, info := range listed {
				ids[index] = info.ID
			}
			if !slices.Equal(ids, test.want) {
				t.Fatalf("listed IDs = %v, want %v", ids, test.want)
			}
		})
	}
}

func TestListSessionEntriesSkipsFilesWithStatErrors(t *testing.T) {
	directory := filepath.Join(t.TempDir(), "sessions")
	store, err := New(directory)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.Create(t.Context(), "session-1"); err != nil {
		t.Fatal(err)
	}
	entries, err := os.ReadDir(directory)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(store.sessionPath("session-1")); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(directory); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(directory, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := entries[0].Info(); !errors.Is(err, syscall.ENOTDIR) {
		t.Fatalf("stat error = %v, want ENOTDIR", err)
	}
	listed, err := store.listSessionEntries(t.Context(), entries)
	if err != nil || listed == nil || len(listed) != 0 {
		t.Fatalf("list with stat errors = %#v, %v", listed, err)
	}
}
