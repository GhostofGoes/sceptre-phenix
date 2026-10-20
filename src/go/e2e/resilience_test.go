//go:build e2e

package e2e

import (
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

// resilienceCmdline returns the command line of a process.
func resilienceCmdline(pid int) ([]string, error) {
	data, err := os.ReadFile(filepath.Join("/proc", strconv.Itoa(pid), "cmdline"))
	if err != nil {
		return nil, err
	}

	argv := resilienceSplitNUL(data)
	if len(argv) == 0 {
		return nil, fmt.Errorf("process %d has no command line", pid)
	}

	return argv, nil
}

func resilienceSplitNUL(data []byte) []string {
	var out []string

	for s := range strings.SplitSeq(string(data), "\x00") {
		if s != "" {
			out = append(out, s)
		}
	}

	return out
}

// resilienceAlive reports whether a process exists and has not exited; a
// zombie counts as exited.
func resilienceAlive(pid int) bool {
	data, err := os.ReadFile(filepath.Join("/proc", strconv.Itoa(pid), "stat"))
	if err != nil {
		return false
	}

	i := bytes.LastIndexByte(data, ')')
	if i < 0 {
		return false
	}

	fields := strings.Fields(string(data[i+1:]))

	return len(fields) > 0 && fields[0] != "Z" && fields[0] != "X"
}

// resilienceProc is a QEMU process minimega launched under cfg.mmBase.
type resilienceProc struct {
	pid     int
	comm    string // kernel process name
	uuid    string // -uuid
	id      string // -name, minimega's VM id
	cmdline []string
}

// resilienceQEMU returns the live QEMU processes running under cfg.mmBase,
// keyed by VM UUID.
func resilienceQEMU(t *testing.T) map[string]resilienceProc {
	t.Helper()

	entries, err := os.ReadDir("/proc")
	if err != nil {
		t.Fatal(err)
	}

	out := make(map[string]resilienceProc)

	for _, e := range entries {
		pid, err := strconv.Atoi(e.Name())
		if err != nil {
			continue
		}

		if p, ok := resilienceQEMUProc(pid); ok {
			out[p.uuid] = p
		}
	}

	return out
}

func resilienceQEMUProc(pid int) (resilienceProc, bool) {
	argv, err := resilienceCmdline(pid)
	if err != nil || !resilienceAlive(pid) {
		return resilienceProc{}, false
	}

	if !minimegaQEMU(argv) {
		return resilienceProc{}, false
	}

	p := resilienceProc{pid: pid, cmdline: argv}

	for i := 0; i+1 < len(argv); i++ {
		switch argv[i] {
		case "-uuid":
			p.uuid = argv[i+1]
		case "-name":
			p.id = argv[i+1]
		}
	}

	if p.uuid == "" {
		return resilienceProc{}, false
	}

	if comm, err := os.ReadFile(filepath.Join("/proc", strconv.Itoa(pid), "comm")); err == nil {
		p.comm = strings.TrimSpace(string(comm))
	}

	return p, true
}
