package mm

import (
	"encoding/json"
	"net"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"testing"

	"github.com/activeshadow/libminimega/minicli"
	"github.com/activeshadow/libminimega/miniclient"

	"phenix/util/common"
)

// fakeReply answers one command sent to the fake minimega. A nil reply sends
// an empty (but successful) response.
type fakeReply func(cmd fakeCommand) []*minicli.Response

// fakeCommand is a command as the fake minimega received it.
type fakeCommand struct {
	raw       string // as sent, with every prefix
	namespace string
	base      string // with the .record, namespace, .columns and .filter prefixes removed
}

var (
	fakeOnce    sync.Once     //nolint:gochecknoglobals // one fake for the package
	fakeMu      sync.Mutex    //nolint:gochecknoglobals // guards fakeHandler and fakeLog
	fakeHandler fakeReply     //nolint:gochecknoglobals // swapped per test
	fakeLog     []fakeCommand //nolint:gochecknoglobals // commands received

	prefixRegex = regexp.MustCompile(
		`^(\.record false |namespace "([^"]*)" |\.columns \S+ |\.filter \S+ )`,
	)
)

func parseFakeCommand(raw string) fakeCommand {
	cmd := fakeCommand{raw: raw}

	rest := raw

	for {
		m := prefixRegex.FindStringSubmatchIndex(rest)
		if m == nil {
			break
		}

		if m[4] >= 0 {
			cmd.namespace = rest[m[4]:m[5]]
		}

		rest = rest[m[1]:]
	}

	cmd.base = rest

	return cmd
}

// useFakeMinimega points mmcli at a fake minimega that answers with handle,
// and returns a function listing the commands received since.
//
// Every test in the package shares one fake (and mmcli's shared connection to
// it), so tests using it must not run in parallel.
func useFakeMinimega(t *testing.T, handle fakeReply) func() []fakeCommand {
	t.Helper()

	fakeOnce.Do(func() {
		dir, err := os.MkdirTemp("", "phenix-mm-test") //nolint:usetesting // outlives the test that creates it
		if err != nil {
			t.Fatalf("creating fake minimega directory: %v", err)
		}

		listener, err := net.Listen("unix", filepath.Join(dir, "minimega"))
		if err != nil {
			t.Fatalf("listening on fake minimega socket: %v", err)
		}

		common.MinimegaBase = dir //nolint:reassign // test fixture

		go func() {
			for {
				conn, err := listener.Accept()
				if err != nil {
					return
				}

				go serveFake(conn)
			}
		}()
	})

	fakeMu.Lock()
	fakeHandler = handle
	fakeLog = nil
	fakeMu.Unlock()

	resetHeadnode()
	resetDiskCaches()

	t.Cleanup(func() {
		fakeMu.Lock()
		fakeHandler = nil
		fakeMu.Unlock()

		resetHeadnode()
		resetDiskCaches()
	})

	return func() []fakeCommand {
		fakeMu.Lock()
		defer fakeMu.Unlock()

		return append([]fakeCommand(nil), fakeLog...)
	}
}

func serveFake(conn net.Conn) {
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

		cmd := parseFakeCommand(req.Command)

		fakeMu.Lock()
		fakeLog = append(fakeLog, cmd)
		handle := fakeHandler
		fakeMu.Unlock()

		var resps []*minicli.Response

		if handle != nil {
			resps = handle(cmd)
		}

		if len(resps) == 0 {
			resps = []*minicli.Response{{Host: "head"}}
		}

		resp := &miniclient.Response{Resp: resps, More: false}

		if err := enc.Encode(resp); err != nil {
			return
		}
	}
}

// tabular builds a tabular response from host.
func tabular(host string, header []string, rows ...[]string) *minicli.Response {
	return &minicli.Response{
		Host:    host,
		Header:  header,
		Tabular: rows,
	}
}

// text builds a plain response from host.
func text(host, body string) *minicli.Response {
	return &minicli.Response{Host: host, Response: body}
}

// countCommands counts the received commands whose base starts with prefix.
func countCommands(cmds []fakeCommand, prefix string) int {
	var count int

	for _, cmd := range cmds {
		if strings.HasPrefix(cmd.base, prefix) {
			count++
		}
	}

	return count
}

func resetHeadnode() {
	headnode.mu.Lock()
	headnode.name = ""
	headnode.mu.Unlock()
}

func resetDiskCaches() {
	diskUsageMu.Lock()
	clear(diskUsageCache)
	diskUsageMu.Unlock()

	snapshotDisks.mu.Lock()
	snapshotDisks.entries = nil
	snapshotDisks.mu.Unlock()
}
