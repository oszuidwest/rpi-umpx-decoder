// Package decodermonitor provides a local, observe-only MicroMPX watchdog.
// It never opens an audio PCM device or controls the decoder service.
package decodermonitor

import (
	jsonv2 "encoding/json/v2"
	"errors"
	"fmt"
	"io"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"
)

const maxDocument = 2 << 20

var pcmPath = regexp.MustCompile(`^/proc/asound/[a-zA-Z0-9_-]+/pcm\d+p/sub\d+/status$`)

// Config is independent of the encoder configuration and belongs on each decoder.
// Sending mail is opt-in; neither check mode nor the default preview mode sends it.
type Config struct {
	Site               string `json:"site"`
	PCMStatus          string `json:"pcm_status"`
	StateDir           string `json:"state_dir"`
	EmailFile          string `json:"email_file"`
	SendMail           bool   `json:"send_mail"`
	HeartbeatURL       string `json:"heartbeat_url,omitempty"`
	PollSeconds        int    `json:"poll_seconds"`
	RecoverySeconds    int    `json:"recovery_seconds"`
	ReminderSeconds    int    `json:"reminder_seconds"`
	UnknownSeconds     int    `json:"unknown_seconds"`
	CriticalCount      int    `json:"critical_count"`
	CriticalWindowSecs int    `json:"critical_window_seconds"`
	StalledChecks      int    `json:"stalled_checks"`
	RetentionDays      int    `json:"retention_days"`
	EvidenceMiB        int    `json:"evidence_mib"`
}

// DefaultConfig supplies provisional, configurable incident thresholds.
func DefaultConfig() Config {
	return Config{
		PCMStatus: "/proc/asound/sndrpihifiberry/pcm0p/sub0/status",
		StateDir:  "/var/lib/micrompx-watchdog", PollSeconds: 10,
		RecoverySeconds: 900, ReminderSeconds: 900, UnknownSeconds: 60,
		CriticalCount: 3, CriticalWindowSecs: 60, StalledChecks: 2,
		RetentionDays: 7, EvidenceMiB: 64,
	}
}

// LoadConfig never creates a missing file; omitted fields keep DefaultConfig values.
func LoadConfig(path string) (Config, error) {
	cfg := DefaultConfig()
	if err := readPrivateJSON(path, &cfg); err != nil {
		return Config{}, fmt.Errorf("read watchdog configuration: %w", err)
	}
	if err := cfg.Validate(); err != nil {
		return Config{}, err
	}
	return cfg, nil
}

// Validate rejects settings that could disable bounds or confuse audio identity.
func (c *Config) Validate() error {
	if strings.TrimSpace(c.Site) == "" || len(c.Site) > 100 || strings.ContainsAny(c.Site, "\r\n\x00") {
		return fmt.Errorf("site must be a non-empty single line of at most 100 bytes")
	}
	if !pcmPath.MatchString(c.PCMStatus) {
		return fmt.Errorf("pcm_status must identify one playback status file under /proc/asound")
	}
	if !filepath.IsAbs(c.StateDir) || filepath.Clean(c.StateDir) == string(filepath.Separator) {
		return fmt.Errorf("state_dir must be a dedicated absolute directory")
	}
	if c.SendMail && !filepath.IsAbs(c.EmailFile) {
		return fmt.Errorf("email_file must be an absolute path when send_mail is enabled")
	}
	for _, b := range []struct {
		name          string
		value, lo, hi int
	}{
		{"poll_seconds", c.PollSeconds, 10, 60},
		{"recovery_seconds", c.RecoverySeconds, 60, 86400},
		{"reminder_seconds", c.ReminderSeconds, 60, 86400},
		{"unknown_seconds", c.UnknownSeconds, c.PollSeconds, 3600},
		{"critical_count", c.CriticalCount, 2, 100},
		{"critical_window_seconds", c.CriticalWindowSecs, 10, 3600},
		{"stalled_checks", c.StalledChecks, 2, 10},
		{"retention_days", c.RetentionDays, 1, 90},
		{"evidence_mib", c.EvidenceMiB, 4, 1024},
	} {
		if b.value < b.lo || b.value > b.hi {
			return fmt.Errorf("%s must be between %d and %d", b.name, b.lo, b.hi)
		}
	}
	if c.HeartbeatURL != "" {
		u, err := url.Parse(c.HeartbeatURL)
		if err != nil || u.Scheme != "https" || u.Hostname() == "" || u.User != nil || u.Fragment != "" {
			return fmt.Errorf("heartbeat_url must be an https URL without userinfo or fragment")
		}
	}
	return nil
}

// LoadEmail reads only the existing Graph configuration shape, not encoder settings.
func LoadEmail(path string) (EmailConfig, error) {
	var cfg EmailConfig
	if err := readPrivateJSON(path, &cfg); err != nil {
		return cfg, fmt.Errorf("read M365 configuration: %w", err)
	}
	if err := cfg.Validate(); err != nil {
		return cfg, fmt.Errorf("invalid M365 configuration: %w", err)
	}
	return cfg, nil
}

// readPrivateJSON rejects duplicate, unknown and case-variant keys.
func readPrivateJSON(path string, target any) error {
	f, err := os.Open(path) //nolint:gosec // Administrator-selected configuration path, never remote input.
	if err != nil {
		return fmt.Errorf("open configuration: %w", err)
	}
	defer func() { _ = f.Close() }()
	data, err := readPrivate(f, 64<<10)
	if err != nil {
		return fmt.Errorf("configuration: %w", err)
	}
	if err := jsonv2.Unmarshal(data, target, jsonv2.RejectUnknownMembers(true)); err != nil {
		// Decoder errors can quote a secret supplied with the wrong type.
		return fmt.Errorf("configuration must be one JSON object without duplicate or unknown fields, with valid value types")
	}
	return nil
}

// readPrivate reads an owner-only regular file of at most limit bytes.
func readPrivate(f *os.File, limit int64) ([]byte, error) {
	info, err := f.Stat()
	if err != nil || !info.Mode().IsRegular() || info.Mode().Perm()&0o077 != 0 {
		return nil, fmt.Errorf("must be a regular file readable only by its owner (mode 0600)")
	}
	return readAtMost(f, limit)
}

// readAtMost fails instead of silently truncating input longer than limit.
func readAtMost(r io.Reader, limit int64) ([]byte, error) {
	data, err := io.ReadAll(io.LimitReader(r, limit+1))
	if err == nil && int64(len(data)) > limit {
		err = errors.New("exceeds size limit")
	}
	return data, err
}

func seconds(n int) time.Duration { return time.Duration(n) * time.Second }
