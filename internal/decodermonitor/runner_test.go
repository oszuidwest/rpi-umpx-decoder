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
	// The first fault is unknown and commits, so its gap is logged once.
	if err := r.poll(t.Context()); err != nil {
		t.Fatal(err)
	}
	checkLogCount(t, &logs, "continuity lost", 1)
	if err := r.poll(t.Context()); !errors.Is(err, errOutboxFull) {
		t.Fatalf("expected a rejected poll: %v", err)
	}
	checkLogCount(t, &logs, "continuity lost", 1)
	if err := store.Update(nil, at, func(s *State) error { s.Outbox = nil; return nil }); err != nil {
		t.Fatal(err)
	}
	probe.inactive = false
	r.blocked = true // as Run leaves it after the rejected poll
	if err := r.poll(t.Context()); err != nil {
		t.Fatal(err)
	}
	// The unchanged reason is not repeated once observations commit again.
	checkLogCount(t, &logs, "continuity lost", 1)
}

type unavailableProbe struct {
	probe Probe
	err   error
}

func (p *unavailableProbe) Read(context.Context, Bookmark) (Probe, error) { return p.probe, p.err }

// Not parallel: it captures the process-wide default logger.
func TestUnknownReasonLoggedAfterCommitAndRestart(t *testing.T) {
	var logs strings.Builder
	previous, writer, flags := slog.Default(), log.Writer(), log.Flags()
	slog.SetDefault(slog.New(slog.NewTextHandler(&logs, nil)))
	t.Cleanup(func() {
		slog.SetDefault(previous)
		log.SetOutput(writer)
		log.SetFlags(flags)
	})
	for _, failed := range []bool{false, true} {
		t.Run(fmt.Sprint("probe_read_error=", failed), func(t *testing.T) {
			logs.Reset()
			cfg, store := testStore(t)
			probe := &unavailableProbe{probe: *healthyProbe(time.Now(), 0)}
			setReason := func(cause string) string {
				if failed {
					probe.err = errors.New(cause + "\r\nunwanted diagnostic line")
					return "probe read failed: " + cause
				}
				probe.probe.Active, probe.probe.StatusErr = "unknown", errors.New(cause+"\r\nunwanted diagnostic line")
				return "decoder service status unavailable: " + cause
			}
			reason := setReason("permission denied")
			// Persisted uncertainty must not suppress the first log after startup.
			if err := store.Update(nil, time.Now(), func(s *State) error { s.UnknownNoticed = true; return nil }); err != nil {
				t.Fatal(err)
			}
			r := Runner{Config: cfg, Store: store, Probe: probe}
			poll := func(want string, count int) {
				t.Helper()
				if err := r.poll(t.Context()); err != nil {
					t.Fatal(err)
				}
				if got := strings.Count(logs.String(), "level=WARN msg=\"watchdog decoder observation unavailable\""); got != count || !strings.Contains(logs.String(), want) || strings.Contains(logs.String(), "unwanted diagnostic line") {
					t.Fatalf("reason log count=%d, want %d containing %q: %s", got, count, want, logs.String())
				}
			}
			poll(reason, 1)
			poll(reason, 1)
			reason = setReason("read timed out")
			poll(reason, 2)
			poll(reason, 2)
			at := time.Now()
			if err := store.Update(nil, at, func(s *State) error {
				s.UnknownNoticed, s.UnknownSince = false, at.Add(-time.Hour)
				for i := range maxOutbox {
					s.Outbox = append(s.Outbox, Mail{ID: uint64(i) + 1})
				}
				return nil
			}); err != nil {
				t.Fatal(err)
			}
			reason = setReason("kernel status unavailable")
			before := logs.String()
			if err := r.poll(t.Context()); !errors.Is(err, errOutboxFull) {
				t.Fatalf("expected rejected poll: %v", err)
			}
			if logs.String() != before {
				t.Fatal("rejected observation logged a reason")
			}
			if err := store.Update(nil, at, func(s *State) error { s.Outbox = nil; return nil }); err != nil {
				t.Fatal(err)
			}
			poll(reason, 3)
			s, err := store.Snapshot()
			if err != nil || len(s.Outbox) != 1 || !strings.HasPrefix(s.Outbox[0].Subject, "[UNKNOWN]") || !strings.Contains(s.Outbox[0].Body, "Reason: "+reason+"\n") || strings.Contains(s.Outbox[0].Body, "unwanted diagnostic line") {
				t.Fatalf("committed reason absent from unknown mail: %+v %v", s.Outbox, err)
			}
			if err := store.Close(); err != nil {
				t.Fatal(err)
			}
			reopened, err := OpenStore(&cfg)
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = reopened.Close() })
			r = Runner{Config: cfg, Store: reopened, Probe: probe}
			poll(reason, 4)
			poll(reason, 4)
			// Complete observations clear deduplication, allowing a recurrence.
			at = time.Now()
			probe.err, probe.probe = nil, *healthyProbe(at, 0)
			probe.probe.PCM.Trigger = "resumed"
			// A fresh baseline is routine: logged once per baseline at info level.
			poll("playback baseline", 4)
			probe.probe = *healthyProbe(at.Add(time.Second), 1920000)
			probe.probe.PCM.Trigger = "resumed"
			poll("playback baseline", 4)
			probe.probe.PCM.Trigger = "reopened"
			poll("playback baseline", 4)
			checkLogCount(t, &logs, "level=INFO msg=\"watchdog establishing playback baseline\"", 2)
			reason = setReason("kernel status unavailable")
			poll(reason, 5)
		})
	}
}

func checkLogCount(t *testing.T, logs *strings.Builder, line string, want int) {
	t.Helper()
	if got := strings.Count(logs.String(), line); got != want {
		t.Fatalf("%d log lines %q, want %d: %s", got, line, want, logs.String())
	}
}
