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

func TestStoreAtomicStateAndRestart(t *testing.T) {
	t.Parallel()
	cfg, store := testStore(t)
	if second, err := OpenStore(&cfg); err == nil {
		_ = second.Close()
		t.Fatal("two watchdogs own the same state")
	}
	at := time.Now()
	if err := store.Update(func(s *State) error {
		s.Bookmark = Bookmark{Cursor: "saved", Key: "proof"}
		s.Incident = Incident{ID: 1, Severity: "critical"}
		s.Outbox = []Mail{{ID: 2, Subject: "pending", Created: at}}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if err := store.Update(func(_ *State) error { return nil }); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(cfg.StateDir, "state.json")
	if info, err := os.Stat(path); err != nil || info.Mode().Perm() != 0o600 {
		t.Fatalf("non-private state: %s %v", path, err)
	}
	if _, err := os.Stat(filepath.Join(cfg.StateDir, "evidence")); !os.IsNotExist(err) {
		t.Fatalf("store created an evidence directory: %v", err)
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
	if err := store.Update(func(s *State) error { s.Bookmark.Cursor = "old"; return nil }); err != nil {
		t.Fatal(err)
	}
	sentinel := errors.New("do not commit")
	err := store.Update(func(s *State) error { s.Bookmark.Cursor = "new"; return sentinel })
	if !errors.Is(err, sentinel) {
		t.Fatal(err)
	}
	state, err := store.Snapshot()
	if err != nil || state.Bookmark.Cursor != "old" {
		t.Fatalf("failed transition committed: %+v %v", state, err)
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
		t.Fatalf("failed transition advanced the cursor on disk: %+v %v", state.Bookmark, err)
	}
}

func TestStoreReplacesInterruptedTransaction(t *testing.T) {
	t.Parallel()
	cfg, store := testStore(t)
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	transaction := filepath.Join(cfg.StateDir, transactionName)
	leftover := `{"interrupted":"` + strings.Repeat("x", 8192) + `"}`
	if err := os.WriteFile(transaction, []byte(leftover), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(transaction, 0o644); err != nil { //nolint:gosec // Deliberately public leftover must not leak its mode.
		t.Fatal(err)
	}
	reopened, err := OpenStore(&cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = reopened.Close() })
	if err := reopened.Update(func(s *State) error { s.Bookmark.Cursor = "next"; return nil }); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(cfg.StateDir, "state.json")
	data, err := os.ReadFile(path) //nolint:gosec // Fixed path inside this test's temporary state directory.
	if err != nil || !json.Valid(data) || strings.Contains(string(data), "interrupted") {
		t.Fatalf("state was not replaced cleanly: %v", err)
	}
	if info, err := os.Stat(path); err != nil || info.Mode().Perm() != 0o600 {
		t.Fatalf("state inherited the leftover mode: %v", err)
	}
	if _, err := os.Lstat(transaction); !os.IsNotExist(err) {
		t.Fatalf("interrupted transaction left behind: %v", err)
	}
}

func TestStoreSnapshotIsIsolatedFromCommittedState(t *testing.T) {
	t.Parallel()
	_, store := testStore(t)
	if err := store.Update(func(s *State) error { s.Outbox = []Mail{{ID: 1}}; return nil }); err != nil {
		t.Fatal(err)
	}
	snapshot, err := store.Snapshot()
	if err != nil {
		t.Fatal(err)
	}
	snapshot.Outbox[0].ID = 99
	sentinel := errors.New("abort")
	if err := store.Update(func(s *State) error { s.Outbox[0].Attempts = 5; return sentinel }); !errors.Is(err, sentinel) {
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
		if err := store.Update(func(s *State) error { s.Bookmark.Cursor = "saved"; return nil }); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	if err := store.Update(func(s *State) error {
		s.LastObserved, s.Stalled, s.LastPCM.Applied = at, 1, 42
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatal("state rewritten although no durable field changed")
	}
	s, err := store.Snapshot()
	if err != nil || !s.LastObserved.Equal(at) || s.Stalled != 1 || s.LastPCM.Applied != 42 {
		t.Fatalf("in-memory baseline lost: %+v %v", s, err)
	}
	if err := store.Update(func(s *State) error { s.Bookmark.Cursor = "moved"; return nil }); err != nil {
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
	if err != nil || s.Bookmark.Cursor != "moved" || !s.LastObserved.IsZero() || s.Stalled != 0 || s.LastPCM.Applied != 0 {
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

func TestStoreRewritesAfterAmbiguousCommit(t *testing.T) {
	t.Parallel()
	if os.Geteuid() == 0 {
		t.Skip("root bypasses the directory permissions that make the sync fail")
	}
	cfg, store := testStore(t)
	if err := store.Update(func(s *State) error { s.Bookmark.Cursor = "before"; return nil }); err != nil {
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
		if err := store.Update(change); err == nil {
			t.Fatal("unsynced state was acknowledged as committed")
		}
	}
	if err := os.Chmod(cfg.StateDir, 0o700); err != nil { //nolint:gosec // Restores the private state directory mode.
		t.Fatal(err)
	}
	// The rename happened, so memory must follow disk rather than the old cache.
	if err := store.Update(func(*State) error { return nil }); err != nil {
		t.Fatal(err)
	}
	if s, err := store.Snapshot(); err != nil || s.Bookmark.Cursor != "renamed" {
		t.Fatalf("memory diverged from the ambiguous commit: %+v %v", s.Bookmark, err)
	}
}

func TestIncidentFirstJournalCursorPersistsAndAppearsInNotice(t *testing.T) {
	for _, tc := range []struct {
		name, cursor, hint string
		kernelFirst        bool
	}{
		{"journal", "s=first;i=1", "journalctl -u micrompx.service --cursor='s=first;i=1'", false},
		{"quoted_cursor", "s=first'entry", "journalctl -u micrompx.service --cursor='s=first'\\''entry'", false},
		{"kernel_then_journal", "s=first;i=1", "journalctl -u micrompx.service --cursor='s=first;i=1'", true},
		{"kernel_only", "", "", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			cfg, store := testStore(t)
			e := Engine{Config: cfg}
			at := time.Now().UTC()
			step := func(store *Store, p *Probe, now time.Time) {
				t.Helper()
				if err := store.Update(func(s *State) error { return e.Step(s, p, now) }); err != nil {
					t.Fatal(err)
				}
			}
			// Kernel faults use two consecutive polls, so the scenarios also hold
			// where explicit faults must be confirmed before an incident opens.
			if tc.kernelFirst {
				for i := range 2 {
					now := at.Add(time.Duration(i-1) * 10 * time.Second)
					p := healthyProbe(now, 0)
					p.PCM.State = "XRUN"
					step(store, p, now)
				}
				s, err := store.Snapshot()
				if err != nil {
					t.Fatal(err)
				}
				checkIncidentCursorNotice(t, &s, "", "")
			}
			if tc.cursor != "" {
				now := at.Add(10 * time.Second)
				p := healthyProbe(now, 1920000)
				first, second := impactRecord(now), impactRecord(now.Add(time.Second))
				first.Cursor, second.Cursor = tc.cursor, "s=second"
				p.Records = []Record{{Cursor: "unrelated", Message: "Buffer filling"}, first, second}
				step(store, p, now)
				if !tc.kernelFirst {
					s, err := store.Snapshot()
					if err != nil {
						t.Fatal(err)
					}
					checkIncidentCursorNotice(t, &s, tc.cursor, tc.hint)
				}
			}
			if err := store.Close(); err != nil {
				t.Fatal(err)
			}
			reopened, err := OpenStore(&cfg)
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = reopened.Close() })
			s, err := reopened.Snapshot()
			if err != nil || s.Incident.FirstJournalCursor != tc.cursor {
				t.Fatalf("first journal impact cursor lost or overwritten: %+v %v", s.Incident, err)
			}
			// Simulate delivered mail, then request an escalation or reminder.
			if err := reopened.Update(func(s *State) error { s.Outbox = nil; return nil }); err != nil {
				t.Fatal(err)
			}
			for i := range 2 {
				now := at.Add(seconds(cfg.ReminderSeconds) + time.Minute + time.Duration(i)*10*time.Second)
				p := healthyProbe(now, 2*1920000)
				p.PCM.State = "XRUN"
				if tc.cursor != "" {
					p.Records = []Record{impactRecord(now)}
				}
				step(reopened, p, now)
			}
			s, err = reopened.Snapshot()
			if err != nil {
				t.Fatal(err)
			}
			checkIncidentCursorNotice(t, &s, tc.cursor, tc.hint)
		})
	}
}

func checkIncidentCursorNotice(t *testing.T, s *State, cursor, hint string) {
	t.Helper()
	if s.Incident.FirstJournalCursor != cursor || len(s.Outbox) != 1 {
		t.Fatalf("wrong incident cursor or notice count: %+v", s)
	}
	body := s.Outbox[0].Body
	if hint == "" {
		if strings.Contains(body, "journalctl") || strings.Contains(body, "Export journal records") {
			t.Fatalf("kernel-only notice has a journal hint: %s", body)
		}
	} else if !strings.Contains(body, hint+"\n") {
		t.Fatalf("notice lacks the first impact's export command %q: %s", hint, body)
	}
}
