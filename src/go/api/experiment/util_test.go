package experiment

import (
	"errors"
	"strings"
	"testing"

	"github.com/hashicorp/go-multierror"

	"phenix/util/mm"
)

func TestLaunchErrors(t *testing.T) {
	t.Run("no failed VMs", func(t *testing.T) {
		vms := mm.VMs{
			{Name: "a", State: "RUNNING"},
			{Name: "b", State: "BUILDING"}, // delayed start
			{Name: "c", State: "QUIT"},
		}

		if err := launchErrors(vms, []string{"a", "b", "c"}); err != nil {
			t.Fatalf("launchErrors() = %v, want nil", err)
		}
	})

	t.Run("failed VMs are reported with minimega's reason", func(t *testing.T) {
		vms := mm.VMs{
			{Name: "ok", State: "RUNNING"},
			{Name: "qxl", State: "ERROR", Tags: map[string]string{"error": "kvm: QXL VGA not available"}},
			{Name: "notag", State: "ERROR"},
		}

		err := launchErrors(vms, []string{"ok", "qxl", "notag"})
		if err == nil {
			t.Fatal("launchErrors() = nil, want an error")
		}

		var merr *multierror.Error
		if !errors.As(err, &merr) || len(merr.Errors) != 2 {
			t.Fatalf("launchErrors() = %v, want a multierror with 2 errors", err)
		}

		for _, want := range []string{"VM qxl is in the ERROR state: kvm: QXL VGA not available", "VM notag is in the ERROR state: minimega recorded no reason"} {
			if !strings.Contains(err.Error(), want) {
				t.Errorf("launchErrors() = %q, want it to contain %q", err, want)
			}
		}

		if strings.Contains(err.Error(), "VM ok") {
			t.Errorf("launchErrors() = %q, reports a running VM", err)
		}
	})

	t.Run("expected VMs minimega does not list are reported", func(t *testing.T) {
		vms := mm.VMs{{Name: "a", State: "RUNNING"}}

		err := launchErrors(vms, []string{"a", "gone"})
		if err == nil || !strings.Contains(err.Error(), "VM gone was not launched by minimega") {
			t.Fatalf("launchErrors() = %v, want an error naming gone", err)
		}

		if strings.Contains(err.Error(), "VM a ") {
			t.Errorf("launchErrors() = %q, reports a launched VM", err)
		}
	})
}

func TestCaptureInProgress(t *testing.T) {
	captures := []mm.Capture{
		{VM: "k1", Interface: 0, Filepath: "/phenix/images/exp/files/a0.pcap"},
		{VM: "k2", Interface: 0, Filepath: "exp/files/sub/b.pcap"},
	}

	tests := map[string]bool{
		"a0.pcap":     true,
		"/a0.pcap":    true,
		"sub/b.pcap":  true,
		"b.pcap":      false, // a different file than sub/b.pcap
		"a0.pcap.txt": false,
		"0.pcap":      false, // substring of a capture's name, not the same file
		"scorch.tgz":  false,
	}

	for file, want := range tests {
		if got := captureInProgress(captures, file); got != want {
			t.Errorf("captureInProgress(%q) = %v, want %v", file, got, want)
		}
	}
}
