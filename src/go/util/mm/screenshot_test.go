package mm

import (
	"encoding/base64"
	"errors"
	"strings"
	"testing"

	"github.com/activeshadow/libminimega/minicli"
)

// TestGetVMScreenshotReportsMinimegaFailures is a regression test: every
// screenshot error but `vm not found` and `vm not running` was dropped, so a
// failed QMP screendump or a lost connection to minimega read as `screenshot
// not found`, and the REST screenshot answered 400 for an outage.
func TestGetVMScreenshotReportsMinimegaFailures(t *testing.T) {
	const qmpErr = "unable to screendump: qmp timeout"

	png := base64.StdEncoding.EncodeToString([]byte("png"))

	tests := []struct {
		name    string
		answers []*minicli.Response // one per namespace host
		want    error               // or nil when wantMsg is set
		wantMsg string
	}{
		{
			name:    "the VM's host fails, another does not know the VM",
			answers: []*minicli.Response{{Host: "h1", Error: qmpErr}, {Host: "h2", Error: "vm not found: vm1"}},
			wantMsg: qmpErr,
		},
		{
			name:    "the VM is paused, another host does not know it",
			answers: []*minicli.Response{{Host: "h2", Error: "vm not found: vm1"}, {Host: "h1", Error: "vm not running: vm1"}},
			want:    ErrVMNotRunning,
		},
		{
			// minimega answers this for every screenshot of a container VM,
			// which the websocket screenshot ticker skips.
			name: "the VM is a container, another host does not know it",
			answers: []*minicli.Response{
				{Host: "h1", Error: "cannot take screenshot of container"},
				{Host: "h2", Error: "vm not found: vm1"},
			},
			want: ErrScreenshotNotFound,
		},
		{
			name:    "no host knows the VM",
			answers: []*minicli.Response{{Host: "h1", Error: "vm not found: vm1"}, {Host: "h2", Error: "vm not found: vm1"}},
			want:    ErrVMNotFound,
		},
		{
			name:    "a screenshot wins over a failure elsewhere",
			answers: []*minicli.Response{{Host: "h2", Error: qmpErr}, {Host: "h1", Data: png}},
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			fake := &fakeMinimega{answer: func(string) minicli.Responses { return test.answers }}
			fake.serve(t)

			shot, err := Minimega{}.GetVMScreenshot(NS("exp"), VMName("vm1"))

			switch {
			case test.want != nil:
				if !errors.Is(err, test.want) {
					t.Errorf("GetVMScreenshot = %v, want %v", err, test.want)
				}
			case test.wantMsg != "":
				if err == nil || errors.Is(err, ErrScreenshotNotFound) || !strings.Contains(err.Error(), test.wantMsg) {
					t.Errorf("GetVMScreenshot = %v, want an error with %q", err, test.wantMsg)
				}
			default:
				if err != nil || string(shot) != "png" {
					t.Errorf("GetVMScreenshot = %q, %v; want the screenshot", shot, err)
				}
			}
		})
	}

	t.Run("minimega is down", func(t *testing.T) {
		withoutMinimega(t)

		_, err := Minimega{}.GetVMScreenshot(NS("exp"), VMName("vm1"))
		if err == nil || errors.Is(err, ErrScreenshotNotFound) || !strings.Contains(err.Error(), "unable to dial") {
			t.Errorf("GetVMScreenshot with minimega down = %v, want the dial error", err)
		}
	})
}
