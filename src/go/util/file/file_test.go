package file_test

import (
	"encoding/json"
	"net"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"sync"
	"testing"

	"github.com/activeshadow/libminimega/minicli"
	"github.com/activeshadow/libminimega/miniclient"

	"phenix/util/common"
	"phenix/util/file"
)

const (
	meshListing = "mesh send all file list exp/files/ recursive"
	headListing = "file list exp/files/ recursive"
)

// fileListing is a `file list` answer from host, listing names.
func fileListing(host string, names ...string) *minicli.Response {
	resp := &minicli.Response{Host: host, Header: []string{"dir", "name", "size", "modified"}, Tabular: [][]string{}}

	for _, name := range names {
		resp.Tabular = append(resp.Tabular, []string{"", name, "0", "2026-10-19T00:00:00Z"})
	}

	return resp
}

// fakeMinimega answers the commands phenix sends it from answers, without the
// `.record false ` prefix mmcli adds, until the test ends. A command missing
// from answers gets an empty answer.
type fakeMinimega struct {
	mu      sync.Mutex
	answers map[string]minicli.Responses
}

func (f *fakeMinimega) set(answers map[string]minicli.Responses) {
	f.mu.Lock()
	defer f.mu.Unlock()

	f.answers = answers
}

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

		f.mu.Lock()
		resps := f.answers[strings.TrimPrefix(req.Command, ".record false ")]
		f.mu.Unlock()

		if enc.Encode(miniclient.Response{Resp: resps}) != nil {
			return
		}
	}
}

// TestGetExperimentFilePaths checks that the strict listing returns the files
// of every node, and fails when a node's listing fails: a subnet capture names
// its file after the paths it does not return, and minimega truncates a file
// that exists.
func TestGetExperimentFilePaths(t *testing.T) {
	fake := new(fakeMinimega)
	fake.serve(t)

	for _, tc := range []struct {
		name    string
		answers map[string]minicli.Responses
		want    []string
		wantErr string // empty when no error is expected
	}{
		{
			name: "mesh and head node",
			answers: map[string]minicli.Responses{
				meshListing: {fileListing("peer", "exp/files/a.pcap", "exp/files/run/b.pcap")},
				headListing: {fileListing("head", "exp/files/c.pcap")},
			},
			want: []string{"a.pcap", "c.pcap", "run/b.pcap"},
		},
		{
			// minimega's answer for a missing files directory, and a mesh
			// without peers.
			name:    "no files",
			answers: map[string]minicli.Responses{headListing: {fileListing("head")}},
		},
		{
			name: "a mesh peer times out",
			answers: map[string]minicli.Responses{
				meshListing: {fileListing("peer", "exp/files/a.pcap"), {Host: "late", Error: "timed out"}},
				headListing: {fileListing("head", "exp/files/c.pcap")},
			},
			wantErr: "listing exp/files/ on the cluster: 1 error occurred:\n\t* timed out",
		},
		{
			name: "the head node fails",
			answers: map[string]minicli.Responses{
				headListing: {{Host: "head", Error: "permission denied"}},
			},
			wantErr: "permission denied",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			fake.set(tc.answers)

			got, err := file.MMClusterFiles{}.GetExperimentFilePaths("exp")
			slices.Sort(got)

			if tc.wantErr == "" {
				if err != nil || !reflect.DeepEqual(got, tc.want) {
					t.Errorf("GetExperimentFilePaths(exp) = %q, %v, want %q", got, err, tc.want)
				}

				return
			}

			if err == nil || !strings.Contains(err.Error(), tc.wantErr) || got != nil {
				t.Errorf("GetExperimentFilePaths(exp) = %q, %v, want no paths and an error containing %q", got, err, tc.wantErr)
			}
		})
	}
}
