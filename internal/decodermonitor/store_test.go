package decodermonitor

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

func testStore(t *testing.T) (Config, *Store) {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("local watchdog storage runs on Linux")
	}
	cfg := DefaultConfig()
	cfg.Site = "Test"
	cfg.StateDir = filepath.Join(t.TempDir(), "state")
	s, err := OpenStore(&cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return cfg, s
}

func TestStoreAtomicStateEvidenceAndRestart(t *testing.T) {
	t.Parallel()
	cfg, store := testStore(t)
	if second, err := OpenStore(&cfg); err == nil {
		_ = second.Close()
		t.Fatal("two watchdogs own the same state")
	}
	at := time.Now()
	records := []Record{impactRecord(at)}
	if err := store.Update(records, at, func(s *State) error {
		s.Bookmark = Bookmark{Cursor: "saved", Key: "proof"}
		s.Incident = Incident{ID: 1, Severity: "critical"}
		s.Outbox = []Mail{{ID: 2, Subject: "pending", Created: at}}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if err := store.Update(records, at, func(_ *State) error { return nil }); err != nil {
		t.Fatal(err)
	}
	entries, err := os.ReadDir(filepath.Join(cfg.StateDir, "evidence"))
	if err != nil || len(entries) != 1 {
		t.Fatalf("evidence replay duplicated or lost: %d %v", len(entries), err)
	}
	for _, path := range []string{filepath.Join(cfg.StateDir, "state.json"), filepath.Join(cfg.StateDir, "evidence", entries[0].Name())} {
		info, err := os.Stat(path)
		if err != nil || info.Mode().Perm() != 0o600 {
			t.Fatalf("non-private evidence/state: %s %v", path, err)
		}
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	reopened, err := OpenStore(&cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = reopened.Close() }()
	s, err := reopened.Snapshot()
	if err != nil || s.Bookmark.Cursor != "saved" || s.Incident.Severity != "critical" || len(s.Outbox) != 1 {
		t.Fatalf("state lost on restart: %+v %v", s, err)
	}
}

func TestStoreFailedTransitionDoesNotAdvanceCursor(t *testing.T) {
	t.Parallel()
	cfg, store := testStore(t)
	at := time.Now()
	if err := store.Update(nil, at, func(s *State) error { s.Bookmark.Cursor = "old"; return nil }); err != nil {
		t.Fatal(err)
	}
	sentinel := errors.New("do not commit")
	err := store.Update([]Record{impactRecord(at)}, at, func(s *State) error { s.Bookmark.Cursor = "new"; return sentinel })
	if !errors.Is(err, sentinel) {
		t.Fatal(err)
	}
	state, err := store.Snapshot()
	if err != nil || state.Bookmark.Cursor != "old" {
		t.Fatalf("failed transition committed: %+v %v", state, err)
	}
	store.maxEvidence = 1
	if err := store.Update([]Record{impactRecord(at)}, at, func(s *State) error { s.Bookmark.Cursor = "new"; return nil }); err == nil {
		t.Fatal("over-quota batch committed")
	}
	state, err = store.Snapshot()
	if err != nil || state.Bookmark.Cursor != "old" {
		t.Fatal("evidence failure advanced cursor")
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	reopened, err := OpenStore(&cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = reopened.Close() })
	if state, err := reopened.Snapshot(); err != nil || state.Bookmark.Cursor != "old" {
		t.Fatalf("evidence failure advanced the cursor on disk: %+v %v", state.Bookmark, err)
	}
}

func TestStoreReplacesInterruptedTransaction(t *testing.T) {
	t.Parallel()
	cfg, store := testStore(t)
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	dirs := []string{cfg.StateDir, filepath.Join(cfg.StateDir, "evidence")}
	leftover := `{"interrupted":"` + strings.Repeat("x", 8192) + `"}`
	for _, dir := range dirs {
		if err := os.WriteFile(filepath.Join(dir, transactionName), []byte(leftover), 0o600); err != nil {
			t.Fatal(err)
		}
		if err := os.Chmod(filepath.Join(dir, transactionName), 0o644); err != nil { //nolint:gosec // Deliberately public leftover must not leak its mode.
			t.Fatal(err)
		}
	}
	reopened, err := OpenStore(&cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = reopened.Close() })
	at := time.Now()
	if err := reopened.Update([]Record{impactRecord(at)}, at, func(s *State) error { s.Bookmark.Cursor = "next"; return nil }); err != nil {
		t.Fatal(err)
	}
	evidence, err := os.ReadDir(dirs[1])
	if err != nil || len(evidence) != 1 {
		t.Fatalf("evidence batch not committed: %v %v", evidence, err)
	}
	for _, path := range []string{filepath.Join(dirs[0], "state.json"), filepath.Join(dirs[1], evidence[0].Name())} {
		data, err := os.ReadFile(path) //nolint:gosec // Fixed paths inside this test's temporary state directory.
		if err != nil || !json.Valid(data) || strings.Contains(string(data), "interrupted") {
			t.Fatalf("%s was not replaced cleanly: %v", path, err)
		}
		if info, err := os.Stat(path); err != nil || info.Mode().Perm() != 0o600 {
			t.Fatalf("%s inherited the leftover mode: %v", path, err)
		}
	}
	for _, dir := range dirs {
		if _, err := os.Lstat(filepath.Join(dir, transactionName)); !os.IsNotExist(err) {
			t.Fatalf("interrupted transaction left in %s: %v", dir, err)
		}
	}
}

func TestStoreSnapshotIsIsolatedFromCommittedState(t *testing.T) {
	t.Parallel()
	_, store := testStore(t)
	at := time.Now()
	if err := store.Update(nil, at, func(s *State) error { s.Outbox = []Mail{{ID: 1}}; return nil }); err != nil {
		t.Fatal(err)
	}
	snapshot, err := store.Snapshot()
	if err != nil {
		t.Fatal(err)
	}
	snapshot.Outbox[0].ID = 99
	sentinel := errors.New("abort")
	if err := store.Update(nil, at, func(s *State) error { s.Outbox[0].Attempts = 5; return sentinel }); !errors.Is(err, sentinel) {
		t.Fatal(err)
	}
	s, err := store.Snapshot()
	if err != nil || s.Outbox[0].ID != 1 || s.Outbox[0].Attempts != 0 {
		t.Fatalf("committed state was mutated outside a commit: %+v %v", s.Outbox, err)
	}
}

func TestStoreSkipsUnchangedStateAndKeepsBaselineInMemory(t *testing.T) {
	t.Parallel()
	cfg, store := testStore(t)
	at := time.Now()
	path := filepath.Join(cfg.StateDir, "state.json")
	for range 2 {
		if err := store.Update(nil, at, func(s *State) error { s.Bookmark.Cursor = "saved"; return nil }); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	if err := store.Update(nil, at, func(s *State) error {
		s.LastObserved, s.Stalled, s.LastPCM.Applied = at, 1, 42
		s.NotHealthySince = at
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatal("state rewritten although no durable field changed")
	}
	s, err := store.Snapshot()
	if err != nil || !s.LastObserved.Equal(at) || s.Stalled != 1 || s.LastPCM.Applied != 42 || !s.NotHealthySince.Equal(at) {
		t.Fatalf("in-memory baseline lost: %+v %v", s, err)
	}
	if err := store.Update(nil, at, func(s *State) error { s.Bookmark.Cursor = "moved"; return nil }); err != nil {
		t.Fatal(err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	reopened, err := OpenStore(&cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = reopened.Close() })
	s, err = reopened.Snapshot()
	if err != nil || s.Bookmark.Cursor != "moved" || !s.LastObserved.IsZero() || s.Stalled != 0 || s.LastPCM.Applied != 0 || !s.NotHealthySince.IsZero() {
		t.Fatalf("restart did not start from a fresh baseline: %+v %v", s, err)
	}
}

func TestStoreCorruptionAndSymlinkFailClosed(t *testing.T) {
	t.Parallel()
	cfg, store := testStore(t)
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(cfg.StateDir, "state.json"), []byte(`{"version":99}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if s, err := OpenStore(&cfg); err == nil {
		_ = s.Close()
		t.Fatal("corrupt state silently discarded")
	}
	if err := os.Remove(filepath.Join(cfg.StateDir, "state.json")); err != nil {
		t.Fatal(err)
	}
	outside := filepath.Join(t.TempDir(), "outside.json")
	if err := os.WriteFile(outside, []byte(`{"version":1}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(cfg.StateDir, "state.json")); err != nil {
		t.Fatal(err)
	}
	if s, err := OpenStore(&cfg); err == nil {
		_ = s.Close()
		t.Fatal("state symlink escaped root")
	}
}

func TestEvidenceRetentionDoesNotTouchUnownedFiles(t *testing.T) {
	t.Parallel()
	cfg, store := testStore(t)
	at := time.Now()
	if err := store.Update([]Record{impactRecord(at)}, at, func(_ *State) error { return nil }); err != nil {
		t.Fatal(err)
	}
	entries, err := os.ReadDir(filepath.Join(cfg.StateDir, "evidence"))
	if err != nil {
		t.Fatal(err)
	}
	old := filepath.Join(cfg.StateDir, "evidence", entries[0].Name())
	if err := os.Chtimes(old, at.Add(-8*24*time.Hour), at.Add(-8*24*time.Hour)); err != nil {
		t.Fatal(err)
	}
	other := filepath.Join(cfg.StateDir, "evidence", "keep.txt")
	if err := os.WriteFile(other, []byte("not watchdog evidence"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(other, at.Add(-9*24*time.Hour), at.Add(-9*24*time.Hour)); err != nil {
		t.Fatal(err)
	}
	if err := store.Prune(at); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(old); !os.IsNotExist(err) {
		t.Fatal("expired batch retained")
	}
	if _, err := os.Stat(other); err != nil {
		t.Fatal("unowned file removed")
	}
}

func TestStoreRewritesAfterAmbiguousCommit(t *testing.T) {
	t.Parallel()
	if os.Geteuid() == 0 {
		t.Skip("root bypasses the directory permissions that make the sync fail")
	}
	cfg, store := testStore(t)
	at := time.Now()
	if err := store.Update(nil, at, func(s *State) error { s.Bookmark.Cursor = "before"; return nil }); err != nil {
		t.Fatal(err)
	}
	// Write and search permission allow the rename; without read permission
	// the directory cannot be opened for its sync.
	if err := os.Chmod(cfg.StateDir, 0o300); err != nil { //nolint:gosec // Owner-only directory without read permission.
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(cfg.StateDir, 0o700) }) //nolint:gosec // Restores the private state directory mode.
	change := func(s *State) error { s.Bookmark.Cursor = "renamed"; return nil }
	for range 2 {
		if err := store.Update(nil, at, change); err == nil {
			t.Fatal("unsynced state was acknowledged as committed")
		}
	}
	if err := os.Chmod(cfg.StateDir, 0o700); err != nil { //nolint:gosec // Restores the private state directory mode.
		t.Fatal(err)
	}
	// The rename happened, so memory must follow disk rather than the old cache.
	if err := store.Update(nil, at, func(*State) error { return nil }); err != nil {
		t.Fatal(err)
	}
	if s, err := store.Snapshot(); err != nil || s.Bookmark.Cursor != "renamed" {
		t.Fatalf("memory diverged from the ambiguous commit: %+v %v", s.Bookmark, err)
	}
}

func TestStoreRejectedChangeCannotAlterCommittedRestarts(t *testing.T) {
	t.Parallel()
	_, store := testStore(t)
	at := time.Now()
	if err := store.Update(nil, at, func(s *State) error { s.Restarts = []time.Time{at, at.Add(time.Second)}; return nil }); err != nil {
		t.Fatal(err)
	}
	// Engine.observeService prunes the slice in place before a later error
	// (for example a full outbox) rejects the whole transition.
	sentinel := errors.New("outbox full")
	if err := store.Update(nil, at, func(s *State) error {
		s.Restarts = append(s.Restarts[:0], at.Add(time.Hour))
		return sentinel
	}); !errors.Is(err, sentinel) {
		t.Fatal(err)
	}
	s, err := store.Snapshot()
	if err != nil || len(s.Restarts) != 2 || !s.Restarts[0].Equal(at) {
		t.Fatalf("rejected transition altered committed restarts: %v %v", s.Restarts, err)
	}
}
