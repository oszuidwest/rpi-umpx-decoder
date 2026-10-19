package decodermonitor

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"slices"
	"sync"
	"time"
)

// Sender permits mail delivery tests without contacting Microsoft Graph.
type Sender interface {
	SendMail(ctx context.Context, subject, body string) error
}

// Runner has an independent mail worker so M365 cannot block local observation.
type Runner struct {
	Config Config
	Store  *Store
	Probe  Prober
	Sender Sender

	blocked    bool   // the last poll could not queue a notification
	lastReason string // only committed polls; a restart logs the current reason again
}

// Run owns and joins its worker. Errors stop only the watchdog, never MicroMPX.
func (r *Runner) Run(ctx context.Context) error {
	if r.Store == nil || r.Probe == nil || (r.Config.SendMail && r.Sender == nil) {
		return fmt.Errorf("watchdog requires a store, probe and configured mail sender")
	}
	ctx, cancel := context.WithCancel(ctx)
	var workers sync.WaitGroup
	workers.Go(func() { r.mailLoop(ctx) })
	defer func() { cancel(); workers.Wait() }()
	ticker := time.NewTicker(seconds(r.Config.PollSeconds))
	defer ticker.Stop()
	heartbeat := time.Now().Add(-time.Minute)
	pruned := time.Time{}
	for {
		if err := ctx.Err(); err != nil {
			return nil
		}
		err := r.poll(ctx)
		// A full queue waits for delivery instead of ending observation: the
		// cursor stays put, the heartbeat stops, and the backlog is processed
		// once mail drains.
		if err != nil && !errors.Is(err, errOutboxFull) {
			return err
		}
		if errors.Is(err, errOutboxFull) && !r.blocked {
			slog.Error("watchdog outbox is full; observation waits for mail delivery", "limit", maxOutbox)
		}
		r.blocked = errors.Is(err, errOutboxFull)
		now := time.Now()
		if now.Sub(pruned) >= time.Hour {
			// Retention is housekeeping: a failure must not end observation.
			if err := r.Store.Prune(now); err != nil {
				slog.Error("watchdog evidence retention failed", "error", err)
			}
			pruned = now
		}
		if r.Config.HeartbeatURL != "" && now.Sub(heartbeat) >= time.Minute {
			if r.ready(now) {
				r.ping(ctx)
			}
			heartbeat = now
		}
		select {
		case <-ctx.Done():
			return nil
		case <-ticker.C:
		}
	}
}

func (r *Runner) poll(ctx context.Context) error {
	state, err := r.Store.Snapshot()
	if err != nil {
		return err
	}
	probe, probeErr := r.Probe.Read(ctx, state.Bookmark)
	if ctx.Err() != nil {
		return ctx.Err()
	}
	now := time.Now()
	engine := Engine{Config: r.Config}
	// Diagnostics follow a committed transition, so a poll rejected by a full
	// outbox neither repeats nor loses them.
	if probeErr != nil {
		probe.Reason = shortDiagnostic("probe read failed: " + probeErr.Error())
		err = r.Store.Update(nil, now, func(s *State) error { return engine.Failed(s, now, probe.Reason) })
	} else {
		err = r.Store.Update(probe.Records, now, func(s *State) error {
			return engine.Step(s, &probe, now)
		})
	}
	if err != nil {
		return err
	}
	if probe.Reason != r.lastReason {
		switch probe.Reason {
		case "":
		case freshBaselineReason:
			slog.Info("watchdog establishing playback baseline")
		default:
			slog.Warn("watchdog decoder observation unavailable", "reason", probe.Reason)
		}
	}
	r.lastReason = probe.Reason
	return nil
}

func (r *Runner) mailLoop(ctx context.Context) {
	ticker := time.NewTicker(2 * time.Second)
	defer ticker.Stop()
	for {
		if ctx.Err() != nil {
			return
		}
		if err := r.deliver(ctx, time.Now()); err != nil {
			slog.Error("watchdog mail state update failed", "error", err)
		}
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}

func (r *Runner) deliver(ctx context.Context, now time.Time) error {
	s, err := r.Store.Snapshot()
	if err != nil {
		return err
	}
	i := r.firstDeliverable(s.Outbox)
	if i < 0 || now.Before(s.Outbox[i].NextAttempt) {
		return nil
	}
	mail := s.Outbox[i]
	var sendErr error
	if mail.Preview {
		slog.Info("watchdog notification preview (not sent)", "notification_id", mail.ID, "subject", mail.Subject)
	} else {
		sendCtx, cancel := context.WithTimeout(ctx, 45*time.Second)
		defer cancel()
		subject := mail.Subject
		body := mail.Body
		if now.Sub(mail.Created) > time.Minute {
			subject = "[DELAYED] " + subject
			body = "This queued notification describes the state at its detection time, not necessarily the current state.\n\n" + body
		}
		sendErr = r.Sender.SendMail(sendCtx, subject, body)
		if sendErr != nil && ctx.Err() != nil {
			return nil // Shutdown, not a failed attempt: retry on the next start.
		}
		if sendErr != nil {
			// Sender errors carry status codes only, never response bodies.
			slog.Warn("watchdog M365 delivery deferred", "notification_id", mail.ID, "attempt", mail.Attempts+1, "error", sendErr)
		} else {
			slog.Info("watchdog mail accepted by M365 (not delivery confirmation)", "notification_id", mail.ID)
		}
	}
	return r.Store.Update(nil, now, func(s *State) error {
		i := slices.IndexFunc(s.Outbox, func(m Mail) bool { return m.ID == mail.ID })
		if i < 0 {
			return fmt.Errorf("watchdog mail queue changed unexpectedly")
		}
		if sendErr == nil {
			s.Outbox = slices.Delete(s.Outbox, i, i+1)
			return nil
		}
		next := &s.Outbox[i]
		next.Attempts++
		next.NextAttempt = now.Add(seconds(1 << min(next.Attempts, 8)))
		var rejection *deliveryError
		if errors.As(sendErr, &rejection) && rejection.retryAfter.After(next.NextAttempt) {
			next.NextAttempt = rejection.retryAfter
		}
		return nil
	})
}

func (r *Runner) ready(now time.Time) bool {
	if r.blocked {
		return false // the last poll's observation was not committed
	}
	s, err := r.Store.Snapshot()
	if err != nil || !s.observing() || s.LastObserved.IsZero() || now.Sub(s.LastObserved) > 2*seconds(r.Config.PollSeconds) {
		return false
	}
	// Known decoder faults do not masquerade as a dead monitoring pipeline.
	i := r.firstDeliverable(s.Outbox)
	return i < 0 || now.Sub(s.Outbox[i].Created) < 5*time.Minute
}

// firstDeliverable skips real mail kept while send_mail is off: it waits for
// delivery to be re-enabled without blocking previews or the heartbeat.
func (r *Runner) firstDeliverable(outbox []Mail) int {
	return slices.IndexFunc(outbox, func(m Mail) bool { return m.Preview || r.Config.SendMail })
}

func (r *Runner) ping(ctx context.Context) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, r.Config.HeartbeatURL, http.NoBody)
	if err != nil {
		slog.Warn("watchdog heartbeat request rejected")
		return
	}
	client := &http.Client{Timeout: 5 * time.Second, CheckRedirect: noRedirect}
	// Administrator-configured HTTPS heartbeat; URL is never logged.
	response, err := client.Do(req)
	if err != nil {
		slog.Warn("watchdog heartbeat failed")
		return
	}
	defer func() { _ = response.Body.Close() }()
	_, _ = io.Copy(io.Discard, io.LimitReader(response.Body, 4096))
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		slog.Warn("watchdog heartbeat rejected", "status", response.StatusCode)
	}
}
