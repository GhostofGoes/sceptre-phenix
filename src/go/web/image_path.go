package web

import (
	"errors"
	"fmt"
	"path/filepath"
	"regexp"
	"strings"
	"unicode"

	"phenix/api/experiment"
	"phenix/api/vm"
	"phenix/types"
	"phenix/util/mm"
)

var (
	errImageOutsideFiles = errors.New("image must be in the minimega files directory")
	errImageOtherExp     = errors.New("image belongs to another experiment")
	errImageUnsafe       = errors.New("image path has characters or a prefix minimega expands")

	// minimega rewrites file:, http(s):// and tar: arguments before it runs a
	// command, so a value like that is never the path it appears to be.
	imageSchemeRe = regexp.MustCompile(`^[A-Za-z][A-Za-z0-9+.-]*:`)
)

// validateImagePath checks a disk or ISO image named in a request before it is
// attached to a VM in expName. minimega opens it on the host as root, so a path
// anywhere else, such as a host block device, would hand the guest that file.
// The image must be one of the known images (the VM's current disk and the
// images in the experiment's topology), or resolve inside the minimega files
// directory and not under another experiment's "<exp>/files" directory. A
// ",<cache mode>" suffix, as minimega disk configs allow, is ignored. An empty
// path means no change and is accepted.
func validateImagePath(expName, image string, known ...string) error {
	path, _, _ := strings.Cut(image, ",")
	if path == "" {
		return nil
	}

	for _, k := range known {
		if k, _, _ = strings.Cut(k, ","); k != "" && k == path {
			return nil
		}
	}

	if imageSchemeRe.MatchString(path) || strings.IndexFunc(image, unsafeImageRune) >= 0 {
		return fmt.Errorf("%w: %q", errImageUnsafe, image)
	}

	dir := filepath.Clean(mm.GetMMFilesDirectory())

	if !filepath.IsAbs(path) {
		path = filepath.Join(dir, path)
	}

	rel, err := filepath.Rel(dir, filepath.Clean(path))
	if err != nil || rel == "." || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return fmt.Errorf("%w: %s", errImageOutsideFiles, image)
	}

	if parts := strings.Split(rel, string(filepath.Separator)); len(parts) > 2 &&
		parts[1] == "files" && parts[0] != expName {
		return fmt.Errorf("%w: %s", errImageOtherExp, image)
	}

	return nil
}

// validateISOPath is validateImagePath for a CD-ROM image, which takes no
// cache mode, so a comma is part of the path.
func validateISOPath(expName, vmName, image string) error {
	if strings.Contains(image, ",") {
		return fmt.Errorf("%w: %q", errImageUnsafe, image)
	}

	return validateVMImage(expName, vmName, image)
}

// validateVMImage is validateImagePath with the VM's current disk and the
// experiment's topology images allowed.
func validateVMImage(expName, vmName, image string) error {
	if image == "" {
		return nil
	}

	exp, err := experiment.Get(expName)
	if err != nil {
		return validateImagePath(expName, image)
	}

	known := topologyImages(exp)

	// A redeploy can leave the running disk different from the topology's.
	if v, err := vm.Get(expName, vmName); err == nil {
		known = append(known, v.Disk)
	}

	return validateImagePath(expName, image, known...)
}

// topologyImages lists the drive images the experiment's topology names.
func topologyImages(exp *types.Experiment) []string {
	var images []string

	if exp == nil || exp.Spec == nil || exp.Spec.Topology() == nil {
		return images
	}

	for _, node := range exp.Spec.Topology().Nodes() {
		if node.Hardware() == nil {
			continue
		}

		for _, drive := range node.Hardware().Drives() {
			if img := drive.Image(); img != "" {
				images = append(images, img)
			}
		}
	}

	return images
}

func unsafeImageRune(r rune) bool {
	return strings.ContainsRune("\"'\\#$", r) || (r != ' ' && !unicode.IsPrint(r))
}
