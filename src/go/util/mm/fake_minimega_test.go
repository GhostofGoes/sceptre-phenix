package mm

import (
	"encoding/json"
	"net"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"

	"github.com/activeshadow/libminimega/minicli"
	"github.com/activeshadow/libminimega/miniclient"

	"phenix/util/common"
	"phenix/util/mm/mmcli"
)

// fakeMinimega stands in for the minimega daemon on a unix socket in
// common.MinimegaBase. It records each command it gets, without the
// `.record false ` prefix mmcli adds, and answers it with answer.
type fakeMinimega struct {
	answer func(cmd string) minicli.Responses

	mu       sync.Mutex
	commands []string
	conns    []net.Conn
}

// sent returns the commands the fake got so far.
func (f *fakeMinimega) sent() []string {
	f.mu.Lock()
	defer f.mu.Unlock()

	return slices.Clone(f.commands)
}

// serve makes the fake the minimega phenix talks to for the rest of the test.
func (f *fakeMinimega) serve(t *testing.T) {
	t.Helper()

	// A short path keeps the socket under the length limit.
	dir, err := os.MkdirTemp("", "mmfake") //nolint:usetesting // removed below
	if err != nil {
		t.Fatal(err)
	}

	listener, err := net.Listen("unix", filepath.Join(dir, "minimega"))
	if err != nil {
		t.Fatal(err)
	}

	original := common.MinimegaBase

	t.Cleanup(func() {
		_ = listener.Close()

		f.mu.Lock()
		for _, conn := range f.conns {
			_ = conn.Close()
		}
		f.mu.Unlock()

		// mmcli keeps its connection for the process; one more command makes
		// it notice the fake is gone, so the next test dials anew.
		_ = mmcli.ErrorResponse(mmcli.Run(&mmcli.Command{Command: "version"}))

		_ = os.RemoveAll(dir)
		common.MinimegaBase = original //nolint:reassign // restore test fixture
	})

	common.MinimegaBase = dir //nolint:reassign // test fixture

	go func() {
		for {
			conn, err := listener.Accept()
			if err != nil {
				return
			}

			f.mu.Lock()
			f.conns = append(f.conns, conn)
			f.mu.Unlock()

			go f.handle(conn)
		}
	}()
}

func (f *fakeMinimega) handle(conn net.Conn) {
	defer func() { _ = conn.Close() }()

	dec, enc := json.NewDecoder(conn), json.NewEncoder(conn)

	for {
		var req miniclient.Request
		if dec.Decode(&req) != nil {
			return
		}

		cmd := strings.TrimPrefix(req.Command, ".record false ")

		f.mu.Lock()
		f.commands = append(f.commands, cmd)
		f.mu.Unlock()

		if enc.Encode(miniclient.Response{Resp: f.answer(cmd)}) != nil {
			return
		}
	}
}
