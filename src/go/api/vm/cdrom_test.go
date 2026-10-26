package vm_test

import (
	"errors"
	"path"
	"slices"
	"strings"
	"testing"

	"github.com/activeshadow/libminimega/minicli"

	"phenix/api/vm"
)

// TestChangeOpticalDiscRefusesHostFiles is a regression test: the optical
// disc path of a REST cdrom change reached minimega verbatim, so a user
// allowed to change discs had QEMU, which runs as root, attach a host file
// such as /etc/shadow to the guest, and a quote made minimega exit when it
// parsed the namespaced command again.
func TestChangeOpticalDiscRefusesHostFiles(t *testing.T) {
	const files = "/srv/minimega/files"

	useRedeployExperiment(t, t.TempDir(), nil)

	vm.SetMMFullPath(t, func(p string) string {
		if path.IsAbs(p) {
			return p
		}

		return path.Join(files, p)
	})

	for _, iso := range []string{
		"/etc/shadow",
		"../../etc/shadow",
		`x"y.iso`,
		"a.iso b.iso",
		"file:/etc/shadow",
		"test-experiment/files/evil.iso",
		"head_other_victim_snapshot",
		"/data/images/win10.iso",
	} {
		fake := useFakeMinimega(t, nil)

		err := vm.ChangeOpticalDisc("test-experiment", "test-vm", iso)
		if !errors.Is(err, vm.ErrInvalidDisk) || !strings.Contains(err.Error(), "VM test-vm") {
			t.Errorf("ChangeOpticalDisc(%q) = %v, want vm.ErrInvalidDisk naming test-vm", iso, err)
		}

		if sent := fake.sent(); len(sent) != 0 {
			t.Errorf("ChangeOpticalDisc(%q) sent %q to minimega", iso, sent)
		}
	}

	// minimega looks a relative path up in its working directory, so phenix
	// sends the path in the files directory it checked.
	for iso, want := range map[string]string{
		"tools.iso":            files + "/tools.iso",
		files + "/./tools.iso": files + "/tools.iso",
		"test.qc2":             files + "/test.qc2", // a topology disk
	} {
		fake := useFakeMinimega(t, func(string) minicli.Responses { return minicli.Responses{{Host: "head"}} })

		if err := vm.ChangeOpticalDisc("test-experiment", "test-vm", iso); err != nil {
			t.Errorf("ChangeOpticalDisc(%q) = %v", iso, err)
		}

		if cmd := "vm cdrom change test-vm " + want; !slices.ContainsFunc(fake.sent(), func(s string) bool {
			return strings.HasSuffix(s, cmd)
		}) {
			t.Errorf("ChangeOpticalDisc(%q) sent %q, want a command ending in %q", iso, fake.sent(), cmd)
		}
	}
}
