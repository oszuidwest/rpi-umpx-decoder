package decodermonitor

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"testing"
	"time"
)

func fakeCommand(t *testing.T, body string) string {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("probe fakes require /bin/sh")
	}
	if _, err := os.Stat("/bin/sh"); os.IsNotExist(err) {
		t.Skip("probe fakes require /bin/sh")
	}
	path := filepath.Join(t.TempDir(), "command")
	if err := os.WriteFile(path, []byte("#!/bin/sh\nset -eu\n"+body), 0o700); err != nil { //nolint:gosec // Executable fixture in this test's private directory.
		t.Fatal(err)
	}
	return path
}

func testProcRoot(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	for name, content := range map[string]string{
		"sys/kernel/random/boot_id":          "a-boot\n",
		"uptime":                             "123.45 67.89\n",
		"asound/hifiberry/pcm0p/sub0/status": "state: RUNNING\nowner_pid: 321\ntrigger_time: 123.456\nhw_ptr: 1000\nappl_ptr: 2000\n",
		"321/status":                         "Name: MicroMPX\nTgid: 100\n",
	} {
		path := filepath.Join(dir, name)
		if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	return dir
}

func testLocalProbe(t *testing.T, journal string) LocalProbe {
	t.Helper()
	return LocalProbe{
		PCMStatus:      "/proc/asound/hifiberry/pcm0p/sub0/status",
		journalctlPath: fakeCommand(t, journal),
		systemctlPath:  fakeCommand(t, "printf 'ActiveState=active\\nMainPID=100\\n'\n"),
		procRoot:       testProcRoot(t),
	}
}

func TestLocalProbeJournalFailuresKeepBookmarkAndPCM(t *testing.T) {
	for _, tc := range []struct{ name, script, diagnostic string }{
		{"stderr", "printf 'warning\\033[0m\\t: damaged journal\\nsecond line\\n' >&2\n", "journal reader reported a diagnostic: warning[0m: damaged journal"},
		{"exit", "exit 7\n", "journal reader failed: exit status 7"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			anchor := journalLine(t, "saved", "overlap", "1792397600000000")
			record, err := decodeRecord([]byte(anchor))
			if err != nil {
				t.Fatal(err)
			}
			bookmark := Bookmark{Cursor: record.Cursor, Key: record.Key}
			p := testLocalProbe(t, "cat <<'JOURNAL'\n"+anchor+journalLine(t, "next", "new entry", "1792397610000000")+"JOURNAL\n"+tc.script)
			result, err := p.Read(t.Context(), bookmark)
			if err != nil || result.JournalErr == nil {
				t.Fatalf("journal failure not reported independently: %+v %v", result, err)
			}
			if got := result.JournalErr.Error(); got != tc.diagnostic {
				t.Fatalf("journal diagnostic = %q, want %q", got, tc.diagnostic)
			}
			if result.Bookmark != bookmark || !result.Gap || result.CaughtUp || len(result.Records) != 0 {
				t.Fatalf("failed journal read advanced or trusted: %+v", result)
			}
			if result.BootID != "a-boot" || result.Monotonic != 123.45 || result.Active != "active" || result.MainPID != 100 || result.StatusErr != nil ||
				result.PCM.State != "RUNNING" || result.PCM.Owner != 100 || result.PCM.Hardware != 1000 || result.PCM.Applied != 2000 {
				t.Fatalf("journal failure blinded independent observations: %+v", result)
			}
		})
	}
}

// Not parallel: a forked fake can briefly inherit another test's store
// lock before exec on macOS.
func TestLocalProbeJournalTruncationCancelsReader(t *testing.T) {
	line := strings.TrimSpace(journalLine(t, "entry", "Buffer filling", "1792397600000000"))
	p := LocalProbe{journalctlPath: fakeCommand(t, fmt.Sprintf("i=0\nwhile [ \"$i\" -lt %d ]; do\n printf '%%s\\n' '%s'\n i=$((i+1))\ndone\nexec sleep 30\n", maxRecords+1, line))}
	ctx, cancel := context.WithTimeout(t.Context(), 3*time.Second)
	defer cancel()
	result, err := p.readJournal(ctx, Bookmark{})
	if err != nil || result.CaughtUp || result.Gap || len(result.Records) != maxRecords || result.Bookmark.Cursor != "entry" {
		t.Fatalf("truncated journal read was lost or trusted: records=%d caught_up=%v gap=%v bookmark=%+v err=%v", len(result.Records), result.CaughtUp, result.Gap, result.Bookmark, err)
	}
	if ctx.Err() != nil {
		t.Fatal("truncated reader waited for the test deadline instead of being canceled")
	}
}

// Not parallel: a forked fake can briefly inherit another test's store
// lock before exec on macOS.
func TestLocalProbeJournalGapRereadsSince(t *testing.T) {
	p := testLocalProbe(t, `printf '%s\n' "$@" END >> "$0.args"
case "$*" in
 *--cursor=*)
  cat <<'JOURNAL'
`+journalLine(t, "missing-overlap", "new entry", "1792397610000000")+`JOURNAL
  ;;
 *)
  cat <<'JOURNAL'
`+journalLine(t, "backfill", "recent entry", "1792397620000000")+`JOURNAL
  ;;
esac
`)
	before := time.Now()
	result, err := p.Read(t.Context(), Bookmark{Cursor: "saved", Key: "missing"})
	if err != nil || result.JournalErr != nil || !result.Gap || !result.CaughtUp || len(result.Records) != 1 || result.Bookmark.Cursor != "backfill" {
		t.Fatalf("gap did not backfill recent history: %+v %v", result, err)
	}
	data, err := os.ReadFile(p.journalctlPath + ".args")
	if err != nil {
		t.Fatal(err)
	}
	calls := strings.Split(string(data), "END\n")
	if len(calls) != 3 || calls[2] != "" {
		t.Fatalf("expected cursor read followed by backfill: %q", calls)
	}
	for i, call := range calls[:2] {
		args := strings.Split(strings.TrimSpace(call), "\n")
		if !slices.Contains(args, "--output-fields=MESSAGE") {
			t.Fatalf("read %d omitted MESSAGE output restriction: %q", i, args)
		}
		if i == 0 {
			if !slices.Contains(args, "--cursor=saved") {
				t.Fatalf("saved cursor not passed verbatim: %q", args)
			}
			continue
		}
		if slices.Contains(args, "--cursor=saved") {
			t.Fatalf("backfill reused the missing cursor: %q", args)
		}
		var since int64
		if _, err := fmt.Sscanf(args[len(args)-1], "--since=@%d", &since); err != nil || since < before.Add(-time.Minute).Unix() || since > time.Now().Add(-time.Minute).Unix() {
			t.Fatalf("backfill did not request the last minute: %q", args)
		}
	}
}

func TestLocalProbeServiceStatusFailure(t *testing.T) {
	for _, tc := range []struct{ name, script string }{
		{"exit", "exit 7\n"},
		{"timeout", "exec sleep 30\n"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			p := testLocalProbe(t, "exit 0\n")
			p.systemctlPath = fakeCommand(t, tc.script)
			ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
			defer cancel()
			result, err := p.Read(ctx, Bookmark{})
			if err != nil || result.StatusErr == nil || result.Active != "unknown" || result.MainPID != 0 {
				t.Fatalf("failed service status was trusted: %+v %v", result, err)
			}
			if ctx.Err() != nil || result.JournalErr != nil || !result.CaughtUp || result.PCM.State != "RUNNING" {
				t.Fatalf("service failure blocked journal/PCM observations: %+v %v", result, ctx.Err())
			}
		})
	}
}
