package mmcli

import (
	"reflect"
	"strings"
	"testing"

	"github.com/activeshadow/libminimega/minicli"
	"github.com/activeshadow/libminimega/miniclient"
)

// tunnelTable is a `cc tunnel list` answer from one host.
func tunnelTable(host string, rows ...[]string) *minicli.Response {
	return &minicli.Response{
		Host:    host,
		Header:  []string{"vm", "id", "src port", "dst", "dst port"},
		Tabular: rows,
	}
}

// TestRunTabularErr covers the error-returning variant the forward reaper
// relies on: RunTabular reads every failure as an empty table, which made the
// reaper drop forwards whose tunnel minimega still held.
func TestRunTabularErr(t *testing.T) {
	row := []string{"c2b", "0", "51234", "127.0.0.1", "8080"}
	wantRow := map[string]string{
		"host": "head", "vm": "c2b", "id": "0", "src port": "51234", "dst": "127.0.0.1", "dst port": "8080",
	}

	for _, tc := range []struct {
		name     string
		resps    minicli.Responses // nil closes the connection unanswered
		wantRows []map[string]string
		wantErr  string // empty when no error is expected
	}{
		{
			name:     "rows",
			resps:    minicli.Responses{tunnelTable("head", row)},
			wantRows: []map[string]string{wantRow},
		},
		{
			name:     "empty table",
			resps:    minicli.Responses{tunnelTable("head")},
			wantRows: []map[string]string{},
		},
		{
			name: "minimega error",
			resps: minicli.Responses{
				{Host: "head", Error: "no such client: 1234"},
			},
			wantRows: []map[string]string{},
			wantErr:  "no such client: 1234",
		},
		{
			name: "one host fails",
			resps: minicli.Responses{
				tunnelTable("head", row),
				{Host: "other", Error: "vm not found: c2b"},
			},
			wantRows: []map[string]string{wantRow},
			wantErr:  "vm not found: c2b",
		},
		{
			name:     "lost connection",
			wantRows: []map[string]string{},
			wantErr:  "running minimega command",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := newFakeMinimega(t, func(string) (*miniclient.Response, fakeAction) {
				if tc.resps == nil {
					return nil, actionClose
				}

				return &miniclient.Response{Resp: tc.resps}, actionReply
			})

			useFakeMinimega(t, dir)

			cmd := NewNamespacedCommand("exp")
			cmd.Command = "cc tunnel list c2b"

			rows, err := RunTabularErr(cmd)

			if !reflect.DeepEqual(rows, tc.wantRows) {
				t.Errorf("rows = %v, want %v", rows, tc.wantRows)
			}

			switch {
			case tc.wantErr == "" && err != nil:
				t.Errorf("unexpected error: %v", err)
			case tc.wantErr != "" && (err == nil || !strings.Contains(err.Error(), tc.wantErr)):
				t.Errorf("error = %v, want one containing %q", err, tc.wantErr)
			}

			// RunTabular keeps returning the same rows and drops the error.
			if tc.resps != nil {
				if got := RunTabular(cmd); !reflect.DeepEqual(got, tc.wantRows) {
					t.Errorf("RunTabular rows = %v, want %v", got, tc.wantRows)
				}
			}
		})
	}
}
