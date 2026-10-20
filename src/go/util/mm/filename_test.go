package mm

import (
	"errors"
	"strings"
	"testing"
)

func TestCheckFileName(t *testing.T) {
	for _, name := range []string{
		"s1",
		"2026-10-19_1504",
		"k1_0_20261019_1504.pcap",
		"mem1.elf",
		"a.b-c_d",
		strings.Repeat("a", MaxFileNameLength),
	} {
		if err := CheckFileName("snapshot name", name); err != nil {
			t.Errorf("CheckFileName(%q) = %v, want nil", name, err)
		}
	}

	// Each of these reached a root `shell rm -f`, QEMU's `exec:cat > <name>`
	// migration URI or minimega's second parse of a namespaced command.
	for _, name := range []string{
		"",
		"x /phenix/images/ubuntu.qc2 y.z",
		"x -rf /var/lib/phenix y",
		"a;reboot;true${IFS}",
		`a\"b`,
		`a'b`,
		"a$HOME",
		"a#b",
		"../../../../etc/x",
		"a/b",
		"/etc/cron.d/x",
		"..",
		"a..b",
		".hidden",
		"-rf",
		"file:x",
		"a\nb",
		"a\tb",
		"ä",
		strings.Repeat("a", MaxFileNameLength+1),
	} {
		err := CheckFileName("snapshot name", name)
		if !errors.Is(err, ErrInvalidFileName) || !strings.Contains(err.Error(), "snapshot name ") {
			t.Errorf("CheckFileName(%q) = %v, want ErrInvalidFileName naming the snapshot name", name, err)
		}
	}
}
