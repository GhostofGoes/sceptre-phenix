package mm

import (
	"errors"
	"fmt"
	"strconv"
	"strings"
	"unicode"
	"unicode/utf8"
)

// ErrUnsafeArgument is returned for a command that minimega cannot run in a
// namespace: one of its arguments would not survive minimega's rebuild of the
// command (see CheckVMTags), which exits minimega for every experiment.
var ErrUnsafeArgument = errors.New("minimega cannot carry this argument through a namespaced command")

// minicliEscapes maps the characters minicli accepts after a backslash to what
// they stand for (pkg/minicli/input.go escapedChars).
var minicliEscapes = map[rune]string{ //nolint:gochecknoglobals // immutable
	'r': "\r", 'n': "\n", 't': "\t", '\\': `\`, '"': `"`, '\'': `'`, '#': "#",
}

// CheckNamespacedCommand returns an error wrapping ErrUnsafeArgument and
// naming the argument when minimega cannot carry an argument of cmd, a command
// in minicli syntax, through its rebuild of a namespaced command, or would
// not run all of cmd: minicli ends a command at an unquoted #. It reads the
// arguments the way minicli does, so the check holds for quoted and escaped
// text too.
func CheckNamespacedCommand(cmd string) error {
	if !utf8.ValidString(cmd) {
		return fmt.Errorf("command %q is not valid UTF-8: %w", cmd, ErrUnsafeArgument)
	}

	args, comment, err := lexMinicli(cmd)
	if err != nil {
		return fmt.Errorf("command %q: %w: %w", cmd, err, ErrUnsafeArgument)
	}

	if comment != nil {
		return fmt.Errorf(
			"command %q: minimega would drop %q, after an unquoted #: %w", cmd, "#"+*comment, ErrUnsafeArgument,
		)
	}

	for _, arg := range args {
		if problem := vmTagProblem(arg); problem != "" {
			return fmt.Errorf("argument %q of command %q %s: %w", arg, cmd, problem, ErrUnsafeArgument)
		}
	}

	return nil
}

// CheckArgument returns an error wrapping ErrUnsafeArgument unless s, put
// into a namespaced command as written, reaches minimega as one argument
// that minimega carries through its rebuild of the command.
func CheckArgument(s string) error {
	if err := CheckNamespacedCommand(s); err != nil {
		return err
	}

	if args, _ := minicliArgs(s); len(args) != 1 || args[0] != s {
		return fmt.Errorf("%q is not one minimega argument as written: %w", s, ErrUnsafeArgument)
	}

	return nil
}

// minimegaWildcardVM is the VM target minimega expands to every VM of a
// namespace (Wildcard in minimega's cmd/minimega/main.go).
const minimegaWildcardVM = "all"

// CheckVMName returns an error wrapping ErrUnsafeArgument unless name, as the
// target of a minimega VM command such as `vm kill`, selects only the VM of
// that name. Besides the quoting CheckArgument checks, minimega reads a
// target as a list (a,b), a range (vm[1-3]), the wildcard all or a VM ID (a
// number) (VMs.apply in cmd/minimega/vms.go), each of which can select other
// VMs than a permission for the name allows. Topology validation refuses
// such hostnames, so no VM has one.
func CheckVMName(name string) error {
	if err := CheckArgument(name); err != nil {
		return err
	}

	if _, err := strconv.Atoi(name); err == nil {
		return fmt.Errorf("VM name %q is a number, which minimega reads as a VM ID: %w", name, ErrUnsafeArgument)
	}

	switch {
	case name == minimegaWildcardVM:
		return fmt.Errorf("VM name %q is minimega's wildcard for every VM: %w", name, ErrUnsafeArgument)
	case strings.ContainsAny(name, ",[]"):
		return fmt.Errorf(
			"VM name %q holds a comma or bracket, which minimega reads as a list or range of VMs: %w",
			name, ErrUnsafeArgument,
		)
	}

	return nil
}

// minicliArgs splits s into the arguments minicli lexes from it
// (pkg/minicli/input.go lexInput): runs of characters between whitespace,
// where '...' and "..." quote whitespace, a backslash escapes one of
// minicliEscapes, and an unquoted # ends the command. A quoted empty string
// is an argument of its own.
func minicliArgs(s string) ([]string, error) {
	args, _, err := lexMinicli(s)

	return args, err
}

// lexMinicli is minicliArgs that also returns the text after an unquoted #,
// which minicli drops, or nil when s holds no unquoted #.
func lexMinicli(s string) ([]string, *string, error) {
	var (
		args    []string
		arg     strings.Builder
		quoted  bool // arg holds a quoted string, which makes an argument even when empty
		quote   rune // the open quote, or 0
		escaped bool
		comment *string
	)

	flush := func() {
		if arg.Len() > 0 || quoted {
			args = append(args, arg.String())
		}

		arg.Reset()

		quoted = false
	}

scan:
	for i, r := range s {
		switch {
		case escaped:
			v, ok := minicliEscapes[r]
			if !ok {
				return nil, nil, fmt.Errorf("unexpected escaped character: %c", r)
			}

			arg.WriteString(v)

			escaped = false
		case r == '\\':
			escaped = true
		case quote != 0:
			if r == quote {
				quote, quoted = 0, true
			} else {
				arg.WriteRune(r)
			}
		case r == '"' || r == '\'':
			quote = r
		case r == '#':
			rest := s[i+1:]
			comment = &rest

			break scan
		case unicode.IsSpace(r):
			flush()
		default:
			arg.WriteRune(r)
		}
	}

	switch {
	case escaped:
		return nil, nil, errors.New("expected escape character")
	case quote != 0:
		return nil, nil, fmt.Errorf("missing terminal %c", quote)
	}

	flush()

	return args, comment, nil
}
