package decodermonitor

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"slices"
	"sync"
)

// transactionName is the in-progress state.json write. The exclusive
// lock and mu make the watchdog its only writer, so a leftover from a crash is
// removed and recreated.
const transactionName = ".watchdog.tmp"

// Store commits cursor, incident and outbox together, using fsync + atomic rename.
type Store struct {
	mu     sync.Mutex
	root   *os.Root
	lock   *os.File
	loaded bool   // false after a failed write: disk is authoritative again
	state  State  // last committed state, including fields kept only in memory
	saved  []byte // durably committed encoding of state; nil forces a rewrite
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
	s := &Store{root: root}
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
func (s *Store) Update(change func(*State) error) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	state, err := s.current()
	if err != nil {
		return err
	}
	if err := change(&state); err != nil {
		return err
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
