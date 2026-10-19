package decodermonitor

import (
	"context"
	"errors"
	"net/http"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"
)

type readinessProbe struct {
	unavailable atomic.Bool
	reads       uint64
}

func (p *readinessProbe) Read(_ context.Context, _ Bookmark) (Probe, error) {
	p.reads++
	probe := *healthyProbe(time.Now(), p.reads*1920000)
	probe.CaughtUp = !p.unavailable.Load()
	return probe, nil
}

type joiningSender struct{ entered, exited chan struct{} }

func (s joiningSender) SendMail(ctx context.Context, _, _ string) error {
	close(s.entered)
	<-ctx.Done()
	// Make worker cleanup observable separately from cancellation.
	time.Sleep(time.Second)
	close(s.exited)
	return ctx.Err()
}

// Not parallel: the heartbeat client uses the process-wide default transport.
func TestRunnerRunHeartbeatCadenceAndMailWorkerJoin(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		cfg, store := testStore(t)
		cfg.HeartbeatURL = "https://heartbeat.example.invalid"
		cfg.SendMail, cfg.UnknownSeconds = true, 3600
		probe := &readinessProbe{}
		sender := joiningSender{entered: make(chan struct{}), exited: make(chan struct{})}
		r := Runner{Config: cfg, Store: store, Probe: probe, Sender: sender}
		pinged := make(chan time.Time, 32)
		previous := http.DefaultTransport
		http.DefaultTransport = roundTripFunc(func(*http.Request) (*http.Response, error) {
			pinged <- time.Now()
			if !r.ready(time.Now()) {
				t.Error("heartbeat sent without current observations")
			}
			return mailResponse(http.StatusNoContent, ""), nil
		})
		defer func() { http.DefaultTransport = previous }()
		ctx, cancel := context.WithCancel(t.Context())
		defer cancel()
		done := make(chan error, 1)
		go func() { done <- r.Run(ctx) }()
		checkPings := func(want int) {
			t.Helper()
			synctest.Wait()
			if got := len(pinged); got != want {
				t.Fatalf("heartbeat count = %d, want %d at %s", got, want, time.Now())
			}
		}
		start := time.Now()
		checkPings(0) // The first playback sample only establishes a baseline.
		time.Sleep(59 * time.Second)
		checkPings(0)
		time.Sleep(time.Second)
		checkPings(1)
		time.Sleep(59 * time.Second)
		checkPings(1)
		time.Sleep(time.Second)
		checkPings(2)
		probe.unavailable.Store(true)
		time.Sleep(time.Minute)
		checkPings(2)
		probe.unavailable.Store(false)
		time.Sleep(time.Minute)
		checkPings(3)
		for i, want := range []time.Duration{time.Minute, 2 * time.Minute, 4 * time.Minute} {
			if got := (<-pinged).Sub(start); got != want {
				t.Errorf("heartbeat %d at %s, want %s", i, got, want)
			}
		}
		// Use synthetic time for queued mail; real file mtimes are irrelevant.
		if err := store.Update(func(s *State) error {
			s.Outbox = []Mail{{ID: 1, Created: time.Now(), Subject: "queued"}}
			return nil
		}); err != nil {
			t.Fatal(err)
		}
		time.Sleep(2 * time.Second)
		<-sender.entered
		cancel()
		synctest.Wait()
		select {
		case err := <-done:
			t.Errorf("Run returned before mail worker cleanup: %v", err)
			time.Sleep(time.Second)
			synctest.Wait()
			return
		default:
		}
		time.Sleep(time.Second)
		if err := <-done; err != nil && !errors.Is(err, context.Canceled) {
			t.Fatal(err)
		}
		select {
		case <-sender.exited:
		default:
			t.Fatal("Run returned before the mail sender exited")
		}
	})
}
