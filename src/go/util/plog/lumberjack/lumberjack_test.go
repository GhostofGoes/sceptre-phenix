package lumberjack

import (
	"path/filepath"
	"testing"
	"time"
)

// useTime makes currentTime give now for the rest of the test.
func useTime(t *testing.T, now time.Time) {
	t.Helper()

	original := currentTime
	t.Cleanup(func() { currentTime = original })

	currentTime = func() time.Time { return now }
}

// timeFromName gives an error, and does not panic, for a name that has the
// prefix and the extension but is shorter than both together.
func TestTimeFromNameShortName(t *testing.T) {
	l := &Logger{Filename: filepath.Join(t.TempDir(), "a..-")}
	prefix, ext := l.prefixAndExt()

	if prefix != "a.-" || ext != ".-" {
		t.Fatalf("prefixAndExt() = %q, %q, want %q, %q", prefix, ext, "a.-", ".-")
	}

	if got, err := l.timeFromName("a.-", prefix, ext); err == nil {
		t.Errorf("timeFromName(%q, %q, %q) = %v, nil, want an error", "a.-", prefix, ext, got)
	}
}

// timeFromName reads back the time in the name that backupName gives. The
// clock is in UTC, so the local and UTC names hold the same time.
func TestTimeFromNameRoundTrip(t *testing.T) {
	now := time.Date(2016, 11, 4, 18, 30, 0, 123_000_000, time.UTC)
	useTime(t, now)

	for _, name := range []string{"server.log", "server", "a.b.log", ".log", "a..-"} {
		for _, local := range []bool{false, true} {
			l := &Logger{Filename: filepath.Join(t.TempDir(), name)}
			prefix, ext := l.prefixAndExt()
			backup := filepath.Base(backupName(l.Filename, local))

			got, err := l.timeFromName(backup, prefix, ext)
			if err != nil {
				t.Errorf("timeFromName(%q, %q, %q) error = %v, want nil", backup, prefix, ext, err)

				continue
			}

			if !got.Equal(now) {
				t.Errorf("timeFromName(%q, %q, %q) = %s, want %s", backup, prefix, ext, got, now)
			}
		}
	}
}
