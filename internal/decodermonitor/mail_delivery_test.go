package decodermonitor

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"testing/synctest"
	"time"
)

func TestPermanentMailRejectionDeadLettersAndRestart(t *testing.T) {
	for _, endpoint := range []string{"token", "sendMail"} {
		for _, status := range []int{400, 401, 403, 404} {
			t.Run(fmt.Sprintf("%s/%d", endpoint, status), func(t *testing.T) {
				t.Parallel()
				cfg, store := testStore(t)
				cfg.SendMail = true
				at := time.Now()
				mail := Mail{ID: 1, Subject: "rejected", Body: "incident details", IncidentID: 7, Created: at}
				if err := store.Update(nil, at, func(s *State) error {
					s.Outbox = []Mail{mail, {ID: 2, Subject: "next", Created: at}}
					return nil
				}); err != nil {
					t.Fatal(err)
				}
				config := testEmail()
				client := newGraphSender(&config)
				reject := true
				accepted := 0
				client.client.Transport = roundTripFunc(func(r *http.Request) (*http.Response, error) {
					if r.URL.String() == client.tokenURL {
						if reject && endpoint == "token" {
							return mailResponse(status, "sensitive upstream diagnostic"), nil
						}
						return mailResponse(200, `{"access_token":"fixture-token","token_type":"Bearer","expires_in":3600}`), nil
					}
					if reject {
						return mailResponse(status, "sensitive upstream diagnostic"), nil
					}
					var payload struct {
						Message graphMessage `json:"message"`
					}
					if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
						t.Fatal(err)
					}
					if payload.Message.Subject != "next" {
						t.Fatalf("unexpected next mail: %q", payload.Message.Subject)
					}
					accepted++
					return mailResponse(http.StatusAccepted, ""), nil
				})
				runner := Runner{Config: cfg, Store: store, Sender: client}
				at = rejectMailFiveTimes(t, &runner, at)
				restartMailRunner(t, &runner)
				checkDeadLetter(t, &runner, &mail, status, at)
				reject = false
				if err := runner.deliver(t.Context(), at); err != nil {
					t.Fatal(err)
				}
				if !runner.ready(at) || accepted != 1 {
					t.Fatal("next mail acceptance did not restore heartbeat")
				}
				restartMailRunner(t, &runner)
				store = runner.Store
				s, err := store.Snapshot()
				if err != nil || len(s.Outbox) != 0 || len(s.DeadLetters) != 1 || s.MailCredentialFailed {
					t.Fatalf("acceptance not durable: %+v %v", s, err)
				}
			})
		}
	}
}

func restartMailRunner(t *testing.T, r *Runner) {
	t.Helper()
	if err := r.Store.Close(); err != nil {
		t.Fatal(err)
	}
	reopened, err := OpenStore(&r.Config)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = reopened.Close() })
	r.Store = reopened
}

func rejectMailFiveTimes(t *testing.T, r *Runner, at time.Time) time.Time {
	t.Helper()
	for attempt := 1; attempt <= 5; attempt++ {
		if err := r.deliver(t.Context(), at); err != nil {
			t.Fatal(err)
		}
		s, err := r.Store.Snapshot()
		if err != nil {
			t.Fatal(err)
		}
		if attempt < 5 {
			if len(s.Outbox) != 2 || s.Outbox[0].Attempts != attempt || len(s.DeadLetters) != 0 || s.MailCredentialFailed {
				t.Fatalf("premature dead letter on attempt %d: %+v", attempt, s)
			}
			at = s.Outbox[0].NextAttempt
		}
		if attempt == 4 {
			restartMailRunner(t, r) // The retry count must survive a restart too.
		}
	}
	return at
}

func checkDeadLetter(t *testing.T, r *Runner, mail *Mail, status int, at time.Time) {
	t.Helper()
	s, err := r.Store.Snapshot()
	if err != nil || len(s.Outbox) != 1 || s.Outbox[0].ID != 2 || len(s.DeadLetters) != 1 {
		t.Fatalf("dead letter or next mail lost on restart: %+v %v", s, err)
	}
	d := s.DeadLetters[0]
	if d.ID != mail.ID || d.Subject != mail.Subject || d.Body != mail.Body || d.IncidentID != mail.IncidentID ||
		d.Attempts != 5 || d.Status != status || !d.Created.Equal(mail.Created) || !d.Failed.Equal(at) {
		t.Fatalf("incomplete dead letter: %+v", d)
	}
	withheld := status == 401 || status == 403
	if s.MailCredentialFailed != withheld {
		t.Fatalf("credential failure = %v, want %v", s.MailCredentialFailed, withheld)
	}
	// A restart requires fresh observations before any heartbeat.
	if err := r.Store.Update(nil, at, func(s *State) error { s.LastObserved = at; return nil }); err != nil {
		t.Fatal(err)
	}
	if r.ready(at) == withheld {
		t.Fatalf("ready = %v after HTTP %d dead letter", r.ready(at), status)
	}
}

func TestTransientMailFailuresNeverDeadLetter(t *testing.T) {
	for _, tc := range []struct {
		name string
		err  error
	}{
		{"408", &deliveryError{status: 408}},
		{"429", &deliveryError{status: 429}},
		{"500", &deliveryError{status: 500}},
		{"503", &deliveryError{status: 503}},
		{"transport", errors.New("M365 request failed")},
		{"timeout", context.DeadlineExceeded},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			cfg, store := testStore(t)
			cfg.SendMail = true
			at := time.Now()
			if err := store.Update(nil, at, func(s *State) error {
				s.Outbox = []Mail{{ID: 1, Created: at}, {ID: 2, Created: at}}
				return nil
			}); err != nil {
				t.Fatal(err)
			}
			sender := &fakeSender{err: tc.err}
			r := Runner{Config: cfg, Store: store, Sender: sender}
			for attempt := 1; attempt <= 20; attempt++ {
				if err := r.deliver(t.Context(), at); err != nil {
					t.Fatal(err)
				}
				s, err := store.Snapshot()
				if err != nil || len(s.Outbox) != 2 || s.Outbox[0].ID != 1 || s.Outbox[0].Attempts != attempt ||
					len(s.DeadLetters) != 0 || s.MailCredentialFailed || sender.calls != attempt {
					t.Fatalf("transient failure discarded mail on attempt %d: %+v %v", attempt, s, err)
				}
				at = s.Outbox[0].NextAttempt
			}
		})
	}
}

func TestMailDeadLetterUsesLatestFailure(t *testing.T) {
	t.Parallel()
	cfg, store := testStore(t)
	cfg.SendMail = true
	at := time.Now()
	if err := store.Update(nil, at, func(s *State) error {
		s.Outbox = []Mail{{ID: 1, Attempts: 4, Created: at}}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	sender := &fakeSender{err: &deliveryError{status: 503}}
	r := Runner{Config: cfg, Store: store, Sender: sender}
	if err := r.deliver(t.Context(), at); err != nil {
		t.Fatal(err)
	}
	s, err := store.Snapshot()
	if err != nil || len(s.Outbox) != 1 || s.Outbox[0].Attempts != 5 || len(s.DeadLetters) != 0 {
		t.Fatalf("latest transient failure dead-lettered: %+v %v", s, err)
	}
	sender.err = fmt.Errorf("token: %w", &deliveryError{status: 404})
	if err := r.deliver(t.Context(), s.Outbox[0].NextAttempt); err != nil {
		t.Fatal(err)
	}
	s, err = store.Snapshot()
	if err != nil || len(s.Outbox) != 0 || len(s.DeadLetters) != 1 || s.DeadLetters[0].Attempts != 6 || s.MailCredentialFailed {
		t.Fatalf("latest permanent failure after threshold not dead-lettered: %+v %v", s, err)
	}
}

func TestPreviewAndFailedSendKeepCredentialFailure(t *testing.T) {
	t.Parallel()
	cfg, store := testStore(t)
	at := time.Now()
	if err := store.Update(nil, at, func(s *State) error {
		s.LastObserved, s.MailCredentialFailed = at, true
		s.Outbox = []Mail{{ID: 1, Preview: true, Created: at}, {ID: 2, Created: at}}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	sender := &fakeSender{err: &deliveryError{status: 404}}
	r := Runner{Config: cfg, Store: store, Sender: sender}
	if err := r.deliver(t.Context(), at); err != nil {
		t.Fatal(err)
	}
	if r.ready(at) || sender.calls != 0 {
		t.Fatal("preview cleared the credential failure or sent real mail")
	}
	r.Config.SendMail = true
	for range 5 {
		if err := r.deliver(t.Context(), at); err != nil {
			t.Fatal(err)
		}
		s, err := store.Snapshot()
		if err != nil || !s.MailCredentialFailed || r.ready(at) {
			t.Fatalf("failed send cleared credential failure: %+v %v", s, err)
		}
		if len(s.Outbox) > 0 {
			at = s.Outbox[0].NextAttempt
		}
	}
	s, err := store.Snapshot()
	if err != nil || len(s.Outbox) != 0 || len(s.DeadLetters) != 1 || !s.MailCredentialFailed {
		t.Fatalf("404 dead letter cleared an earlier credential failure: %+v %v", s, err)
	}
}

func TestDeadLetterBoundAndSnapshotIsolation(t *testing.T) {
	t.Parallel()
	cfg, store := testStore(t)
	cfg.SendMail = true
	at := time.Now()
	if err := store.Update(nil, at, func(s *State) error {
		for i := range 34 {
			s.Outbox = append(s.Outbox, Mail{ID: uint64(i + 1), Attempts: 4, Created: at})
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	r := Runner{Config: cfg, Store: store, Sender: &fakeSender{err: &deliveryError{status: 400}}}
	for range 34 {
		if err := r.deliver(t.Context(), at); err != nil {
			t.Fatal(err)
		}
	}
	s, err := store.Snapshot()
	if err != nil || len(s.Outbox) != 0 || len(s.DeadLetters) != 32 {
		t.Fatalf("dead-letter bound not applied: %+v %v", s, err)
	}
	for i, d := range s.DeadLetters {
		if d.ID != uint64(i+3) {
			t.Fatalf("oldest dead letters were not dropped: %+v", s.DeadLetters)
		}
	}
	s.DeadLetters[0].Subject = "outside mutation"
	sentinel := errors.New("abort")
	if err := store.Update(nil, at, func(s *State) error {
		s.DeadLetters[0].Attempts = 99
		return sentinel
	}); !errors.Is(err, sentinel) {
		t.Fatal(err)
	}
	s, err = store.Snapshot()
	if err != nil || s.DeadLetters[0].Subject != "" || s.DeadLetters[0].Attempts != 5 {
		t.Fatalf("dead letters mutated outside a commit: %+v %v", s.DeadLetters, err)
	}
}

// Not parallel: it captures the process-wide default logger.
func TestDeadLetterLogRequiresCommit(t *testing.T) {
	var logs strings.Builder
	previous, writer, flags := slog.Default(), log.Writer(), log.Flags()
	slog.SetDefault(slog.New(slog.NewTextHandler(&logs, nil)))
	t.Cleanup(func() {
		slog.SetDefault(previous)
		log.SetOutput(writer)
		log.SetFlags(flags)
	})
	cfg, store := testStore(t)
	cfg.SendMail = true
	at := time.Now()
	if err := store.Update(nil, at, func(s *State) error {
		s.Outbox = []Mail{{ID: 7, Subject: "rejected", Attempts: 4, Created: at}}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	// A nonempty transaction directory makes atomicWrite fail before rename.
	path := filepath.Join(cfg.StateDir, transactionName)
	if err := os.Mkdir(path, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(path, "keep"), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	r := Runner{Config: cfg, Store: store, Sender: &fakeSender{err: &deliveryError{status: 403}}}
	if err := r.deliver(t.Context(), at); err == nil {
		t.Fatal("failed dead-letter commit was acknowledged")
	}
	if strings.Contains(logs.String(), "dead-lettered") {
		t.Fatal("uncommitted dead letter was logged")
	}
	s, err := store.Snapshot()
	if err != nil || len(s.Outbox) != 1 || s.Outbox[0].Attempts != 4 || len(s.DeadLetters) != 0 || s.MailCredentialFailed {
		t.Fatalf("failed dead-letter commit lost mail: %+v %v", s, err)
	}
	if err := os.RemoveAll(path); err != nil {
		t.Fatal(err)
	}
	if err := r.deliver(t.Context(), at); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(logs.String(), `level=ERROR msg="watchdog M365 mail dead-lettered" notification_id=7 subject=rejected status=403 attempts=5`) {
		t.Fatalf("missing dead-letter error log: %s", logs.String())
	}
}

func TestStoreRejectsOversizedDeadLetters(t *testing.T) {
	t.Parallel()
	cfg, store := testStore(t)
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	data, err := json.Marshal(State{Version: 1, DeadLetters: make([]DeadLetter, 33)})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(cfg.StateDir, "state.json"), data, 0o600); err != nil {
		t.Fatal(err)
	}
	if reopened, err := OpenStore(&cfg); err == nil {
		_ = reopened.Close()
		t.Fatal("oversized dead-letter history accepted")
	}
}

func TestMailCheckIsQueuedOnceWhileCredentialFailureIsLatched(t *testing.T) {
	t.Parallel()
	cfg := DefaultConfig()
	cfg.Site, cfg.SendMail = "Test", true
	e, at := Engine{cfg}, time.Now()
	for _, tc := range []struct {
		name     string
		state    State
		sendMail bool
		want     int
	}{
		{"latched", State{Version: 1, MailCredentialFailed: true}, true, 1},
		{"not_latched", State{Version: 1}, true, 0},
		{"mail_disabled", State{Version: 1, MailCredentialFailed: true}, false, 0},
		{"real_mail_queued", State{Version: 1, MailCredentialFailed: true, Outbox: []Mail{{ID: 7}}}, true, 1},
		{"only_preview_queued", State{Version: 1, MailCredentialFailed: true, Outbox: []Mail{{ID: 7, Preview: true}}}, true, 2},
	} {
		e.Config.SendMail = tc.sendMail
		s := tc.state
		for range 2 {
			if err := e.MailCheck(&s, at); err != nil {
				t.Fatal(err)
			}
		}
		if len(s.Outbox) != tc.want {
			t.Errorf("%s: outbox=%d, want %d", tc.name, len(s.Outbox), tc.want)
		}
		if tc.want > len(tc.state.Outbox) && (!strings.HasPrefix(s.Outbox[len(s.Outbox)-1].Subject, "[MAIL CHECK]") || s.Outbox[len(s.Outbox)-1].Preview) {
			t.Errorf("%s: no real mail check queued: %+v", tc.name, s.Outbox)
		}
	}
}

func TestRunQueuesMailCheckSoTheLatchCanClear(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		cfg, store := testStore(t)
		cfg.SendMail = true
		if err := store.Update(nil, time.Now(), func(s *State) error { s.MailCredentialFailed = true; return nil }); err != nil {
			t.Fatal(err)
		}
		sender := &fakeSender{}
		entered := make(chan struct{})
		r := Runner{Config: cfg, Store: store, Probe: canceledProbe{entered}, Sender: sender}
		ctx, cancel := context.WithCancel(t.Context())
		done := make(chan error, 1)
		go func() { done <- r.Run(ctx) }()
		<-entered
		time.Sleep(time.Second)
		synctest.Wait()
		cancel()
		if err := <-done; err != nil && !errors.Is(err, context.Canceled) {
			t.Fatal(err)
		}
		s, err := store.Snapshot()
		if err != nil || sender.calls != 1 || !strings.HasPrefix(sender.subject, "[MAIL CHECK]") || s.MailCredentialFailed {
			t.Fatalf("mail check not sent or latch not cleared: calls=%d subject=%q state=%+v %v", sender.calls, sender.subject, s, err)
		}
	})
}

type healthyRunProbe struct{ polls uint64 }

func (p *healthyRunProbe) Read(context.Context, Bookmark) (Probe, error) {
	p.polls++
	return *healthyProbe(time.Now(), p.polls*1920000), nil
}

func TestMailCheckWaitsForAFullOutboxToDrain(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		cfg, store := testStore(t)
		cfg.SendMail = true
		if err := store.Update(nil, time.Now(), func(s *State) error {
			s.MailCredentialFailed = true
			for i := range maxOutbox {
				s.Outbox = append(s.Outbox, Mail{ID: uint64(i) + 1, Preview: true, Created: time.Now()})
			}
			s.NextID = maxOutbox
			return nil
		}); err != nil {
			t.Fatal(err)
		}
		sender := &fakeSender{}
		r := Runner{Config: cfg, Store: store, Probe: &healthyRunProbe{}, Sender: sender}
		ctx, cancel := context.WithCancel(t.Context())
		done := make(chan error, 1)
		go func() { done <- r.Run(ctx) }()
		// Previews drain one per mail tick; the check is queued afterwards.
		time.Sleep(time.Duration(maxOutbox+10) * 2 * time.Second)
		synctest.Wait()
		cancel()
		if err := <-done; err != nil && !errors.Is(err, context.Canceled) {
			t.Fatalf("a full outbox stopped the watchdog at start: %v", err)
		}
		s, err := store.Snapshot()
		// Queued behind the previews, the check can be labelled [DELAYED].
		if err != nil || sender.calls != 1 || !strings.Contains(sender.subject, "[MAIL CHECK]") || s.MailCredentialFailed {
			t.Fatalf("mail check not retried after the outbox drained: calls=%d subject=%q latched=%v %v", sender.calls, sender.subject, s.MailCredentialFailed, err)
		}
	})
}
