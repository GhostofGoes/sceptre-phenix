package mm

import (
	"errors"
	"fmt"
	"regexp"
	"strings"
)

// ErrInvalidFileName is wrapped by the error CheckFileName returns for a name
// phenix refuses to pass to minimega.
var ErrInvalidFileName = errors.New("invalid file name")

// MaxFileNameLength bounds a name CheckFileName accepts, so that the files
// phenix names after it, such as <experiment>_<vm>__<name>.state, stay within
// the 255 bytes a file name may have.
const MaxFileNameLength = 128

// fileNameRegex is the allow-list of CheckFileName.
var fileNameRegex = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]*$`)

// CheckFileName returns an error wrapping ErrInvalidFileName, naming what and
// name, unless name is a plain file name: letters, digits, '.', '_' and '-',
// starting with a letter or digit, without "..", and at most
// MaxFileNameLength long.
//
// The names users give snapshots, captures and memory dumps end up in minimega
// `shell` commands, in QEMU's `exec:` migration URI, which QEMU runs through
// /bin/sh as root, and in namespaced commands that minimega rebuilds and parses
// again. Escaping cannot make every name safe in all of them, so only names
// that need no escaping anywhere are accepted.
func CheckFileName(what, name string) error {
	var problem string

	switch {
	case name == "":
		problem = "is empty"
	case len(name) > MaxFileNameLength:
		problem = fmt.Sprintf("is longer than %d characters", MaxFileNameLength)
	case !fileNameRegex.MatchString(name):
		problem = "may hold only letters, digits, '.', '_' and '-', and must start with a letter or digit"
	case strings.Contains(name, ".."):
		problem = `holds ".."`
	default:
		return nil
	}

	return fmt.Errorf("%s %q %s: %w", what, name, problem, ErrInvalidFileName)
}
