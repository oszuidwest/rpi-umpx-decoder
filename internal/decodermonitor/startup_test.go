package decodermonitor

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"
)

func TestEngineStartupGrace(t *testing.T) {
	for _, active := range []string{"activating", "active", "failed"} {
		t.Run(active, func(t *testing.T) {
			t.Parallel()
			cfg := DefaultConfig()
			cfg.UnknownSeconds = 10
			e, s := Engine{cfg}, State{Version: 1}
			at := time.Now()
			for i := 0; i < cfg.StartupGraceSeconds/cfg.PollSeconds; i++ {
				now := at.Add(seconds(i * cfg.PollSeconds))
				p := healthyProbe(now, 0)
				p.Monotonic, p.ActiveEnterMonotonic = 1000+float64(i*cfg.PollSeconds), 1000
				p.Active, p.PCM = active, PCM{State: "closed"}
				if active == "activating" {
					p.MainPID, p.ActiveEnterMonotonic = 0, 0
				}
				if active == "failed" {
					p.MainPID = 0
				}
				advance(t, &e, &s, p, now)
				if s.Incident.ID != 0 || s.observing() {
					t.Fatalf("startup became an incident or health at poll %d: %+v", i, s)
				}
			}
			if len(s.Outbox) != 1 || !strings.HasPrefix(s.Outbox[0].Subject, "[UNKNOWN]") {
				t.Fatalf("startup uncertainty not reported as unknown: %+v", s.Outbox)
			}
		})
	}
}

func TestEngineStartupGraceBlocksRecovery(t *testing.T) {
	t.Parallel()
	cfg := DefaultConfig()
	e, s := Engine{cfg}, State{Version: 1}
	at := time.Now()
	advance(t, &e, &s, healthyProbe(at, 0), at)
	at = at.Add(10 * time.Second)
	p := healthyProbe(at, 1920000)
	p.Records = []Record{impactRecord(at)}
	advance(t, &e, &s, p, at)
	lastImpact := s.Incident.LastImpact
	// The next healthy poll could recover; a starting decoder must earn no credit.
	s.Incident.CleanSince = at.Add(-seconds(cfg.RecoverySeconds))
	for i := 1; i <= 3; i++ {
		now := at.Add(seconds(i * cfg.PollSeconds))
		p := healthyProbe(now, 0)
		p.PCM.State = "closed"
		p.ActiveEnterMonotonic = p.Monotonic - float64(i*cfg.PollSeconds)
		advance(t, &e, &s, p, now)
		if s.Incident.Severity != "warning" || !s.Incident.LastImpact.Equal(lastImpact) ||
			!s.Incident.CleanSince.IsZero() || s.observing() || len(s.Outbox) != 1 {
			t.Fatalf("startup fault escalated or earned recovery credit: %+v", s)
		}
	}
}

func TestEngineStartupFaultExpires(t *testing.T) {
	for _, tc := range []struct {
		name, active string
		grace, age   int
	}{
		{"activating", "activating", 60, 0},
		{"closed", "active", 60, 0},
		{"failed_after_start", "failed", 60, 0},
		{"configured_grace", "activating", 30, 0},
		{"already_starting", "active", 60, 50},
		{"never_active", "active", 60, 1000},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			cfg := DefaultConfig()
			cfg.StartupGraceSeconds, cfg.UnknownSeconds = tc.grace, 3600
			e, s := Engine{cfg}, State{Version: 1}
			at := time.Now()
			expires := max(tc.grace-tc.age, 0)
			for elapsed := 0; elapsed <= expires+cfg.PollSeconds; elapsed += cfg.PollSeconds {
				now := at.Add(seconds(elapsed))
				p := healthyProbe(now, 0)
				p.Monotonic, p.ActiveEnterMonotonic = 1000+float64(elapsed), float64(1000-tc.age)
				p.Active, p.PCM = tc.active, PCM{State: "closed"}
				if tc.active != "active" {
					p.MainPID = 0
				}
				advance(t, &e, &s, p, now)
				if elapsed < expires+cfg.PollSeconds {
					if s.Incident.ID != 0 || s.observing() {
						t.Fatalf("fault confirmed before grace plus two polls at %ds: %+v", elapsed, s)
					}
				} else if s.Incident.Severity != "critical" || len(s.Outbox) != 1 ||
					!strings.HasPrefix(s.Outbox[0].Subject, "[CRITICAL]") {
					t.Fatalf("fault not confirmed after grace at %ds: %+v", elapsed, s)
				}
			}
		})
	}
}

func TestEngineCrashLoopCannotRenewGrace(t *testing.T) {
	t.Parallel()
	cfg := DefaultConfig()
	cfg.UnknownSeconds = 3600
	e, s := Engine{cfg}, State{Version: 1}
	at := time.Now()
	for elapsed := 0; elapsed <= cfg.StartupGraceSeconds+cfg.PollSeconds; elapsed += cfg.PollSeconds {
		now := at.Add(seconds(elapsed))
		p := healthyProbe(now, 0)
		p.Monotonic, p.ActiveEnterMonotonic = 1000+float64(elapsed), 1000+float64(elapsed)
		// Fresh start timestamps without a PID change: only the grace is tested.
		p.PCM = PCM{State: "closed"}
		advance(t, &e, &s, p, now)
		if elapsed < cfg.StartupGraceSeconds+cfg.PollSeconds {
			if s.Incident.Severity == "critical" || s.observing() {
				t.Fatalf("crash loop confirmed early or counted as health at %ds: %+v", elapsed, s)
			}
		} else if s.Incident.Severity != "critical" || !strings.HasPrefix(s.Outbox[len(s.Outbox)-1].Subject, "[CRITICAL]") {
			t.Fatalf("fresh timestamps hid crash loop beyond the grace: %+v", s)
		}
	}
}

func TestEnginePlaybackProgressRenewsStartupBound(t *testing.T) {
	t.Parallel()
	cfg := DefaultConfig()
	cfg.UnknownSeconds = 3600
	e, s := Engine{cfg}, State{Version: 1}
	at := time.Now()
	for elapsed := 0; elapsed <= 80; elapsed += 10 {
		now := at.Add(seconds(elapsed))
		p := healthyProbe(now, uint64(elapsed)*192000)
		if elapsed <= 60 {
			p.PCM.State = "unavailable"
		}
		advance(t, &e, &s, p, now)
	}
	if !s.observing() {
		t.Fatal("playback never established progress")
	}
	for elapsed := 90; elapsed <= 120; elapsed += 10 {
		now := at.Add(seconds(elapsed))
		p := healthyProbe(now, 0)
		p.PCM.State, p.ActiveEnterMonotonic = "closed", float64(at.Add(90*time.Second).Unix())
		advance(t, &e, &s, p, now)
		if s.Incident.ID != 0 || s.observing() {
			t.Fatalf("old unhealthy streak denied a fresh startup grace: %+v", s)
		}
	}
}

func TestEngineRestartWarningAndRecovery(t *testing.T) {
	for _, pid := range []int{200, 0} {
		t.Run(fmt.Sprint(pid), func(t *testing.T) {
			t.Parallel()
			cfg := DefaultConfig()
			e, s := Engine{cfg}, State{Version: 1}
			at := time.Now()
			advance(t, &e, &s, healthyProbe(at, 0), at)
			advance(t, &e, &s, healthyProbe(at.Add(10*time.Second), 1920000), at.Add(10*time.Second))
			at = at.Add(20 * time.Second)
			p := healthyProbe(at, 0)
			p.MainPID, p.PCM = pid, PCM{State: "closed"}
			p.ActiveEnterMonotonic = p.Monotonic
			if pid == 0 {
				p.Active = "activating"
			}
			advance(t, &e, &s, p, at)
			reason := fmt.Sprintf("decoder service restarted or stopped (MainPID 100 → %d)", pid)
			if s.Incident.Severity != "warning" || s.Incident.Reason != reason || len(s.Outbox) != 1 ||
				!strings.HasPrefix(s.Outbox[0].Subject, "[WARNING]") || !strings.Contains(s.Outbox[0].Body, reason) ||
				!s.Incident.CleanSince.IsZero() {
				t.Fatalf("restart not reported immediately: %+v", s)
			}
			for elapsed := 10; elapsed <= cfg.RecoverySeconds+20; elapsed += 10 {
				now := at.Add(seconds(elapsed))
				p := healthyProbe(now, uint64(elapsed)*192000)
				p.MainPID, p.PCM.Owner = 200, 200
				advance(t, &e, &s, p, now)
				if elapsed < cfg.RecoverySeconds+20 && s.Incident.ID == 0 {
					t.Fatalf("restart recovered before 15 minutes of progress at %ds", elapsed)
				}
			}
			if s.Incident.ID != 0 || len(s.Outbox) != 2 || !strings.HasPrefix(s.Outbox[1].Subject, "[RECOVERY]") {
				t.Fatalf("restart failed to recover after clean interval: %+v", s)
			}
		})
	}
}

func TestEngineRestartRequiresKnownPreviousPID(t *testing.T) {
	for _, tc := range []struct {
		name      string
		interrupt func(*testing.T, *Engine, *State, *Probe, time.Time) time.Time
	}{
		{"watchdog_start", func(t *testing.T, _ *Engine, s *State, _ *Probe, at time.Time) time.Time {
			data, err := json.Marshal(s)
			if err != nil {
				t.Fatal(err)
			}
			*s = State{}
			if err := json.Unmarshal(data, s); err != nil {
				t.Fatal(err)
			}
			return at
		}},
		{"polling_gap", func(_ *testing.T, _ *Engine, _ *State, _ *Probe, at time.Time) time.Time { return at.Add(time.Minute) }},
		{"boot_change", func(_ *testing.T, _ *Engine, _ *State, p *Probe, at time.Time) time.Time {
			p.BootID = "boot-b"
			return at
		}},
		{"failed_poll", func(t *testing.T, e *Engine, s *State, _ *Probe, at time.Time) time.Time {
			if err := e.Failed(s, at); err != nil {
				t.Fatal(err)
			}
			return at.Add(10 * time.Second)
		}},
		{"previous_pid_zero", func(_ *testing.T, _ *Engine, s *State, _ *Probe, at time.Time) time.Time {
			s.LastPID = 0
			return at
		}},
		{"systemctl_failed", func(t *testing.T, e *Engine, s *State, p *Probe, at time.Time) time.Time {
			p.Active, p.MainPID = "unknown", 0
			advance(t, e, s, p, at)
			return at.Add(10 * time.Second)
		}},
		{"systemctl_unknown_with_pid", func(t *testing.T, e *Engine, s *State, p *Probe, at time.Time) time.Time {
			p.Active = "unknown"
			advance(t, e, s, p, at)
			return at.Add(10 * time.Second)
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			cfg := DefaultConfig()
			cfg.UnknownSeconds = 3600
			e, s := Engine{cfg}, State{Version: 1}
			at := time.Now()
			advance(t, &e, &s, healthyProbe(at, 0), at)
			at = at.Add(10 * time.Second)
			p := healthyProbe(at, 1920000)
			at = tc.interrupt(t, &e, &s, p, at)
			p.Active, p.MainPID, p.PCM.Owner = "active", 200, 200
			p.Monotonic = float64(at.Unix())
			advance(t, &e, &s, p, at)
			if s.Incident.ID != 0 || len(s.Outbox) != 0 || s.observing() {
				t.Fatalf("unknown previous PID became a restart or health: %+v", s)
			}
		})
	}
}

func TestEngineStartupBoundResetsAfterObservationBreak(t *testing.T) {
	for _, tc := range []struct {
		name string
		gap  time.Duration
		boot string
	}{
		{"polling_gap", time.Minute, "boot-a"},
		{"boot_change", 0, "boot-b"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			cfg := DefaultConfig()
			cfg.UnknownSeconds = 3600
			e, s := Engine{cfg}, State{Version: 1}
			at := time.Now()
			for elapsed := 0; elapsed < cfg.StartupGraceSeconds; elapsed += 10 {
				now := at.Add(seconds(elapsed))
				p := healthyProbe(now, 0)
				p.Active, p.MainPID = "activating", 0
				advance(t, &e, &s, p, now)
			}
			at = at.Add(seconds(cfg.StartupGraceSeconds) + tc.gap)
			for elapsed := 0; elapsed <= 20; elapsed += 10 {
				now := at.Add(seconds(elapsed))
				p := healthyProbe(now, 0)
				p.Active, p.MainPID, p.BootID = "activating", 0, tc.boot
				advance(t, &e, &s, p, now)
				if s.Incident.ID != 0 || len(s.Outbox) != 0 || s.observing() {
					t.Fatalf("unobserved time consumed startup grace: %+v", s)
				}
			}
		})
	}
}

func TestEngineStartupGraceRestartsFaultConfirmation(t *testing.T) {
	t.Parallel()
	cfg := DefaultConfig()
	e, s := Engine{cfg}, State{Version: 1}
	at := time.Now()
	for i, active := range []string{"inactive", "activating", "inactive", "inactive"} {
		now := at.Add(seconds(i * cfg.PollSeconds))
		p := healthyProbe(now, 0)
		p.Active, p.MainPID = active, 0
		advance(t, &e, &s, p, now)
		if i < 3 {
			if s.Incident.ID != 0 || s.observing() {
				t.Fatalf("grace retained an earlier fault confirmation at poll %d: %+v", i, s)
			}
		} else if s.Incident.Severity != "critical" {
			t.Fatalf("faults after grace were not confirmed: %+v", s)
		}
	}
}

func TestEngineRepeatedRestartsEscalateEvenWhenPlaybackStarts(t *testing.T) {
	t.Parallel()
	cfg := DefaultConfig()
	cfg.UnknownSeconds = 3600
	e, s := Engine{cfg}, State{Version: 1}
	at := time.Now()
	// The decoder crashes right after opening its PCM: activating and a
	// RUNNING poll under a fresh PID alternate, so pointers never progress.
	for i := range 2*cfg.CriticalCount + 1 {
		now := at.Add(seconds(i * cfg.PollSeconds))
		p := healthyProbe(now, uint64(i)*1920000)
		p.Monotonic, p.ActiveEnterMonotonic = 1000+float64(i*cfg.PollSeconds), 1000+float64(i*cfg.PollSeconds)
		p.MainPID, p.PCM.Owner = 100+i, 100+i
		if i%2 == 1 {
			p.Active, p.MainPID = "activating", 0
		}
		advance(t, &e, &s, p, now)
	}
	if s.Incident.Severity != "critical" || !strings.Contains(s.Incident.Reason, "restarting repeatedly") {
		t.Fatalf("crash loop that reaches RUNNING stayed below critical: %+v", s.Incident)
	}
	if got := subjects(&s); len(got) != 2 || !strings.HasPrefix(got[0], "[WARNING]") || !strings.HasPrefix(got[1], "[CRITICAL]") {
		t.Fatalf("want one restart warning, then one critical: %q", got)
	}
}
