package decodermonitor

import (
	"bufio"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"time"
	"unicode"
)

const maxRecords = 512

// Bookmark keeps the opaque journal cursor and the canonical entry fingerprint.
// The overlap entry, not a comparison of opaque cursor strings, checks continuity.
type Bookmark struct {
	Cursor string `json:"cursor"`
	Key    string `json:"key"`
}

// Record preserves the scoped journal entry's identity and original message.
type Record struct {
	Cursor  string `json:"cursor"`
	Key     string `json:"key"`
	TimeUS  int64  `json:"time_us"`
	BootID  string `json:"boot_id"`
	Message string `json:"message"`
}

// PCM is a read-only kernel snapshot, not a measurement of audible programme level.
type PCM struct {
	State    string `json:"state"`
	Owner    int    `json:"owner"`
	Trigger  string `json:"trigger"`
	Hardware uint64 `json:"hardware"`
	Applied  uint64 `json:"applied"`
}

// Probe reports both journal continuity and playback progress independently.
type Probe struct {
	Bookmark             Bookmark // where the next read continues
	BootID               string
	Monotonic            float64
	Gap                  bool // overlap entry not found or journal unreadable; history may be missing
	CaughtUp             bool
	Active               string
	ActiveEnterMonotonic float64 // seconds of CLOCK_MONOTONIC; zero means never active
	MainPID              int
	PCM                  PCM
	Records              []Record
	JournalErr           error  // journal unreadable; Gap is set and Bookmark is the input
	StatusErr            error  // systemctl failed; Active is "unknown"
	PCMErr               error  // kernel playback status unavailable
	Reason               string // first check preventing complete current observations
}

func (p *Probe) unknownReason() string {
	switch {
	case p.JournalErr != nil:
		return shortDiagnostic("journal unavailable: " + p.JournalErr.Error())
	case p.Gap:
		return "journal continuity lost; missing history cannot be reconstructed"
	case !p.CaughtUp:
		return "journal reader has not caught up"
	}
	for _, record := range p.Records {
		if strings.Contains(strings.ToLower(record.Message), "suppressed") {
			return "journal reported suppressed messages"
		}
	}
	switch {
	case p.StatusErr != nil:
		return shortDiagnostic("decoder service status unavailable: " + p.StatusErr.Error())
	case p.Active == "unknown":
		return "decoder service status unknown"
	}
	var pcm string
	switch {
	case p.PCM.State == "unavailable" && p.PCMErr != nil:
		pcm = "playback PCM unavailable: " + p.PCMErr.Error()
	case p.PCM.State == "unavailable":
		pcm = "playback PCM unavailable"
	case p.Active == "active" && p.PCM.State == "RUNNING" && p.PCM.Owner != p.MainPID:
		pcm = fmt.Sprintf("playback PCM owner PID %d differs from decoder MainPID %d", p.PCM.Owner, p.MainPID)
	}
	// The same probe can confirm a service fault, so name that first.
	if pcm != "" && (p.Active != "active" || p.MainPID <= 0) {
		pcm = "decoder service is not active; " + pcm
	}
	return shortDiagnostic(pcm)
}

// Prober permits deterministic watchdog tests without opening audio devices.
type Prober interface {
	Read(context.Context, Bookmark) (Probe, error)
}

// LocalProbe reads journalctl and procfs without opening a PCM device.
type LocalProbe struct{ PCMStatus string }

// Read only invokes journalctl and systemctl show, never service-control actions.
func (p LocalProbe) Read(ctx context.Context, bookmark Bookmark) (Probe, error) {
	var result Probe
	root, err := os.OpenRoot("/proc")
	if err != nil {
		return Probe{}, fmt.Errorf("open procfs: %w", err)
	}
	defer func() { _ = root.Close() }()
	boot, err := readProc(root, "sys/kernel/random/boot_id")
	if err != nil {
		return Probe{}, err
	}
	uptime, err := readProc(root, "uptime")
	if err != nil {
		return Probe{}, err
	}
	fields := strings.Fields(uptime)
	if len(fields) == 0 {
		return Probe{}, fmt.Errorf("missing system uptime")
	}
	result.Monotonic, err = strconv.ParseFloat(fields[0], 64)
	if err != nil {
		return Probe{}, fmt.Errorf("invalid system uptime")
	}
	result.BootID = strings.TrimSpace(boot)
	result.PCM, result.PCMErr = readPCM(root, p.PCMStatus)
	statusCtx, statusCancel := context.WithTimeout(ctx, 2*time.Second)
	result.Active, result.MainPID, result.ActiveEnterMonotonic, result.StatusErr = serviceStatus(statusCtx)
	statusCancel()
	if result.StatusErr != nil {
		result.Active = "unknown"
	}
	// Journal failures must not blind the independent kernel playback check.
	journalCtx, journalCancel := context.WithTimeout(ctx, 5*time.Second)
	journal, journalErr := readJournal(journalCtx, bookmark)
	if journalErr == nil && journal.Gap {
		journal, journalErr = readJournal(journalCtx, Bookmark{})
		journal.Gap = true
	}
	journalCancel()
	if journalErr != nil {
		result.Bookmark, result.Gap, result.JournalErr = bookmark, true, journalErr
	} else {
		result.Bookmark, result.Records = journal.Bookmark, journal.Records
		result.Gap, result.CaughtUp = journal.Gap, journal.CaughtUp
	}
	return result, nil
}

func readJournal(ctx context.Context, bookmark Bookmark) (Probe, error) {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	// Only MESSAGE plus the identity fields journalctl always adds: a large field,
	// such as a coredump entry about the unit, must not exceed the line limit.
	args := []string{"--unit=micrompx.service", "--no-pager", "--quiet", "--all", "--output=json", "--output-fields=MESSAGE", "--no-tail"}
	if bookmark.Cursor != "" {
		if len(bookmark.Cursor) > 4096 || strings.ContainsRune(bookmark.Cursor, 0) {
			return Probe{}, fmt.Errorf("invalid saved journal cursor")
		}
		args = append(args, "--cursor="+bookmark.Cursor)
	} else {
		args = append(args, "--since=@"+strconv.FormatInt(time.Now().Add(-time.Minute).Unix(), 10))
	}
	cmd := exec.CommandContext(ctx, "/usr/bin/journalctl", args...)
	cmd.WaitDelay = time.Second
	stderr := &boundedBuffer{limit: 8192}
	cmd.Stderr = stderr
	pipe, err := cmd.StdoutPipe()
	if err != nil {
		return Probe{}, fmt.Errorf("open journal output: %w", err)
	}
	if err := cmd.Start(); err != nil {
		return Probe{}, fmt.Errorf("start journal reader: %w", err)
	}
	result, readErr := parseJournal(pipe, bookmark)
	truncated := readErr == nil && (!result.CaughtUp || result.Gap)
	if readErr != nil || truncated {
		cancel()
	}
	waitErr := cmd.Wait()
	if readErr != nil {
		return Probe{}, readErr
	}
	if !truncated && waitErr != nil {
		return Probe{}, fmt.Errorf("journal reader failed: %w", waitErr)
	}
	if stderr.Len() != 0 {
		// Includes permission/corruption warnings even if journalctl exits zero.
		return Probe{}, fmt.Errorf("journal reader reported a diagnostic: %s", stderr.firstLine())
	}
	return result, nil
}

func parseJournal(input io.Reader, bookmark Bookmark) (Probe, error) {
	result := Probe{Bookmark: bookmark}
	scanner := bufio.NewScanner(input)
	scanner.Buffer(make([]byte, 4096), maxDocument)
	verified, size := bookmark.Cursor == "", 0
	for scanner.Scan() {
		line := scanner.Bytes()
		size += len(line) + 1
		if size > maxDocument || len(result.Records) >= maxRecords {
			return result, nil
		}
		record, err := decodeRecord(line)
		if err != nil {
			return Probe{}, err
		}
		if !verified {
			if bookmark.Key == "" || record.Key != bookmark.Key {
				result.Gap = true
				return result, nil
			}
			verified = true
			continue
		}
		result.Records = append(result.Records, record)
		result.Bookmark = Bookmark{Cursor: record.Cursor, Key: record.Key}
	}
	if err := scanner.Err(); err != nil {
		return Probe{}, fmt.Errorf("read journal output: %w", err)
	}
	if !verified {
		result.Gap = true
		return result, nil
	}
	result.CaughtUp = true
	return result, nil
}

func decodeRecord(line []byte) (Record, error) {
	var fields map[string]any
	decoder := json.NewDecoder(bytes.NewReader(line))
	decoder.UseNumber()
	if err := decoder.Decode(&fields); err != nil {
		return Record{}, fmt.Errorf("invalid journal JSON")
	}
	text := func(key string) string { value, _ := fields[key].(string); return value }
	record := Record{Cursor: text("__CURSOR"), BootID: text("_BOOT_ID"), Message: journalText(fields["MESSAGE"])}
	var err error
	record.TimeUS, err = strconv.ParseInt(text("__REALTIME_TIMESTAMP"), 10, 64)
	if err != nil || record.TimeUS <= 0 || record.Cursor == "" || record.BootID == "" {
		return Record{}, fmt.Errorf("journal entry lacks identity metadata")
	}
	delete(fields, "__CURSOR")
	canonical, err := json.Marshal(fields)
	if err != nil {
		return Record{}, fmt.Errorf("canonicalize journal entry: %w", err)
	}
	record.Key = fmt.Sprintf("%x", sha256.Sum256(canonical))
	return record, nil
}

// journalText decodes MESSAGE, which journalctl emits as a byte array when it
// contains control characters or invalid UTF-8. Any other shape reads as empty
// rather than failing the batch, so the cursor can still move past it.
func journalText(value any) string {
	switch value := value.(type) {
	case string:
		return value
	case []any:
		data := make([]byte, 0, len(value))
		for _, item := range value {
			number, _ := item.(json.Number)
			b, err := strconv.ParseUint(number.String(), 10, 8)
			if err != nil {
				return ""
			}
			data = append(data, byte(b))
		}
		return strings.ToValidUTF8(string(data), "\uFFFD")
	}
	return ""
}

func serviceStatus(ctx context.Context) (active string, pid int, activeEnter float64, err error) {
	cmd := exec.CommandContext(ctx, "/usr/bin/systemctl", "show", "--no-pager", "micrompx.service", "-p", "ActiveState", "-p", "MainPID", "-p", "ActiveEnterTimestampMonotonic")
	cmd.WaitDelay = time.Second
	output, err := cmd.Output()
	if err != nil {
		return "", 0, 0, fmt.Errorf("read decoder service status: %w", err)
	}
	return parseServiceStatus(string(output))
}

func parseServiceStatus(output string) (active string, pid int, activeEnter float64, err error) {
	values := parseFields(output, "=")
	pid, pidErr := strconv.Atoi(values["MainPID"])
	entered, enteredErr := strconv.ParseUint(values["ActiveEnterTimestampMonotonic"], 10, 64)
	if pidErr != nil || pid < 0 || enteredErr != nil || values["ActiveState"] == "" {
		return "", 0, 0, fmt.Errorf("incomplete decoder service status")
	}
	return values["ActiveState"], pid, float64(entered) / 1e6, nil
}

func readProc(root *os.Root, name string) (string, error) {
	f, err := root.Open(name)
	if err != nil {
		return "", fmt.Errorf("read kernel status: %w", err)
	}
	defer func() { _ = f.Close() }()
	data, err := readAtMost(f, 16<<10)
	if err != nil {
		return "", fmt.Errorf("invalid kernel status: %w", err)
	}
	return string(data), nil
}

// readPCM relies on root to keep path inside /proc; Config.Validate checks its shape.
func readPCM(root *os.Root, path string) (PCM, error) {
	unavailable := PCM{State: "unavailable"}
	text, err := readProc(root, strings.TrimPrefix(path, "/proc/"))
	if err != nil {
		return unavailable, fmt.Errorf("PCM status %s: %w", path, err)
	}
	if strings.TrimSpace(text) == "closed" {
		return PCM{State: "closed"}, nil
	}
	fields := parseFields(text, ":")
	owner, ownerErr := strconv.Atoi(fields["owner_pid"])
	hardware, hardwareErr := strconv.ParseUint(fields["hw_ptr"], 10, 64)
	applied, appliedErr := strconv.ParseUint(fields["appl_ptr"], 10, 64)
	var invalid string
	switch {
	case ownerErr != nil || owner <= 0:
		invalid = "owner_pid"
	case hardwareErr != nil:
		invalid = "hw_ptr"
	case appliedErr != nil:
		invalid = "appl_ptr"
	case fields["state"] == "":
		invalid = "state"
	case fields["trigger_time"] == "":
		invalid = "trigger_time"
	}
	if invalid != "" {
		return unavailable, fmt.Errorf("PCM status %s: missing or invalid %s", path, invalid)
	}
	// owner_pid can be a thread; the service reports its process ID.
	process, err := readProc(root, fmt.Sprintf("%d/status", owner))
	if err != nil {
		return unavailable, fmt.Errorf("PCM owner %d process status: %w", owner, err)
	}
	pid, err := strconv.Atoi(parseFields(process, ":")["Tgid"])
	if err != nil || pid <= 0 {
		return unavailable, fmt.Errorf("PCM owner %d process status: missing or invalid Tgid", owner)
	}
	return PCM{State: fields["state"], Owner: pid, Trigger: fields["trigger_time"], Hardware: hardware, Applied: applied}, nil
}

func parseFields(text, separator string) map[string]string {
	values := make(map[string]string)
	for line := range strings.SplitSeq(text, "\n") {
		if key, value, ok := strings.Cut(line, separator); ok {
			values[strings.TrimSpace(key)] = strings.TrimSpace(value)
		}
	}
	return values
}

// boundedBuffer keeps the first limit bytes of subprocess output and discards
// the rest, so a large write still shows that output was produced.
type boundedBuffer struct {
	buffer bytes.Buffer
	limit  int
}

func (b *boundedBuffer) Len() int { return b.buffer.Len() }

func (b *boundedBuffer) Write(data []byte) (int, error) {
	b.buffer.Write(data[:min(len(data), b.limit-b.Len())])
	return len(data), nil
}

// firstLine returns a short, printable summary of the output for logs.
func (b *boundedBuffer) firstLine() string {
	return shortDiagnostic(b.buffer.String())
}

// shortDiagnostic accepts only local probe diagnostics, never mail/config errors.
func shortDiagnostic(text string) string {
	line, _, _ := strings.Cut(text, "\n")
	line = strings.Map(func(r rune) rune {
		if unicode.IsPrint(r) {
			return r
		}
		return -1
	}, line)
	if len(line) > 200 {
		line = strings.ToValidUTF8(line[:200], "")
	}
	return line
}
