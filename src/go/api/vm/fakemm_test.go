package vm_test

import (
	"encoding/json"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/activeshadow/libminimega/minicli"
	"github.com/activeshadow/libminimega/miniclient"

	"phenix/util/common"
)

// fakeMinimega answers the commands that this package sends to minimega
// through mmcli rather than mm.MM, and records them.
type fakeMinimega struct {
	mu       sync.Mutex
	dir      string
	listener net.Listener
	answer   func(cmd string) minicli.Responses
	commands []string
	raw      []string
}

//nolint:gochecknoglobals // mmcli keeps one connection for the whole process
var (
	fakeMMOnce sync.Once
	fakeMM     fakeMinimega
)

func TestMain(m *testing.M) {
	code := m.Run()

	if fakeMM.listener != nil {
		_ = fakeMM.listener.Close()
		_ = os.RemoveAll(fakeMM.dir)
	}

	os.Exit(code)
}

// useFakeMinimega points mmcli at a fake minimega that answers each command
// with answer until the test ends. Every test shares one socket, since mmcli
// keeps its connection to minimega for the life of the process.
func useFakeMinimega(t *testing.T, answer func(cmd string) minicli.Responses) *fakeMinimega {
	t.Helper()

	fakeMMOnce.Do(func() {
		// The socket outlives the first test that uses it, so it cannot live
		// in a t.TempDir; a short path also keeps it under the socket path limit.
		dir, err := os.MkdirTemp("", "fakemm") //nolint:usetesting // removed in TestMain
		if err != nil {
			t.Fatalf("creating fake minimega directory: %v", err)
		}

		listener, err := net.Listen("unix", filepath.Join(dir, "minimega"))
		if err != nil {
			t.Fatalf("listening on fake minimega socket: %v", err)
		}

		fakeMM.dir, fakeMM.listener = dir, listener
		common.MinimegaBase = dir //nolint:reassign // test fixture for the whole package

		go fakeMM.accept()
	})

	fakeMM.mu.Lock()
	fakeMM.answer, fakeMM.commands, fakeMM.raw = answer, nil, nil
	fakeMM.mu.Unlock()

	t.Cleanup(func() {
		fakeMM.mu.Lock()
		fakeMM.answer = nil
		fakeMM.mu.Unlock()
	})

	return &fakeMM
}

func (f *fakeMinimega) accept() {
	for {
		conn, err := f.listener.Accept()
		if err != nil {
			return
		}

		go f.serve(conn)
	}
}

func (f *fakeMinimega) serve(conn net.Conn) {
	defer func() { _ = conn.Close() }()

	var (
		dec = json.NewDecoder(conn)
		enc = json.NewEncoder(conn)
	)

	for {
		var req miniclient.Request

		if err := dec.Decode(&req); err != nil {
			return
		}

		// Drop the `.record false` and `namespace "<exp>"` prefixes mmcli adds.
		cmd := strings.TrimPrefix(req.Command, ".record false ")
		if ns, ok := strings.CutPrefix(cmd, "namespace "); ok {
			if _, rest, ok := strings.Cut(ns, " "); ok {
				cmd = rest
			}
		}

		f.mu.Lock()
		f.commands = append(f.commands, cmd)
		f.raw = append(f.raw, req.Command)
		answer := f.answer
		f.mu.Unlock()

		// Outside a test that uses it, the fake fails like an absent minimega.
		resps := mmError("dial unix: connect: no such file or directory")
		if answer != nil {
			resps = answer(cmd)
		}

		if err := enc.Encode(miniclient.Response{Resp: resps}); err != nil {
			return
		}
	}
}

// sent returns the commands the fake has answered in this test.
func (f *fakeMinimega) sent() []string {
	f.mu.Lock()
	defer f.mu.Unlock()

	return append([]string(nil), f.commands...)
}

// sentRaw returns the commands the fake has answered in this test as mmcli
// sent them, with their `.record false` and `namespace "<exp>"` prefixes.
func (f *fakeMinimega) sentRaw() []string {
	f.mu.Lock()
	defer f.mu.Unlock()

	return append([]string(nil), f.raw...)
}

// mmTable is a minimega answer holding one table.
func mmTable(header []string, rows ...[]string) minicli.Responses {
	return minicli.Responses{{Host: "head", Header: header, Tabular: rows}}
}

// mmError is a minimega answer holding one error.
func mmError(msg string) minicli.Responses {
	return minicli.Responses{{Host: "head", Error: msg}}
}
