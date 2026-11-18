package cmd

import (
	"errors"
	"io"
	"os"
	"strings"
	"testing"

	"github.com/spf13/cobra"

	"phenix/api/vm"
	"phenix/util/mm"
)

// partialListCmdTestMM lists the VM of the host that answered, labeled web,
// and the error of one that did not.
type partialListCmdTestMM struct {
	mm.MM
}

func (partialListCmdTestMM) GetVMInfoErr(...mm.Option) (mm.VMs, error) {
	return mm.VMs{{
		Name:    "test-vm",
		Host:    "c1",
		State:   "RUNNING",
		Running: true,
		Tags:    map[string]string{"web": "true"},
	}}, errors.New("c2: timed out")
}

// TestVMInfoByLabelShowsTheVMsOfHostsThatAnswered is a regression test:
// phenix vm info --label failed and showed nothing when one host of a
// running experiment did not answer minimega's vm info, where it had shown
// the matching VMs of the other hosts, while the commands that act on the
// VMs of a label must still fail.
func TestVMInfoByLabelShowsTheVMsOfHostsThatAnswered(t *testing.T) {
	installCaptureCmdTestExperiment(t)

	original := mm.DefaultMM
	t.Cleanup(func() { mm.DefaultMM = original }) //nolint:reassign // restore test double

	mm.DefaultMM = partialListCmdTestMM{} //nolint:reassign // install test double

	vms, err := listVMsByLabel("test-experiment", []string{"web"})
	if !errors.Is(err, vm.ErrPartialList) || len(vms) != 1 || vms[0].Name != "test-vm" {
		t.Errorf("listVMsByLabel(web) = %v, %v, want test-vm with vm.ErrPartialList", vms, err)
	}

	_, err = listVMsByLabel("test-experiment", []string{"db"})
	if err == nil || errors.Is(err, vm.ErrPartialList) || !strings.Contains(err.Error(), "c2: timed out") {
		t.Errorf("listVMsByLabel(db) = %v, want an error naming the failed host", err)
	}

	acting := &cobra.Command{}
	acting.Flags().StringArray("label", nil, "")

	if err := acting.Flags().Set("label", "web"); err != nil {
		t.Fatal(err)
	}

	if _, names, err := vmTargetNamesForCommand(acting, []string{"test-experiment"}); err == nil {
		t.Errorf("vmTargetNamesForCommand(--label web) = %v, nil, want the partial list's error", names)
	}

	info := newVMInfoCmd()
	if err := info.Flags().Set("label", "web"); err != nil {
		t.Fatal(err)
	}

	out := captureStdout(t, func() {
		if err := info.RunE(info, []string{"test-experiment"}); err != nil {
			t.Errorf("phenix vm info --label web = %v, want the VMs the other hosts listed", err)
		}
	})

	if !strings.Contains(out, "test-vm") {
		t.Errorf("phenix vm info --label web printed %q, want test-vm", out)
	}
}

// captureStdout returns what fn writes to [os.Stdout].
func captureStdout(t *testing.T, fn func()) string {
	t.Helper()

	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}

	original := os.Stdout
	os.Stdout = w //nolint:reassign // capture output

	defer func() { os.Stdout = original }() //nolint:reassign // restore output

	read := make(chan string)

	go func() {
		b, _ := io.ReadAll(r)
		read <- string(b)
	}()

	fn()

	_ = w.Close()

	return <-read
}
