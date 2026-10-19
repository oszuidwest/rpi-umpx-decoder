package decodermonitor

import (
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"unicode/utf8"
)

func journalLine(t *testing.T, cursor, message, timestamp string) string {
	t.Helper()
	data, err := json.Marshal(map[string]any{"__CURSOR": cursor, "_BOOT_ID": "a-boot", "__REALTIME_TIMESTAMP": timestamp, "__MONOTONIC_TIMESTAMP": "1234", "_PID": "100", "MESSAGE": message})
	if err != nil {
		t.Fatal(err)
	}
	return string(data) + "\n"
}

func TestJournalOpaqueCursorAliasAndLostAnchor(t *testing.T) {
	t.Parallel()
	first := journalLine(t, "opaque-original", "Buffer filling", "1792397600000000")
	record, err := decodeRecord([]byte(first))
	if err != nil {
		t.Fatal(err)
	}
	bookmark := Bookmark{Cursor: record.Cursor, Key: record.Key}
	alias := journalLine(t, "different-opaque-alias", "Buffer filling", "1792397600000000")
	next := journalLine(t, "next", "Buffer empty", "1792397610000000")
	result, err := parseJournal(strings.NewReader(alias+next), bookmark)
	if err != nil || result.Gap || !result.CaughtUp || len(result.Records) != 1 || result.Bookmark.Cursor != "next" {
		t.Fatalf("opaque alias rejected: %+v %v", result, err)
	}
	for _, input := range []string{next, ""} {
		result, err := parseJournal(strings.NewReader(input), bookmark)
		if err != nil || !result.Gap || result.CaughtUp {
			t.Fatalf("missing anchor was trusted: %+v %v", result, err)
		}
	}
}

func TestJournalBoundsAndMalformedRecords(t *testing.T) {
	t.Parallel()
	var input strings.Builder
	for i := range maxRecords + 1 {
		input.WriteString(journalLine(t, string(rune('a'+i)), "Buffer filling", "1792397600000000"))
	}
	result, err := parseJournal(strings.NewReader(input.String()), Bookmark{})
	if err != nil || result.CaughtUp || len(result.Records) != maxRecords {
		t.Fatalf("bad record bound: %d %v %v", len(result.Records), result.CaughtUp, err)
	}
	for _, text := range []string{"not json", `{}`, `{"MESSAGE":[10,20]}`, strings.Repeat("a", 130<<10)} {
		if _, err := parseJournal(strings.NewReader(text), Bookmark{}); err == nil {
			t.Fatal("malformed/oversize journal accepted")
		}
	}
	buffer := &boundedBuffer{limit: 2}
	if n, err := buffer.Write([]byte("abc")); err != nil || n != 3 || buffer.Len() != 2 {
		t.Fatalf("over-limit output must be accepted but truncated: %d %v %d", n, err, buffer.Len())
	}
	if _, err := io.Copy(buffer, strings.NewReader("abc")); err != nil || buffer.Len() != 2 {
		t.Fatal("io.Copy bypassed the subprocess output bound")
	}
	diagnostic := &boundedBuffer{limit: 8192}
	_, _ = diagnostic.Write([]byte("Journal file x.journal~ is truncated\x1b[0m, ignoring file.\nsecond line"))
	if got := diagnostic.firstLine(); got != "Journal file x.journal~ is truncated[0m, ignoring file." {
		t.Fatalf("diagnostic not summarized safely: %q", got)
	}
}

func TestJournalNonTextualAndLargeMessagesAdvance(t *testing.T) {
	t.Parallel()
	byteArray := func(cursor, message string) string {
		data, err := json.Marshal(map[string]any{"__CURSOR": cursor, "_BOOT_ID": "a-boot", "__REALTIME_TIMESTAMP": "1792397600000000", "MESSAGE": []byte(message)})
		if err != nil {
			t.Fatal(err)
		}
		// encoding/json writes []byte as base64; journalctl writes a number array.
		var fields map[string]any
		if err := json.Unmarshal(data, &fields); err != nil {
			t.Fatal(err)
		}
		numbers := make([]int, 0, len(message))
		for _, b := range []byte(message) {
			numbers = append(numbers, int(b))
		}
		fields["MESSAGE"] = numbers
		data, err = json.Marshal(fields)
		if err != nil {
			t.Fatal(err)
		}
		return string(data) + "\n"
	}
	input := byteArray("colour", "\x1b[31mwarning: Buffer empty\x1b[0m") +
		byteArray("binary", "\xffbad") +
		`{"__CURSOR":"no-message","_BOOT_ID":"a-boot","__REALTIME_TIMESTAMP":"1792397600000000"}` + "\n" +
		journalLine(t, "large", strings.Repeat("x", 200<<10), "1792397600000000")
	result, err := parseJournal(strings.NewReader(input), Bookmark{})
	if err != nil || !result.CaughtUp || len(result.Records) != 4 || result.Bookmark.Cursor != "large" {
		t.Fatalf("unusual journal entries wedged the cursor: %+v %v", result.Bookmark, err)
	}
	if !strings.Contains(result.Records[0].Message, "Buffer empty") || !utf8.ValidString(result.Records[1].Message) || result.Records[2].Message != "" {
		t.Fatalf("messages not decoded: %q", []string{result.Records[0].Message, result.Records[1].Message, result.Records[2].Message})
	}
}

func TestReadPCMUsesProcStatusWithoutOpeningAudio(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	for _, path := range []string{"asound/hifiberry/pcm0p/sub0", "321"} {
		if err := os.MkdirAll(filepath.Join(dir, path), 0o700); err != nil {
			t.Fatal(err)
		}
	}
	status := "state: RUNNING\nowner_pid: 321\ntrigger_time: 123.456\nhw_ptr: 1000\nappl_ptr: 2000\n"
	path := filepath.Join(dir, "asound", "hifiberry", "pcm0p", "sub0", "status")
	if err := os.WriteFile(path, []byte(status), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "321", "status"), []byte("Name: MicroMPX\nTgid: 100\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	root, err := os.OpenRoot(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = root.Close() }()
	pcm := readPCM(root, "/proc/asound/hifiberry/pcm0p/sub0/status")
	if pcm.State != "RUNNING" || pcm.Owner != 100 || pcm.Hardware != 1000 || pcm.Applied != 2000 {
		t.Fatalf("bad PCM parse: %+v", pcm)
	}
	if err := os.WriteFile(path, []byte("closed\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if got := readPCM(root, "/proc/asound/hifiberry/pcm0p/sub0/status"); got.State != "closed" {
		t.Fatal(got)
	}
	if got := readPCM(root, "/proc/asound/../../etc/passwd"); got.State != "unavailable" {
		t.Fatal("unsafe proc path accepted")
	}
}
