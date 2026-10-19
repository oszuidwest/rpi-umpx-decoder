package decodermonitor

import (
	"errors"
	"fmt"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"
)

const (
	maxOutbox       = 128
	maxMailAttempts = 5
	maxDeadLetters  = 32
)

var silenceSamples = regexp.MustCompile(`\b(\d+) samples silence played\b`)

// errOutboxFull rejects a transition that needs a notification the queue cannot hold.
var errOutboxFull = errors.New("watchdog outbox is full; cursor was not advanced")

// Incident is persisted with the cursor and notifications in one state commit.
type Incident struct {
	ID         uint64    `json:"id"`
	Opened     time.Time `json:"opened"`
	LastImpact time.Time `json:"last_impact"`
	CleanSince time.Time `json:"-"` // a restart cannot count unobserved time toward recovery
	LastNotice time.Time `json:"last_notice"`
	Severity   string    `json:"severity"`
	Reason     string    `json:"reason"`
	Records    int64     `json:"records"`
	Historical bool      `json:"historical"`
	// What the last notice announced; a difference is news, not a reminder.
	NoticedSeverity   string `json:"noticed_severity"`
	NoticedHistorical bool   `json:"noticed_historical"`
}

// Mail carries no credentials and has a stable ID for at-least-once delivery.
type Mail struct {
	ID          uint64    `json:"id"`
	Created     time.Time `json:"created"`
	Subject     string    `json:"subject"`
	Body        string    `json:"body"`
	Preview     bool      `json:"preview"`
	Attempts    int       `json:"attempts"`
	NextAttempt time.Time `json:"next_attempt"`
	IncidentID  uint64    `json:"incident_id,omitempty"`
}

// DeadLetter retains a mail removed from the outbox after permanent rejection.
type DeadLetter struct {
	Mail
	Status int       `json:"status"`
	Failed time.Time `json:"failed"`
}

// State includes both transport incidents and separate monitoring uncertainty.
// The playback baseline is not persisted: after a restart the first poll is
// never continuous, so the watchdog starts from a fresh baseline.
type State struct {
	Version        int         `json:"version"`
	Bookmark       Bookmark    `json:"bookmark"`
	BootID         string      `json:"-"`
	LastObserved   time.Time   `json:"-"`
	LastMonotonic  float64     `json:"-"`
	LastPCM        PCM         `json:"-"`
	LastPID        int         `json:"-"`
	Stalled        int         `json:"-"`
	Recent         []time.Time `json:"recent"`
	Incident       Incident    `json:"incident"`
	UnknownSince   time.Time   `json:"-"` // a restart starts a fresh grace period
	UnknownNoticed bool        `json:"unknown_noticed"`
	NextID         uint64      `json:"next_id"`
	Outbox         []Mail      `json:"outbox"`

	DeadLetters          []DeadLetter `json:"dead_letters,omitempty"`
	MailCredentialFailed bool         `json:"mail_credential_failed,omitempty"`
}

// Engine contains no I/O.
type Engine struct{ Config Config }

func (s *State) nextID() uint64 { s.NextID++; return s.NextID }

// observing reports whether the latest observations were complete.
func (s *State) observing() bool { return s.UnknownSince.IsZero() }

func (e *Engine) enqueue(s *State, now time.Time, subject, body string, incidentID uint64) error {
	if len(s.Outbox) >= maxOutbox {
		return errOutboxFull
	}
	id := s.nextID()
	s.Outbox = append(s.Outbox, Mail{
		ID: id, Created: now, Subject: subject + " - " + e.Config.Site,
		Body:       fmt.Sprintf("Location: %s\nNotification ID: %d\nDetected: %s\n\n%s\n\nObserve-only: no restart, reset or audio change was performed.\nThis does not certify RF or audible programme health.\n", e.Config.Site, id, now.Format(time.RFC3339), body),
		Preview:    !e.Config.SendMail,
		IncidentID: incidentID,
	})
	return nil
}

func (e *Engine) unknown(s *State, now time.Time) error {
	s.Incident.CleanSince = time.Time{}
	if s.UnknownSince.IsZero() {
		s.UnknownSince = now
	}
	if !s.UnknownNoticed && now.Sub(s.UnknownSince) >= seconds(e.Config.UnknownSeconds) {
		if err := e.enqueue(s, now, "[UNKNOWN] Decoder monitoring unavailable", "The watchdog cannot currently establish complete, current decoder observations. Check journal access/continuity, playback status, storage and watchdog logs. No audio recovery has been inferred.", 0); err != nil {
			return err
		}
		s.UnknownNoticed = true
	}
	return nil
}

// Failed records uncertainty without clearing an existing decoder incident.
func (e *Engine) Failed(s *State, now time.Time) error {
	s.LastObserved = time.Time{}
	return e.unknown(s, now)
}

func (e *Engine) observed(s *State, now time.Time) error {
	if s.UnknownNoticed {
		if err := e.enqueue(s, now, "[MONITORING OK] Decoder observations resumed", "Complete current monitoring observations have resumed. This is not an audio-incident recovery notification.", 0); err != nil {
			return err
		}
	}
	s.UnknownSince, s.UnknownNoticed = time.Time{}, false
	return nil
}

// Step applies one bounded read without I/O. Store.Update persists the records
// as evidence before it commits this state, so the cursor never runs ahead.
func (e *Engine) Step(s *State, p *Probe, now time.Time) error {
	continuous := !s.LastObserved.IsZero() && now.Sub(s.LastObserved) >= 0 && now.Sub(s.LastObserved) <= 2*seconds(e.Config.PollSeconds)
	if !continuous {
		s.Incident.CleanSince = time.Time{}
		s.Stalled = 0
	}
	uncertain := p.unverified()
	for _, record := range p.Records {
		if strings.Contains(strings.ToLower(record.Message), "suppressed") {
			uncertain = true
		}
		e.record(s, record, now)
	}
	progress, fault := e.playback(s, p, continuous)
	if fault != "" {
		e.impact(s, now, "critical", fault, false)
	}
	// The current read can still report an incident while journal continuity is
	// uncertain. Neither EOF nor ActiveState alone permits recovery.
	if uncertain || (!progress && fault == "") {
		if err := e.unknown(s, now); err != nil {
			return err
		}
	} else if err := e.observed(s, now); err != nil {
		return err
	}
	s.Bookmark = p.Bookmark
	s.BootID, s.LastPCM, s.LastPID = p.BootID, p.PCM, p.MainPID
	s.LastObserved, s.LastMonotonic = now, p.Monotonic
	// playback only reports progress without a fault.
	if err := e.recover(s, now, progress); err != nil {
		return err
	}
	return e.notice(s, p.CaughtUp, now)
}

func (e *Engine) recover(s *State, now time.Time, playbackOK bool) error {
	if s.Incident.ID == 0 || !s.observing() || !playbackOK {
		return nil
	}
	if s.Incident.CleanSince.IsZero() {
		s.Incident.CleanSince = now
	}
	if now.Sub(s.Incident.CleanSince) < seconds(e.Config.RecoverySeconds) {
		return nil
	}
	body := fmt.Sprintf("Incident %d: no new decoder-impact messages during %s of current observations, with playback pointers progressing. Last impact: %s.\nThis is not a measurement of RF or programme audio.", s.Incident.ID, seconds(e.Config.RecoverySeconds), s.Incident.LastImpact.Format(time.RFC3339))
	if err := e.enqueue(s, now, "[RECOVERY] Decoder impact observations cleared", body, 0); err != nil {
		return err
	}
	s.Incident, s.Recent = Incident{}, nil
	return nil
}

func (e *Engine) playback(s *State, p *Probe, continuous bool) (progress bool, fault string) {
	if p.Active == "unknown" {
		return false, ""
	}
	if p.Active != "active" || p.MainPID <= 0 {
		return false, "decoder service is not active"
	}
	if p.PCM.State == "unavailable" {
		return false, ""
	}
	if p.PCM.State != "RUNNING" {
		return false, "playback PCM is not running (" + p.PCM.State + ")"
	}
	if p.PCM.Owner != p.MainPID {
		return false, ""
	}
	same := continuous && s.BootID == p.BootID && s.LastPID == p.MainPID && s.LastPCM.Owner == p.PCM.Owner && s.LastPCM.Trigger == p.PCM.Trigger && s.LastPCM.State == "RUNNING" && p.Monotonic > s.LastMonotonic
	if !same {
		s.Stalled = 0
		return false, ""
	}
	// Counter wrap or a rewind is not a stall: only equality is used. A reopen
	// changes trigger/owner and must establish a fresh baseline first.
	if s.LastPCM.Applied == p.PCM.Applied || s.LastPCM.Hardware == p.PCM.Hardware {
		s.Stalled++
		if s.Stalled >= e.Config.StalledChecks {
			return false, "playback sample delivery or hardware position stopped progressing"
		}
		return false, ""
	}
	s.Stalled = 0
	return true, ""
}

func (e *Engine) record(s *State, record Record, now time.Time) {
	starvation := strings.Contains(record.Message, "Buffer empty")
	impact := starvation
	if match := silenceSamples.FindStringSubmatch(record.Message); len(match) == 2 {
		n, err := strconv.ParseUint(match[1], 10, 64)
		impact = impact || (err == nil && n > 0)
	}
	if !impact {
		return
	}
	eventTime := time.UnixMicro(record.TimeUS)
	// A replay/backfill records what happened, not a claim of a live outage.
	// Live records can be up to one poll old, plus slack for a late poll.
	historical := now.Sub(eventTime) > max(30*time.Second, 2*seconds(e.Config.PollSeconds)) || eventTime.After(now.Add(2*time.Second))
	e.impact(s, eventTime, "warning", "decoder reported buffer starvation or inserted silence", historical)
	s.Incident.Records++
	if starvation {
		cutoff := eventTime.Add(-seconds(e.Config.CriticalWindowSecs))
		s.Recent = slices.DeleteFunc(s.Recent, func(t time.Time) bool { return t.Before(cutoff) || t.After(eventTime) })
		s.Recent = append(s.Recent, eventTime)
		if len(s.Recent) >= e.Config.CriticalCount {
			s.Incident.Severity = "critical"
		}
		if len(s.Recent) > e.Config.CriticalCount {
			s.Recent = s.Recent[len(s.Recent)-e.Config.CriticalCount:]
		}
	}
}

func (e *Engine) impact(s *State, when time.Time, severity, reason string, historical bool) {
	if s.Incident.ID == 0 {
		s.Incident = Incident{ID: s.nextID(), Opened: when, Severity: severity, Reason: reason, Historical: historical}
	}
	if severity == "critical" {
		s.Incident.Severity = severity
		s.Incident.Reason = reason
	}
	if !historical {
		s.Incident.Historical = false
	}
	if when.After(s.Incident.LastImpact) {
		s.Incident.LastImpact = when
	}
	s.Incident.CleanSince = time.Time{}
}

func (e *Engine) notice(s *State, caughtUp bool, now time.Time) error {
	i := &s.Incident
	if i.ID == 0 || (!caughtUp && i.Historical) {
		return nil
	}
	changed := i.LastNotice.IsZero() || i.NoticedSeverity != i.Severity || i.NoticedHistorical != i.Historical
	reminder := s.observing() && now.Sub(i.LastNotice) >= seconds(e.Config.ReminderSeconds)
	if !changed && (!reminder || slices.ContainsFunc(s.Outbox, func(m Mail) bool { return m.IncidentID == i.ID })) {
		return nil
	}
	label := strings.ToUpper(i.Severity)
	if i.Historical {
		label = "RETROSPECTIVE " + label
	}
	if !changed {
		label = "REMINDER " + label
	}
	body := fmt.Sprintf("Incident %d\nReason: %s\nFirst impact: %s\nLast impact: %s\nImpact log records: %d (not independent physical dropouts)\nSeverity: %s\n", i.ID, i.Reason, i.Opened.Format(time.RFC3339), i.LastImpact.Format(time.RFC3339), i.Records, i.Severity)
	if err := e.enqueue(s, now, "["+label+"] MicroMPX decoder incident", body, i.ID); err != nil {
		return err
	}
	i.LastNotice, i.NoticedSeverity, i.NoticedHistorical = now, i.Severity, i.Historical
	return nil
}
