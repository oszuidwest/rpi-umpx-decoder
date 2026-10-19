package decodermonitor

import (
	"bytes"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"os"
	"regexp"
	"slices"
	"sync"
	"time"
)

var evidenceName = regexp.MustCompile(`^[0-9a-f]{64}\.json$`)

// transactionName is the single in-progress write per directory. The exclusive
// lock and mu make the watchdog its only writer, so a leftover from a crash is
// removed and recreated.
const transactionName = ".watchdog.tmp"

// Store commits cursor, incident and outbox together, using fsync + atomic rename.
// Evidence is durable BEFORE the cursor advances. A crash in between may replay
// evidence, but cannot acknowledge unwritten journal records.
type Store struct {
	mu          sync.Mutex
	root        *os.Root
	evidence    *os.Root
	lock        *os.File
	retention   time.Duration
	maxEvidence int64
	loaded      bool   // false after a failed write: disk is authoritative again
	state       State  // last committed state, including fields kept only in memory
	saved       []byte // durably committed encoding of state; nil forces a rewrite
}

// OpenStore exclusively owns a dedicated private state directory for one site.
func OpenStore(cfg *Config) (*Store, error) {
	if err := os.MkdirAll(cfg.StateDir, 0o700); err != nil {
		return nil, fmt.Errorf("create watchdog state directory: %w", err)
	}
	root, err := os.OpenRoot(cfg.StateDir)
	if err != nil {
		return nil, fmt.Errorf("open watchdog state directory: %w", err)
	}
	s := &Store{root: root, retention: time.Duration(cfg.RetentionDays) * 24 * time.Hour, maxEvidence: int64(cfg.EvidenceMiB) << 20}
	ok := false
	defer func() {
		if !ok {
			_ = s.Close()
		}
	}()
	info, err := root.Stat(".")
	if err != nil || !info.IsDir() || info.Mode().Perm()&0o077 != 0 {
		return nil, fmt.Errorf("state directory must be private (mode 0700)")
	}
	s.lock, err = root.OpenFile("watchdog.lock", os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, fmt.Errorf("open watchdog lock: %w", err)
	}
	if err := lockState(s.lock); err != nil {
		return nil, fmt.Errorf("another watchdog owns this state directory: %w", err)
	}
	if err := root.MkdirAll("evidence", 0o700); err != nil {
		return nil, fmt.Errorf("create evidence directory: %w", err)
	}
	s.evidence, err = root.OpenRoot("evidence")
	if err != nil {
		return nil, fmt.Errorf("open evidence directory: %w", err)
	}
	if err := syncRoot(root); err != nil {
		return nil, err
	}
	if _, err := s.current(); err != nil {
		return nil, err
	}
	ok = true
	return s, nil
}

// Close releases the lock and directory handles.
func (s *Store) Close() error {
	var result error
	if s.evidence != nil {
		result = errors.Join(result, s.evidence.Close())
	}
	if s.lock != nil {
		result = errors.Join(result, s.lock.Close())
	}
	if s.root != nil {
		result = errors.Join(result, s.root.Close())
	}
	return result
}

func (s *Store) load() (State, error) {
	f, err := s.root.Open("state.json")
	if errors.Is(err, os.ErrNotExist) {
		return State{Version: 1}, nil
	}
	if err != nil {
		return State{}, fmt.Errorf("open watchdog state: %w", err)
	}
	defer func() { _ = f.Close() }()
	data, err := readPrivate(f, maxDocument)
	if err != nil {
		return State{}, fmt.Errorf("watchdog state: %w", err)
	}
	var state State
	if err := json.Unmarshal(data, &state); err != nil || state.Version != 1 || len(state.Outbox) > maxOutbox {
		return State{}, fmt.Errorf("watchdog state is corrupt or unsupported; refusing to discard it")
	}
	return state, nil
}

// current returns a copy of the committed state. After a failed write it
// re-reads disk, because the rename may have succeeded before the error. The
// next Update then rewrites the file, re-establishing its durability.
func (s *Store) current() (State, error) {
	if !s.loaded {
		state, err := s.load()
		if err != nil {
			return State{}, err
		}
		s.state, s.saved, s.loaded = state, nil, true
	}
	state := s.state
	state.Recent, state.Outbox = slices.Clone(s.state.Recent), slices.Clone(s.state.Outbox)
	return state, nil
}

// Snapshot returns the committed state.
func (s *Store) Snapshot() (State, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.current()
}

// Update persists one transition and its notifications without network I/O.
// The file is rewritten only when its durable fields changed.
func (s *Store) Update(records []Record, now time.Time, change func(*State) error) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	state, err := s.current()
	if err != nil {
		return err
	}
	if err := change(&state); err != nil {
		return err
	}
	if len(records) > 0 {
		if err := s.writeEvidence(records, now); err != nil {
			return err
		}
	}
	data, err := json.Marshal(state)
	if err != nil {
		return fmt.Errorf("encode watchdog state: %w", err)
	}
	if len(data) > maxDocument {
		return fmt.Errorf("watchdog state exceeds size limit")
	}
	if !bytes.Equal(data, s.saved) {
		if err := atomicWrite(s.root, "state.json", data); err != nil {
			s.loaded = false
			return err
		}
	}
	s.state, s.saved = state, data
	return nil
}

func (s *Store) writeEvidence(records []Record, now time.Time) error {
	data, err := json.Marshal(records)
	if err != nil {
		return fmt.Errorf("encode evidence: %w", err)
	}
	if int64(len(data)) > s.maxEvidence {
		return fmt.Errorf("evidence batch exceeds quota")
	}
	name := fmt.Sprintf("%x.json", sha256.Sum256(data))
	if existing, err := s.evidence.ReadFile(name); err == nil {
		if !bytes.Equal(existing, data) {
			return fmt.Errorf("evidence hash collision or corruption")
		}
		// A prior rename may have succeeded while its directory sync failed.
		// Re-establish durability before acknowledging this evidence again.
		return syncRoot(s.evidence)
	} else if !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("check evidence batch: %w", err)
	}
	if err := s.trimEvidence(now, int64(len(data))); err != nil {
		return err
	}
	return atomicWrite(s.evidence, name, data)
}

func (s *Store) trimEvidence(now time.Time, incoming int64) error {
	entries, err := fs.ReadDir(s.evidence.FS(), ".")
	if err != nil {
		return fmt.Errorf("list evidence: %w", err)
	}
	type item struct {
		name     string
		size     int64
		modified time.Time
	}
	var items []item
	var total int64
	for _, entry := range entries {
		if !evidenceName.MatchString(entry.Name()) || !entry.Type().IsRegular() {
			continue
		}
		info, err := entry.Info()
		if err != nil {
			return fmt.Errorf("stat evidence: %w", err)
		}
		items = append(items, item{entry.Name(), info.Size(), info.ModTime()})
		total += info.Size()
	}
	slices.SortFunc(items, func(a, b item) int { return a.modified.Compare(b.modified) })
	removed := 0
	for _, item := range items {
		if total+incoming <= s.maxEvidence && now.Sub(item.modified) <= s.retention {
			break
		}
		if err := s.evidence.Remove(item.name); err != nil {
			return fmt.Errorf("prune expired or over-quota evidence: %w", err)
		}
		total -= item.size
		removed++
	}
	if removed > 0 {
		if err := syncRoot(s.evidence); err != nil {
			return err
		}
		slog.Info("watchdog evidence retention applied", "removed_batches", removed)
	}
	return nil
}

func atomicWrite(root *os.Root, name string, data []byte) (retErr error) {
	// Never reuse a leftover: its mode or a symlink would carry over.
	if err := root.Remove(transactionName); err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("remove interrupted watchdog transaction: %w", err)
	}
	tmp, err := root.OpenFile(transactionName, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return fmt.Errorf("create watchdog transaction: %w", err)
	}
	defer func() {
		_ = tmp.Close()
		if retErr != nil {
			_ = root.Remove(transactionName)
		}
	}()
	if _, err := tmp.Write(data); err != nil {
		return fmt.Errorf("write watchdog transaction: %w", err)
	}
	if err := tmp.Sync(); err != nil {
		return fmt.Errorf("sync watchdog transaction: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("close watchdog transaction: %w", err)
	}
	if err := root.Rename(transactionName, name); err != nil {
		return fmt.Errorf("commit watchdog transaction: %w", err)
	}
	return syncRoot(root)
}

func syncRoot(root *os.Root) error {
	f, err := root.Open(".")
	if err != nil {
		return fmt.Errorf("open watchdog directory for sync: %w", err)
	}
	defer func() { _ = f.Close() }()
	if err := f.Sync(); err != nil {
		return fmt.Errorf("sync watchdog directory: %w", err)
	}
	return nil
}

// Prune enforces age and byte retention even while the decoder logs are quiet.
func (s *Store) Prune(now time.Time) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.trimEvidence(now, 0)
}
