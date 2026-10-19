package decodermonitor

import (
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"
)

func healthyProbe(at time.Time, frame uint64) *Probe {
	return &Probe{BootID: "boot-a", Monotonic: float64(at.Unix()),
		CaughtUp: true, Active: "active", MainPID: 100,
		PCM: PCM{State: "RUNNING", Owner: 100, Trigger: "stream-a", Hardware: frame, Applied: frame + 4096}}
}

func impactRecord(at time.Time) Record {
	return Record{TimeUS: at.UnixMicro(), Message: "warning: Buffer empty: had to jump", Cursor: at.String(), Key: at.String(), BootID: "boot-a"}
}

func advance(t *testing.T, e *Engine, state *State, p *Probe, at time.Time) {
	t.Helper()
	if err := e.Step(state, p, at); err != nil {
		t.Fatal(err)
	}
}

func TestEngineImpactEscalationAndNoFalseRecovery(t *testing.T) {
	t.Parallel()
	cfg := DefaultConfig()
	cfg.Site = "Test site"
	e, s := Engine{cfg}, State{Version: 1}
	at := time.Date(2026, 10, 19, 8, 0, 0, 0, time.UTC)
	advance(t, &e, &s, healthyProbe(at, 0), at)
	for i := 1; i <= 3; i++ {
		now := at.Add(time.Duration(i) * 10 * time.Second)
		p := healthyProbe(now, uint64(i)*1920000)
		p.Records = []Record{impactRecord(now), {TimeUS: now.UnixMicro(), Message: "100 samples silence played"}}
		advance(t, &e, &s, p, now)
	}
	if s.Incident.Severity != "critical" || len(s.Outbox) != 2 {
		t.Fatalf("incident=%+v, notices=%d", s.Incident, len(s.Outbox))
	}
	if s.Incident.Records != 6 {
		t.Fatalf("impact record count: %d", s.Incident.Records)
	}
	for i := 4; i <= 40; i++ {
		now := at.Add(time.Duration(i) * 10 * time.Second)
		advance(t, &e, &s, healthyProbe(now, uint64(i)*1920000), now)
	}
	if s.Incident.ID == 0 {
		t.Fatal("six-minute gap falsely cleared the incident")
	}
	if err := e.Failed(&s, at.Add(410*time.Second), "probe read failed"); err != nil {
		t.Fatal(err)
	}
	for i := 42; i <= 50; i++ {
		now := at.Add(time.Duration(i) * 10 * time.Second)
		p := healthyProbe(now, uint64(i)*1920000)
		p.CaughtUp = false
		advance(t, &e, &s, p, now)
	}
	if s.observing() || !s.Incident.CleanSince.IsZero() || !s.UnknownNoticed {
		t.Fatalf("incomplete observations allowed recovery: %+v", s)
	}
	for i := 51; i <= 141; i++ {
		now := at.Add(time.Duration(i) * 10 * time.Second)
		advance(t, &e, &s, healthyProbe(now, uint64(i)*1920000), now)
	}
	if s.Incident.ID != 0 {
		t.Fatalf("incident did not recover after a fresh 15-minute window: %+v", s.Incident)
	}
	recoveries := 0
	for _, mail := range s.Outbox {
		if strings.Contains(mail.Subject, "[RECOVERY]") {
			recoveries++
		}
	}
	if recoveries != 1 {
		t.Fatalf("recovery mails: %d", recoveries)
	}
}

func TestEngineKernelWatchdogWithoutJournalOrHTTP(t *testing.T) {
	for _, tc := range []struct {
		name   string
		change func(*Probe)
	}{
		{"application_stops", func(p *Probe) { p.PCM.Applied = 4096 }},
		{"hardware_stops", func(p *Probe) { p.PCM.Hardware = 0 }},
		{"both_stop", func(p *Probe) { p.PCM.Applied, p.PCM.Hardware = 4096, 0 }},
		{"journal_also_unavailable", func(p *Probe) { p.PCM.Applied = 4096; p.CaughtUp = false; p.Gap = true }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			cfg := DefaultConfig()
			cfg.Site = "Test"
			e, s := Engine{cfg}, State{Version: 1}
			at := time.Now()
			advance(t, &e, &s, healthyProbe(at, 0), at)
			for i := 1; i <= cfg.StalledChecks; i++ {
				now := at.Add(time.Duration(i) * 10 * time.Second)
				p := healthyProbe(now, uint64(i)*1920000)
				tc.change(p)
				advance(t, &e, &s, p, now)
				if i < cfg.StalledChecks && s.Incident.ID != 0 {
					t.Fatal("playback stall opened before enough comparisons")
				}
			}
			if s.Incident.Severity != "critical" || len(s.Outbox) != 1 {
				t.Fatalf("silent freeze not detected: %+v", s)
			}
			if !strings.Contains(s.Incident.Reason, "stopped progressing") {
				t.Fatal(s.Incident.Reason)
			}
			if !s.Incident.CleanSince.IsZero() {
				t.Fatal("frozen playback accrued clean time")
			}
		})
	}
}

func TestEngineIgnoresFECAndZeroSilence(t *testing.T) {
	t.Parallel()
	cfg := DefaultConfig()
	cfg.Site = "Test"
	e, s := Engine{cfg}, State{Version: 1}
	at := time.Now()
	advance(t, &e, &s, healthyProbe(at, 0), at)
	for i := 1; i < 120; i++ {
		now := at.Add(time.Duration(i) * 10 * time.Second)
		p := healthyProbe(now, uint64(i)*1920000)
		p.Records = []Record{{TimeUS: now.UnixMicro(), Message: "Recovered packet using FEC"}, {TimeUS: now.UnixMicro(), Message: "0 samples silence played"}}
		advance(t, &e, &s, p, now)
	}
	if len(s.Outbox) != 0 || s.Incident.ID != 0 || !s.observing() {
		t.Fatalf("clean decoder alarmed: %+v", s)
	}
}

func TestEnginePlaybackReopenAndCounterWrap(t *testing.T) {
	t.Parallel()
	cfg := DefaultConfig()
	cfg.Site = "Test"
	e, s := Engine{cfg}, State{Version: 1}
	at := time.Now()
	advance(t, &e, &s, healthyProbe(at, ^uint64(0)-10000), at)
	at = at.Add(10 * time.Second)
	advance(t, &e, &s, healthyProbe(at, 10000), at)
	if s.Incident.ID != 0 || !s.observing() {
		t.Fatal("counter wrap became a stall")
	}
	at = at.Add(10 * time.Second)
	p := healthyProbe(at, 0)
	p.PCM.Trigger = "reopened"
	advance(t, &e, &s, p, at)
	if s.Incident.ID != 0 || s.observing() {
		t.Fatal("reopen must establish a fresh baseline, not alarm or recover")
	}
}

func TestEngineUnknownGapAndDelayedBackfill(t *testing.T) {
	t.Parallel()
	cfg := DefaultConfig()
	cfg.Site = "Test"
	e, s := Engine{cfg}, State{Version: 1}
	at := time.Now()
	p := healthyProbe(at, 0)
	p.Gap, p.CaughtUp = true, false
	p.Records = []Record{impactRecord(at.Add(-10 * time.Minute))}
	advance(t, &e, &s, p, at)
	if len(s.Outbox) != 0 || s.observing() {
		t.Fatal("backfill generated a live notice or false health")
	}
	at = at.Add(10 * time.Second)
	advance(t, &e, &s, healthyProbe(at, 1920000), at)
	if len(s.Outbox) != 1 || !strings.Contains(s.Outbox[0].Subject, "RETROSPECTIVE") {
		t.Fatalf("missing retrospective summary: %+v", s.Outbox)
	}
}

func TestEngineOutboxBackpressureDoesNotDropMail(t *testing.T) {
	t.Parallel()
	cfg := DefaultConfig()
	cfg.Site = "Test"
	e, s := Engine{cfg}, State{Version: 1}
	for i := range maxOutbox {
		s.Outbox = append(s.Outbox, Mail{ID: uint64(i) + 1})
	}
	at := time.Now()
	p := healthyProbe(at, 0)
	p.Active = "inactive"
	advance(t, &e, &s, p, at)
	if err := e.Step(&s, p, at.Add(10*time.Second)); err == nil {
		t.Fatal("full outbox silently dropped an incident")
	}
	if len(s.Outbox) != maxOutbox {
		t.Fatal("pending mail was discarded")
	}
}

func TestEngineFaultsAndMissingTelemetry(t *testing.T) {
	for _, tc := range []struct {
		name   string
		change func(*Probe)
		reason string
	}{
		{"inactive", func(p *Probe) { p.Active = "inactive" }, "decoder service is not active"},
		{"activating", func(p *Probe) { p.Active, p.MainPID = "activating", 0 }, "decoder service is not active"},
		{"no_pid", func(p *Probe) { p.MainPID = 0 }, "decoder service is not active"},
		{"negative_pid", func(p *Probe) { p.MainPID = -1 }, "decoder service is not active"},
		{"xrun", func(p *Probe) { p.PCM.State = "XRUN" }, "playback PCM is not running (XRUN)"},
		{"closed", func(p *Probe) { p.PCM = PCM{State: "closed"} }, "playback PCM is not running (closed)"},
		{"journal_unavailable", func(p *Probe) {
			p.Active, p.Gap, p.CaughtUp = "inactive", true, false
		}, "decoder service is not active"},
		{"missing", func(p *Probe) { p.PCM = PCM{State: "unavailable"} }, ""},
		{"foreign_audio", func(p *Probe) { p.PCM.Owner = 200 }, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			cfg := DefaultConfig()
			cfg.Site = "Test"
			e, s := Engine{cfg}, State{Version: 1}
			at := time.Now()
			p := healthyProbe(at, 0)
			tc.change(p)
			advance(t, &e, &s, p, at)
			if s.Incident.ID != 0 || len(s.Outbox) != 0 || s.observing() {
				t.Fatalf("first fault or missing telemetry was not unknown: %+v", s)
			}
			at = at.Add(10 * time.Second)
			p = healthyProbe(at, 1920000)
			tc.change(p)
			advance(t, &e, &s, p, at)
			if tc.reason == "" {
				if s.Incident.ID != 0 || len(s.Outbox) != 0 || s.observing() {
					t.Fatalf("unverified telemetry became health or an incident: %+v", s)
				}
			} else if s.Incident.Severity != "critical" || s.Incident.Reason != tc.reason || len(s.Outbox) != 1 {
				t.Fatalf("two faults did not confirm the original critical reason: %+v", s)
			}
		})
	}
}

func TestEngineExplicitFaultsRequireContinuousPolls(t *testing.T) {
	for _, tc := range []struct {
		name      string
		interrupt func(*testing.T, *Engine, *State, time.Time) time.Time
	}{
		{"healthy", func(t *testing.T, e *Engine, s *State, at time.Time) time.Time {
			advance(t, e, s, healthyProbe(at, 1920000), at)
			return at.Add(10 * time.Second)
		}},
		{"service_unknown", func(t *testing.T, e *Engine, s *State, at time.Time) time.Time {
			p := healthyProbe(at, 1920000)
			p.Active = "unknown"
			advance(t, e, s, p, at)
			return at.Add(10 * time.Second)
		}},
		{"pcm_unavailable", func(t *testing.T, e *Engine, s *State, at time.Time) time.Time {
			p := healthyProbe(at, 1920000)
			p.PCM.State = "unavailable"
			advance(t, e, s, p, at)
			return at.Add(10 * time.Second)
		}},
		{"foreign_audio", func(t *testing.T, e *Engine, s *State, at time.Time) time.Time {
			p := healthyProbe(at, 1920000)
			p.PCM.Owner = 200
			advance(t, e, s, p, at)
			return at.Add(10 * time.Second)
		}},
		{"failed_read", func(t *testing.T, e *Engine, s *State, at time.Time) time.Time {
			if err := e.Failed(s, at, "probe read failed"); err != nil {
				t.Fatal(err)
			}
			return at.Add(10 * time.Second)
		}},
		{"late_poll", func(_ *testing.T, _ *Engine, _ *State, at time.Time) time.Time {
			return at.Add(20*time.Second + time.Nanosecond)
		}},
		{"clock_rewind", func(_ *testing.T, _ *Engine, _ *State, at time.Time) time.Time {
			return at.Add(-20 * time.Second)
		}},
		{"boot_changed", func(_ *testing.T, _ *Engine, s *State, at time.Time) time.Time {
			s.BootID = "previous-boot"
			return at
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			cfg := DefaultConfig()
			cfg.Site = "Test"
			e, s := Engine{cfg}, State{Version: 1}
			at := time.Now()
			p := healthyProbe(at, 0)
			p.Active = "inactive"
			advance(t, &e, &s, p, at)
			at = tc.interrupt(t, &e, &s, at.Add(10*time.Second))
			p = healthyProbe(at, 2*1920000)
			p.Active = "inactive"
			advance(t, &e, &s, p, at)
			if s.Incident.ID != 0 || len(s.Outbox) != 0 || s.observing() {
				t.Fatalf("nonconsecutive faults opened an incident or counted as health: %+v", s)
			}
			at = at.Add(10 * time.Second)
			advance(t, &e, &s, p, at)
			if s.Incident.Severity != "critical" || len(s.Outbox) != 1 {
				t.Fatalf("fault confirmation did not resume after interruption: %+v", s)
			}
		})
	}
}

func TestEngineExplicitFaultDebouncesEscalationAndBlocksRecovery(t *testing.T) {
	t.Parallel()
	cfg := DefaultConfig()
	cfg.Site = "Test"
	e, s := Engine{cfg}, State{Version: 1}
	at := time.Now()
	advance(t, &e, &s, healthyProbe(at, 0), at)
	at = at.Add(10 * time.Second)
	p := healthyProbe(at, 1920000)
	p.Records = []Record{impactRecord(at)}
	advance(t, &e, &s, p, at)
	if s.Incident.CleanSince.IsZero() {
		t.Fatal("healthy playback did not start the clean interval")
	}
	lastImpact := s.Incident.LastImpact
	at = at.Add(10 * time.Second)
	p = healthyProbe(at, 2*1920000)
	p.PCM.State = "closed"
	advance(t, &e, &s, p, at)
	if s.observing() || !s.Incident.CleanSince.IsZero() || s.Incident.Severity != "warning" ||
		!s.Incident.LastImpact.Equal(lastImpact) || len(s.Outbox) != 1 {
		t.Fatalf("first fault escalated or accrued recovery credit: %+v", s)
	}
	at = at.Add(10 * time.Second)
	p = healthyProbe(at, 3*1920000)
	p.Active = "inactive"
	advance(t, &e, &s, p, at)
	if s.Incident.Severity != "critical" || s.Incident.Reason != "decoder service is not active" ||
		!s.Incident.LastImpact.Equal(at) || len(s.Outbox) != 2 {
		t.Fatalf("consecutive explicit faults did not escalate: %+v", s)
	}
}

func subjects(s *State) []string {
	var result []string
	for _, mail := range s.Outbox {
		result = append(result, mail.Subject)
	}
	return result
}

func TestEngineRetrospectiveIncidentAnnouncesLiveRecurrenceAndEscalation(t *testing.T) {
	t.Parallel()
	cfg := DefaultConfig()
	cfg.Site = "Test"
	e, s := Engine{cfg}, State{Version: 1}
	at := time.Now()
	p := healthyProbe(at, 0)
	p.Records = []Record{impactRecord(at.Add(-time.Hour))}
	advance(t, &e, &s, p, at)
	if got := subjects(&s); len(got) != 1 || !strings.HasPrefix(got[0], "[RETROSPECTIVE WARNING]") {
		t.Fatalf("no retrospective summary: %q", got)
	}
	// A backlog escalates the retrospective incident while not caught up.
	at = at.Add(10 * time.Second)
	p = healthyProbe(at, 1920000)
	p.CaughtUp = false
	old := at.Add(-time.Hour)
	p.Records = []Record{impactRecord(old), impactRecord(old.Add(time.Second)), impactRecord(old.Add(2 * time.Second))}
	advance(t, &e, &s, p, at)
	at = at.Add(10 * time.Second)
	advance(t, &e, &s, healthyProbe(at, 2*1920000), at)
	if got := subjects(&s); len(got) != 2 || !strings.HasPrefix(got[1], "[RETROSPECTIVE CRITICAL]") {
		t.Fatalf("escalation during a backlog was not announced: %q", got)
	}
	at = at.Add(10 * time.Second)
	p = healthyProbe(at, 3*1920000)
	p.Records = []Record{impactRecord(at)}
	advance(t, &e, &s, p, at)
	if got := subjects(&s); len(got) != 3 || !strings.HasPrefix(got[2], "[CRITICAL]") {
		t.Fatalf("live recurrence not announced as live news: %q", got)
	}
}

func TestEngineLiveRecordsWithLongPollAreNotRetrospective(t *testing.T) {
	t.Parallel()
	cfg := DefaultConfig()
	cfg.Site, cfg.PollSeconds = "Test", 60
	e, s := Engine{cfg}, State{Version: 1}
	at := time.Now()
	p := healthyProbe(at, 0)
	p.Records = []Record{impactRecord(at.Add(-50 * time.Second))}
	advance(t, &e, &s, p, at)
	if got := subjects(&s); len(got) != 1 || !strings.HasPrefix(got[0], "[WARNING]") {
		t.Fatalf("live record from the last poll labelled retrospective: %q", got)
	}
}

func TestEngineUncertainInputsBlockHealth(t *testing.T) {
	for _, tc := range []struct {
		name   string
		change func(*Probe)
	}{
		{"service_status_unknown", func(p *Probe) { p.Active = "unknown" }},
		{"journal_suppressed", func(p *Probe) {
			p.Records = []Record{{TimeUS: time.Now().UnixMicro(), Message: "Suppressed 1234 messages from micrompx.service"}}
		}},
		{"gap_after_backfill", func(p *Probe) { p.Gap = true }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			cfg := DefaultConfig()
			cfg.Site = "Test"
			e, s := Engine{cfg}, State{Version: 1}
			at := time.Now()
			advance(t, &e, &s, healthyProbe(at, 0), at)
			at = at.Add(10 * time.Second)
			advance(t, &e, &s, healthyProbe(at, 1920000), at)
			if !s.observing() {
				t.Fatal("healthy baseline not established")
			}
			at = at.Add(10 * time.Second)
			p := healthyProbe(at, 2*1920000)
			tc.change(p)
			advance(t, &e, &s, p, at)
			if s.observing() || s.Incident.ID != 0 {
				t.Fatalf("uncertain input treated as health or as a fault: %+v", s.Incident)
			}
		})
	}
}

func TestEngineRemindersAreDeduplicated(t *testing.T) {
	t.Parallel()
	cfg := DefaultConfig()
	cfg.Site = "Test"
	e, s := Engine{cfg}, State{Version: 1}
	at := time.Now()
	p := healthyProbe(at, 0)
	p.Active = "inactive"
	advance(t, &e, &s, p, at)
	if len(s.Outbox) != 0 {
		t.Fatal("unconfirmed fault queued mail")
	}
	advance(t, &e, &s, p, at.Add(10*time.Second))
	if s.Incident.Severity != "critical" || len(s.Outbox) != 1 {
		t.Fatal("confirmed fault did not queue the initial critical")
	}
	for i := 2; i <= 190; i++ {
		now := at.Add(time.Duration(i) * 10 * time.Second)
		p := healthyProbe(now, 0)
		p.Active = "inactive"
		advance(t, &e, &s, p, now)
	}
	if got := subjects(&s); len(got) != 1 {
		t.Fatalf("reminder queued while the incident's mail is still pending: %q", got)
	}
	s.Outbox = nil
	for i := 191; i <= 290; i++ {
		now := at.Add(time.Duration(i) * 10 * time.Second)
		p := healthyProbe(now, 0)
		p.Active = "inactive"
		advance(t, &e, &s, p, now)
	}
	if got := subjects(&s); len(got) != 1 || !strings.HasPrefix(got[0], "[REMINDER CRITICAL]") {
		t.Fatalf("missing single reminder after delivery: %q", got)
	}
}

func ExampleEngine() {
	cfg := DefaultConfig()
	cfg.Site = "Example transmitter"
	s := State{Version: 1}
	now := time.Date(2026, 10, 19, 8, 0, 0, 0, time.UTC)
	p := healthyProbe(now, 0)
	p.Active = "inactive"
	engine := Engine{Config: cfg}
	if err := engine.Step(&s, p, now); err != nil {
		panic(err)
	}
	err := engine.Step(&s, p, now.Add(10*time.Second))
	fmt.Println(err, s.Incident.Severity, s.Outbox[0].Preview)
	// Output: <nil> critical true
}

func TestEngineUnknownReasonInMail(t *testing.T) {
	for _, tc := range []struct {
		name, want string
		change     func(*Probe, *State)
	}{
		{"journal_error_first", "journal unavailable: permission denied", func(p *Probe, _ *State) {
			p.JournalErr, p.Gap, p.CaughtUp = errors.New("permission denied\nmore detail"), true, false
			p.StatusErr, p.PCMErr = errors.New("systemctl failed"), errors.New("PCM failed")
			p.Active, p.PCM.State = "unknown", "unavailable"
		}},
		{"gap_first", "journal continuity lost; missing history cannot be reconstructed", func(p *Probe, _ *State) {
			p.Gap, p.CaughtUp, p.Active = true, false, "unknown"
		}},
		{"backlog_first", "journal reader has not caught up", func(p *Probe, _ *State) {
			p.CaughtUp, p.Active = false, "unknown"
		}},
		{"suppressed_first", "journal reported suppressed messages", func(p *Probe, _ *State) {
			p.Records = []Record{{Message: "Suppressed messages with private operational data"}}
			p.Active = "unknown"
		}},
		{"systemctl_error", "decoder service status unavailable: systemctl failed", func(p *Probe, _ *State) {
			p.Active, p.StatusErr = "unknown", errors.New("systemctl failed")
		}},
		{"systemctl_unknown", "decoder service status unknown", func(p *Probe, _ *State) { p.Active = "unknown" }},
		{"pcm_error", "playback PCM unavailable: /proc/asound/card/pcm0p/sub0/status: permission denied", func(p *Probe, _ *State) {
			p.PCM.State, p.PCMErr = "unavailable", errors.New("/proc/asound/card/pcm0p/sub0/status: permission denied")
		}},
		{"pcm_unavailable", "playback PCM unavailable", func(p *Probe, _ *State) { p.PCM.State = "unavailable" }},
		{"owner_mismatch", "playback PCM owner PID 321 differs from decoder MainPID 100", func(p *Probe, _ *State) { p.PCM.Owner = 321 }},
		{"fresh_baseline", "no playback progress yet (fresh baseline)", func(_ *Probe, s *State) { s.LastObserved = time.Time{} }},
		{"pending_progress", "playback pointers have not both progressed yet", func(p *Probe, s *State) { p.PCM.Applied = s.LastPCM.Applied }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			cfg := DefaultConfig()
			cfg.Site = "Test"
			e, s := Engine{cfg}, State{Version: 1}
			at := time.Now()
			advance(t, &e, &s, healthyProbe(at, 0), at)
			at = at.Add(10 * time.Second)
			p := healthyProbe(at, 1920000)
			tc.change(p, &s)
			s.UnknownSince = at.Add(-seconds(cfg.UnknownSeconds))
			advance(t, &e, &s, p, at)
			if p.Reason != tc.want || s.observing() || len(s.Outbox) != 1 || !strings.HasPrefix(s.Outbox[0].Subject, "[UNKNOWN]") || !strings.Contains(s.Outbox[0].Body, "Reason: "+tc.want+"\n") {
				t.Fatalf("unknown reason missing or inconsistent: reason=%q mail=%+v", p.Reason, s.Outbox)
			}
		})
	}
}

func TestEngineReasonNamesFaultAwaitingConfirmation(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name, want string
		change     func(*Probe)
	}{
		{"service", "decoder service is not active (awaiting confirmation)", func(p *Probe) { p.Active = "inactive" }},
		{"pcm", "playback PCM is not running (closed) (awaiting confirmation)", func(p *Probe) { p.PCM.State = "closed" }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			cfg := DefaultConfig()
			cfg.Site = "Test"
			e, s := Engine{cfg}, State{Version: 1}
			at := time.Now()
			p := healthyProbe(at, 0)
			tc.change(p)
			advance(t, &e, &s, p, at)
			if p.Reason != tc.want || s.Incident.ID != 0 {
				t.Fatalf("first explicit fault: reason=%q incident=%+v", p.Reason, s.Incident)
			}
		})
	}
}
