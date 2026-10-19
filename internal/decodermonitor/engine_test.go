package decodermonitor

import (
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
	if err := e.Failed(&s, at.Add(410*time.Second)); err != nil {
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
	if err := e.Step(&s, p, at); err == nil {
		t.Fatal("full outbox silently dropped an incident")
	}
	if len(s.Outbox) != maxOutbox {
		t.Fatal("pending mail was discarded")
	}
}

func TestEngineFaultsAndMissingTelemetry(t *testing.T) {
	for _, tc := range []struct {
		name, state string
		owner       int
		want        string
	}{
		{"xrun", "XRUN", 100, "critical"}, {"closed", "closed", 0, "critical"},
		{"missing", "unavailable", 0, ""}, {"foreign_audio", "RUNNING", 200, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			cfg := DefaultConfig()
			cfg.Site = "Test"
			e, s := Engine{cfg}, State{Version: 1}
			at := time.Now()
			p := healthyProbe(at, 0)
			p.PCM.State, p.PCM.Owner = tc.state, tc.owner
			advance(t, &e, &s, p, at)
			if s.Incident.Severity != tc.want {
				t.Fatalf("got %s; want %s", s.Incident.Severity, tc.want)
			}
			if tc.want == "" && s.observing() {
				t.Fatal("unverified telemetry was considered healthy")
			}
		})
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
	for i := 1; i <= 190; i++ {
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
	err := engine.Step(&s, p, now)
	fmt.Println(err, s.Incident.Severity, s.Outbox[0].Preview)
	// Output: <nil> critical true
}
