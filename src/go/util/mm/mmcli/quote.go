package mmcli

import (
	"errors"
	"fmt"
	"strings"
	"unicode"
)

// ErrUnsafeArg is returned by CheckArg for a value minimega cannot be trusted
// to read back unchanged.
var ErrUnsafeArg = errors.New("value is not safe to pass to minimega")

// QuoteArgs quotes each argument with QuoteArg and joins them with spaces.
func QuoteArgs(args []string) string {
	quoted := make([]string, len(args))

	for i, arg := range args {
		quoted[i] = QuoteArg(arg)
	}

	return strings.Join(quoted, " ")
}

// QuoteArg quotes arg so that minimega's command lexer reads it back as one
// unchanged argument. Outside quotes the lexer splits on whitespace, starts a
// quoted string at `"` or `'`, treats `\` as an escape, and drops everything
// from `#` on as a comment. Inside double quotes only `"` and `\` are special.
// An empty argument must be quoted or the lexer drops it.
//
// Quote every value that is not a fixed keyword before putting it in a
// command: an unquoted value can end the command early or, where a pattern
// ends in a nested command or a list, add arguments or run a second command.
func QuoteArg(arg string) string {
	if arg != "" && !strings.ContainsFunc(arg, needsQuote) {
		return arg
	}

	var b strings.Builder

	b.WriteByte('"')

	for _, r := range arg {
		switch r {
		case '"', '\\':
			b.WriteByte('\\')
			b.WriteRune(r)
		case '\n':
			b.WriteString(`\n`)
		case '\r':
			b.WriteString(`\r`)
		case '\t':
			b.WriteString(`\t`)
		default:
			b.WriteRune(r)
		}
	}

	b.WriteByte('"')

	return b.String()
}

func needsQuote(r rune) bool {
	return unicode.IsSpace(r) || strings.ContainsRune(`"'\#`, r)
}

// CheckArg rejects values that quoting alone cannot carry through minimega
// intact. Quoting protects the first parse, but minimega rebuilds some nested
// commands, such as those under namespace and mesh send, from their parsed
// arguments and parses them again, quoting only arguments that contain
// whitespace. A quote, backslash, or `#` then splits or truncates the value on
// the second parse, and a character the rebuilt quoting escapes as \x or \u
// makes minimega fail to parse its own command and exit. minimega also expands
// `$VAR` in arguments and acts on `file:`, `http:`, `https:`, and `tar:` arguments, so those
// are refused as well. Check every value from a request before it reaches a
// minimega command.
func CheckArg(arg string) error {
	for _, r := range arg {
		if strings.ContainsRune(`"'\#$`, r) || (r != ' ' && !unicode.IsPrint(r)) {
			return fmt.Errorf("%w: %q contains %q", ErrUnsafeArg, arg, r)
		}
	}

	// minimega acts on these URL schemes, which url.Parse matches in any case.
	lower := strings.ToLower(arg)

	for _, scheme := range []string{"file:", "http:", "https:", "tar:"} {
		if strings.HasPrefix(lower, scheme) {
			return fmt.Errorf("%w: %q looks like a URL", ErrUnsafeArg, arg)
		}
	}

	return nil
}

// CheckArgs calls CheckArg on each value and returns the first error.
func CheckArgs(args ...string) error {
	for _, arg := range args {
		if err := CheckArg(arg); err != nil {
			return err
		}
	}

	return nil
}
