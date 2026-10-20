package plog

import (
	"bytes"
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// Catches the writer/reader timezone mismatch (only reproduces when TZ != UTC).
func TestGetLogsReturnsRecentEntry(t *testing.T) {
	AddFileHandler(filepath.Join(t.TempDir(), "phenix.log"), GetDefaultFileHandlerOpts())

	Info(TypeSystem, "recent entry for GetLogs test")

	logs, err := GetLogs(time.Now().Add(-10*time.Minute), time.Now().Add(time.Minute))
	if err != nil {
		t.Fatalf("GetLogs: %v", err)
	}

	for _, l := range logs {
		if strings.Contains(l.Message, "recent entry for GetLogs test") {
			return
		}
	}

	t.Fatalf("entry not found in recent window (got %d entries)", len(logs))
}

// TestAddFileToCacheSkipsTornLine checks that a line torn by two processes
// writing the log file is skipped without a warning on the console, which
// every CLI command would print, and that the byte positions cached for the
// lines after it still point at the start of a line, so a re-read does not
// meet the torn line again.
func TestAddFileToCacheSkipsTornLine(t *testing.T) {
	var logs bytes.Buffer

	handler.AddHandler(
		"torn-line-test",
		slog.NewJSONHandler(&logs, &slog.HandlerOptions{Level: slog.LevelDebug}),
	)
	t.Cleanup(func() { handler.RemoveHandler("torn-line-test") })

	first := time.Date(2026, 10, 18, 22, 40, 0, 0, time.UTC)
	later := first.Add(2 * DefaultLogFidelity)

	entry := func(at time.Time, msg string) string {
		return fmt.Sprintf(`{"time":%d,"level":"INFO","msg":%q,"type":"SYSTEM"}`+"\n", at.UnixMicro(), msg)
	}

	lines := []string{
		entry(first, "first"),
		`:50.619","level":"INFO","msg":"experiment created","type":"SYSTEM"}` + "\n",
		entry(later, "later"),
	}

	fname := filepath.Join(t.TempDir(), "phenix.log")
	if err := os.WriteFile(fname, []byte(strings.Join(lines, "")), 0o600); err != nil {
		t.Fatalf("writing the log file: %v", err)
	}

	c := Cache{
		Cache:    make(map[int64]CacheEntry),
		FileMap:  make(map[int]CacheFileInfo),
		Fidelity: DefaultLogFidelity,
	}

	// The second read is the re-read every CLI command runs.
	for read := range 2 {
		if err := c.AddFileToCache(fname); err != nil {
			t.Fatalf("read %d: adding the log file to the cache: %v", read, err)
		}
	}

	key := later.Truncate(DefaultLogFidelity).UnixMicro()

	cached, ok := c.Cache[key]
	if !ok {
		t.Fatalf("the cache has no entry for %s: %+v", later, c.Cache)
	}

	if want := len(lines[0]) + len(lines[1]); cached.BytePos != want {
		t.Errorf("the entry of the line after the torn line is cached at byte %d, want %d", cached.BytePos, want)
	}

	skipped := 0

	for line := range strings.Lines(logs.String()) {
		var record struct {
			Level string `json:"level"`
			Msg   string `json:"msg"`
		}

		if err := json.Unmarshal([]byte(line), &record); err != nil {
			t.Fatalf("decoding log record %q: %v", line, err)
		}

		if record.Level != slog.LevelDebug.String() && record.Level != slog.LevelInfo.String() {
			t.Errorf("reading the torn line logged at %s: %s", record.Level, record.Msg)
		}

		if strings.HasPrefix(record.Msg, "skipping a log file line") {
			skipped++
		}
	}

	if skipped != 1 {
		t.Errorf("the torn line was reported %d times at debug over two reads, want once:\n%s", skipped, logs.String())
	}
}
