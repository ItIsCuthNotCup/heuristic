package localfile

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"slices"
	"strings"

	"github.com/ItIsCuthNotCup/heuristic/harness/session"
	"github.com/ItIsCuthNotCup/heuristic/harness/sessionstore"
)

func (store *Store) ListSessions(ctx context.Context) ([]sessionstore.SessionInfo, error) {
	if err := context.Cause(ctx); err != nil {
		return nil, err
	}
	entries, err := os.ReadDir(store.directory)
	if err != nil {
		return nil, fmt.Errorf("list sessions: %w", err)
	}
	return store.listSessionEntries(ctx, entries)
}

func (store *Store) listSessionEntries(ctx context.Context, entries []fs.DirEntry) ([]sessionstore.SessionInfo, error) {
	sessions := make([]sessionstore.SessionInfo, 0)
	for _, entry := range entries {
		if err := context.Cause(ctx); err != nil {
			return nil, err
		}
		name, ok := strings.CutSuffix(entry.Name(), sessionFileSuffix)
		if !ok || !entry.Type().IsRegular() {
			continue
		}
		id := session.ID(name)
		if err := validateSessionID(id); err != nil {
			continue
		}
		info, err := entry.Info()
		if errors.Is(err, errors.ErrUnsupported) {
			return nil, fmt.Errorf("list session %q: %w", id, err)
		}
		if err != nil {
			continue
		}
		sessions = append(sessions, sessionstore.SessionInfo{
			ID: id, LastUpdatedAt: info.ModTime().UTC(),
		})
	}
	slices.SortFunc(sessions, func(left, right sessionstore.SessionInfo) int {
		return strings.Compare(string(left.ID), string(right.ID))
	})
	return sessions, nil
}
