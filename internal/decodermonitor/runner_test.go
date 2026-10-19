package decodermonitor

import (
	"context"
	"errors"
	"fmt"
	"log"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"testing/synctest"
	"time"
)

type fakeSender struct {
	err     error
	calls   int
	subject string
}

func (f *fakeSender) SendMail(_ context.Context, subject, _ string) error {
	f.calls++
	f.subject = subject
	return f.err
}

func TestMailFailureRetryAndRestart(t *testing.T) {
	t.Parallel()
	cfg, store := testStore(t)
	cfg.SendMail = true
	at := time.Now()
	if err := store.Update(nil, at, func(s *State) error {
		s.Outbox = []Mail{{ID: 1, Subject: "queued", Created: at.Add(-time.Hour)}}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	sender := &fakeSender{err: errors.New("sensitive OAuth response must not be persisted")}
	r := Runner{Config: cfg, Store: store, Sender: sender}
	if err := r.deliver(t.Context(), at); err != nil {
		t.Fatal(err)
	}
	s, err := store.Snapshot()
	if err != nil || len(s.Outbox) != 1 || s.Outbox[0].Attempts != 1 || !s.Outbox[0].NextAttempt.After(at) {
		t.Fatalf("failed mail not retained: %+v %v", s, err)
	}
	if !strings.HasPrefix(sender.subject, "[DELAYED]") {
		t.Fatal("old queued mail represented as current")
	}
	if err := r.deliver(t.Context(), at); err != nil {
		t.Fatal(err)
	}
	if sender.calls != 1 {
		t.Fatal("retry ignored backoff")
	}
	sender.err = nil
	if err := r.deliver(t.Context(), at.Add(time.Minute)); err != nil {
		t.Fatal(err)
	}
	s, err = store.Snapshot()
	if err != nil || len(s.Outbox) != 0 || sender.calls != 2 {
		t.Fatalf("accepted mail not acknowledged: %+v %v", s, err)
	}
}

func TestPreviewNeverSendsAndDisabledDeliveryPreservesQueue(t *testing.T) {
	t.Parallel()
	cfg, store := testStore(t)
	at := time.Now()
	sender := &fakeSender{}
	r := Runner{Config: cfg, Store: store, Sender: sender}
	if err := store.Update(nil, at, func(s *State) error { s.Outbox = []Mail{{ID: 1, Preview: true}, {ID: 2}}; return nil }); err != nil {
		t.Fatal(err)
	}
	if err := r.deliver(t.Context(), at); err != nil {
		t.Fatal(err)
	}
	if err := r.deliver(t.Context(), at); err != nil {
		t.Fatal(err)
	}
	s, err := store.Snapshot()
	if err != nil || sender.calls != 0 || len(s.Outbox) != 1 || s.Outbox[0].ID != 2 {
		t.Fatalf("preview sent or real queued mail discarded: %+v %v", s, err)
	}
}

type canceledProbe struct{ entered chan struct{} }

func (p canceledProbe) Read(ctx context.Context, _ Bookmark) (Probe, error) {
	close(p.entered)
	<-ctx.Done()
	return Probe{}, ctx.Err()
}

func TestRunnerJoinsWorkersOnCancellation(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		cfg, store := testStore(t)
		entered := make(chan struct{})
		r := Runner{Config: cfg, Store: store, Probe: canceledProbe{entered}}
		ctx, cancel := context.WithCancel(t.Context())
		done := make(chan error, 1)
		go func() { done <- r.Run(ctx) }()
		<-entered
		cancel()
		if err := <-done; err != nil && !errors.Is(err, context.Canceled) {
			t.Fatal(err)
		}
		synctest.Wait()
	})
}

func TestPreviewsDrainBehindRetainedRealMail(t *testing.T) {
	t.Parallel()
	cfg, store := testStore(t)
	cfg.HeartbeatURL = "https://heartbeat.example.invalid"
	at := time.Now()
	sender := &fakeSender{}
	r := Runner{Config: cfg, Store: store, Sender: sender}
	if err := store.Update(nil, at, func(s *State) error {
		s.LastObserved = at
		s.Outbox = []Mail{{ID: 1, Created: at.Add(-time.Hour)}, {ID: 2, Preview: true, Created: at.Add(-time.Hour)}}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if err := r.deliver(t.Context(), at); err != nil {
		t.Fatal(err)
	}
	s, err := store.Snapshot()
	if err != nil || sender.calls != 0 || len(s.Outbox) != 1 || s.Outbox[0].ID != 1 {
		t.Fatalf("preview blocked or real mail sent/discarded: %+v %v", s.Outbox, err)
	}
	if !r.ready(at) {
		t.Fatal("retained real mail stopped the heartbeat while sending is off")
	}
	r.Config.SendMail = true
	if r.ready(at) {
		t.Fatal("stuck deliverable mail did not stop the heartbeat")
	}
}

func TestReadyRequiresCurrentObservation(t *testing.T) {
	t.Parallel()
	cfg, store := testStore(t)
	at := time.Now()
	r := Runner{Config: cfg, Store: store}
	for _, tc := range []struct {
		name  string
		state func(*State)
		want  bool
	}{
		{"current", func(s *State) { s.LastObserved = at }, true},
		{"never_observed", func(s *State) {}, false},
		{"stale", func(s *State) { s.LastObserved = at.Add(-time.Minute) }, false},
		{"unknown", func(s *State) { s.LastObserved, s.UnknownSince = at, at }, false},
		{"fresh_mail", func(s *State) { s.LastObserved, s.Outbox = at, []Mail{{ID: 1, Preview: true, Created: at}} }, true},
	} {
		if err := store.Update(nil, at, func(s *State) error { *s = State{Version: 1}; tc.state(s); return nil }); err != nil {
			t.Fatal(err)
		}
		if got := r.ready(at); got != tc.want {
			t.Errorf("%s: ready=%v, want %v", tc.name, got, tc.want)
		}
	}
	if err := store.Update(nil, at, func(s *State) error { *s = State{Version: 1, LastObserved: at}; return nil }); err != nil {
		t.Fatal(err)
	}
	r.blocked = true
	if r.ready(at) {
		t.Error("heartbeat allowed right after a poll that could not be committed")
	}
}

type shutdownSender struct{ cancel context.CancelFunc }

func (s shutdownSender) SendMail(ctx context.Context, _, _ string) error {
	s.cancel()
	<-ctx.Done()
	return ctx.Err()
}

func TestShutdownDuringSendIsNotAFailedAttempt(t *testing.T) {
	t.Parallel()
	cfg, store := testStore(t)
	cfg.SendMail = true
	at := time.Now()
	if err := store.Update(nil, at, func(s *State) error { s.Outbox = []Mail{{ID: 1, Created: at}}; return nil }); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	r := Runner{Config: cfg, Store: store, Sender: shutdownSender{cancel}}
	if err := r.deliver(ctx, at); err != nil {
		t.Fatal(err)
	}
	s, err := store.Snapshot()
	if err != nil || len(s.Outbox) != 1 || s.Outbox[0].Attempts != 0 || !s.Outbox[0].NextAttempt.IsZero() {
		t.Fatalf("shutdown counted as a delivery failure: %+v %v", s.Outbox, err)
	}
}

type scriptedProbe struct{ seen []Bookmark }

func (p *scriptedProbe) Read(_ context.Context, bookmark Bookmark) (Probe, error) {
	p.seen = append(p.seen, bookmark)
	at := time.Now()
	probe := *healthyProbe(at, uint64(len(p.seen))*1920000)
	record := Record{TimeUS: at.UnixMicro(), Message: "Buffer filling", Cursor: fmt.Sprint("c", len(p.seen)), Key: fmt.Sprint("k", len(p.seen)), BootID: "boot-a"}
	probe.Records, probe.Bookmark = []Record{record}, Bookmark{Cursor: record.Cursor, Key: record.Key}
	return probe, nil
}

func TestPollCommitsEvidenceAndAdvancesTheBookmark(t *testing.T) {
	t.Parallel()
	cfg, store := testStore(t)
	probe := &scriptedProbe{}
	r := Runner{Config: cfg, Store: store, Probe: probe}
	for range 3 {
		if err := r.poll(t.Context()); err != nil {
			t.Fatal(err)
		}
	}
	if len(probe.seen) != 3 || probe.seen[0] != (Bookmark{}) || probe.seen[2] != (Bookmark{Cursor: "c2", Key: "k2"}) {
		t.Fatalf("poll did not resume from the committed bookmark: %+v", probe.seen)
	}
	entries, err := os.ReadDir(filepath.Join(cfg.StateDir, "evidence"))
	if err != nil || len(entries) != 3 {
		t.Fatalf("records were not committed as evidence: %d %v", len(entries), err)
	}
}

type faultProbe struct {
	reads  int
	cancel context.CancelFunc
}

func (p *faultProbe) Read(_ context.Context, _ Bookmark) (Probe, error) {
	p.reads++
	if p.reads == 3 {
		p.cancel()
	}
	probe := *healthyProbe(time.Now(), 0)
	probe.Active = "inactive"
	return probe, nil
}

func TestFullOutboxDoesNotStopObservation(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		cfg, store := testStore(t)
		cfg.SendMail = true
		at := time.Now()
		if err := store.Update(nil, at, func(s *State) error {
			for i := range maxOutbox {
				s.Outbox = append(s.Outbox, Mail{ID: uint64(i) + 1, Created: at, NextAttempt: at.Add(time.Hour)})
			}
			return nil
		}); err != nil {
			t.Fatal(err)
		}
		ctx, cancel := context.WithCancel(t.Context())
		probe := &faultProbe{cancel: cancel}
		r := Runner{Config: cfg, Store: store, Probe: probe, Sender: &fakeSender{}}
		if err := r.Run(ctx); err != nil && !errors.Is(err, context.Canceled) {
			t.Fatalf("full outbox ended observation: %v", err)
		}
		if r.ready(time.Now()) {
			t.Fatal("heartbeat allowed while observations cannot be committed")
		}
		s, err := store.Snapshot()
		if err != nil || probe.reads != 3 || s.Incident.ID != 0 || len(s.Outbox) != maxOutbox {
			t.Fatalf("full outbox committed or dropped state: reads=%d incident=%+v outbox=%d %v", probe.reads, s.Incident, len(s.Outbox), err)
		}
	})
}

type gapProbe struct{ inactive bool }

func (p *gapProbe) Read(_ context.Context, _ Bookmark) (Probe, error) {
	probe := *healthyProbe(time.Now(), 0)
	probe.Gap = true
	if p.inactive {
		probe.Active = "inactive"
	}
	return probe, nil
}

// Not parallel: it captures the process-wide default logger.
func TestJournalGapIsLoggedWhenItsObservationCommits(t *testing.T) {
	var logs strings.Builder
	previous, writer, flags := slog.Default(), log.Writer(), log.Flags()
	slog.SetDefault(slog.New(slog.NewTextHandler(&logs, nil)))
	t.Cleanup(func() {
		// slog.SetDefault also redirects the standard logger.
		slog.SetDefault(previous)
		log.SetOutput(writer)
		log.SetFlags(flags)
	})
	cfg, store := testStore(t)
	at := time.Now()
	if err := store.Update(nil, at, func(s *State) error {
		for i := range maxOutbox {
			s.Outbox = append(s.Outbox, Mail{ID: uint64(i) + 1, Preview: true, Created: at})
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	probe := &gapProbe{inactive: true}
	r := Runner{Config: cfg, Store: store, Probe: probe}
	if err := r.poll(t.Context()); !errors.Is(err, errOutboxFull) {
		t.Fatalf("expected a rejected poll: %v", err)
	}
	if strings.Contains(logs.String(), "continuity lost") {
		t.Fatal("gap logged for an observation that was not committed")
	}
	if err := store.Update(nil, at, func(s *State) error { s.Outbox = nil; return nil }); err != nil {
		t.Fatal(err)
	}
	probe.inactive = false
	r.blocked = true // as Run leaves it after the rejected poll
	if err := r.poll(t.Context()); err != nil {
		t.Fatal(err)
	}
	if got := strings.Count(logs.String(), "continuity lost"); got != 1 {
		t.Fatalf("committed gap logged %d times", got)
	}
}
