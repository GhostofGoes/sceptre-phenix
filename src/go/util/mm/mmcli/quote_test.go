package mmcli

import (
	"errors"
	"slices"
	"sync"
	"testing"

	"github.com/activeshadow/libminimega/minicli"
)

var registerPatterns sync.Once //nolint:gochecknoglobals // minicli handlers are process-wide

// registerMinimegaPatterns registers handlers with the same patterns minimega
// uses, so that tests parse command strings exactly as minimega would. The
// .filter, .columns, and .record builtins come with minicli itself.
func registerMinimegaPatterns() {
	registerPatterns.Do(func() {
		nop := func(*minicli.Command, chan<- minicli.Responses) {}

		minicli.MustRegister(&minicli.Handler{
			Patterns: []string{"namespace [name]", "namespace <name> (command)"},
			Call:     nop,
		})
		minicli.MustRegister(&minicli.Handler{Patterns: []string{"vm info [summary,]"}, Call: nop})
		minicli.MustRegister(&minicli.Handler{Patterns: []string{"vm start <vm target>"}, Call: nop})
		minicli.MustRegister(&minicli.Handler{Patterns: []string{"shell <command>..."}, Call: nop})
	})
}

// awkwardValues are names minimega's lexer would split, truncate, or unescape
// if they reached it unquoted.
func awkwardValues() []string {
	return []string{
		"vm1",
		"",
		"vm 1",
		"vm1 # trailing comment",
		"vm1 vm info",
		`say "hi"`,
		"it's",
		`back\slash`,
		"tab\tand\nnewline",
		"-starts-with-dash",
	}
}

func TestQuoteArgRoundTripsThroughMinimegaLexer(t *testing.T) {
	registerMinimegaPatterns()

	for _, value := range awkwardValues() {
		c, err := minicli.Compile("shell echo " + QuoteArg(value))
		if err != nil {
			t.Fatalf("compiling quoted %q: %v", value, err)
		}

		want := []string{"echo", value}
		if got := c.ListArgs["command"]; !slices.Equal(got, want) {
			t.Errorf("value %q: minimega would read %q, want %q", value, got, want)
		}
	}
}

func TestQuoteArgsKeepsEachArgumentSeparate(t *testing.T) {
	registerMinimegaPatterns()

	c, err := minicli.Compile("shell " + QuoteArgs(awkwardValues()))
	if err != nil {
		t.Fatalf("compiling: %v", err)
	}

	if got := c.ListArgs["command"]; !slices.Equal(got, awkwardValues()) {
		t.Fatalf("minimega would read %q, want %q", got, awkwardValues())
	}
}

// TestCommandStringKeepsFilterAndNamespaceIntact checks that a filter value or
// namespace cannot end its argument early. .filter and namespace both take
// the rest of the line as a nested command, so a value that ended early would
// run whatever followed it as a command of its own.
func TestCommandStringKeepsFilterAndNamespaceIntact(t *testing.T) {
	registerMinimegaPatterns()

	for _, value := range awkwardValues() {
		cmd := NewNamespacedCommand("exp " + value)
		cmd.Command = "vm info"
		cmd.Columns = []string{"host", "name"}
		cmd.Filters = []string{"type=kvm", "name=" + value}

		c, err := minicli.Compile(cmd.String())
		if err != nil {
			t.Fatalf("value %q: compiling %q: %v", value, cmd.String(), err)
		}

		// .record false namespace <ns> .columns <cols> .filter <name> .filter <type> vm info
		ns := c.Subcommand
		if ns == nil || ns.StringArgs["name"] != "exp "+value {
			t.Fatalf("value %q: namespace not parsed intact from %q", value, cmd.String())
		}

		var filters []string

		leaf := ns.Subcommand.Subcommand // skip .columns
		for leaf != nil && leaf.StringArgs["filter"] != "" {
			filters = append(filters, leaf.StringArgs["filter"])
			leaf = leaf.Subcommand
		}

		if want := []string{"name=" + value, "type=kvm"}; !slices.Equal(filters, want) {
			t.Errorf("value %q: filters %q, want %q", value, filters, want)
		}

		if leaf == nil || leaf.Original != "vm info" || leaf.Subcommand != nil {
			t.Errorf("value %q: innermost command is not `vm info` in %q", value, cmd.String())
		}
	}
}

func TestCheckArg(t *testing.T) {
	allowed := []string{
		"", "vm1", "vm 1", "exp_1-a.b", "foo@bar", "/phenix/images/disk.qc2",
		"relative/path.qc2", "ünïcödé", "-starts-with-dash", "C:drive",
	}

	for _, value := range allowed {
		if err := CheckArg(value); err != nil {
			t.Errorf("CheckArg(%q) = %v, want nil", value, err)
		}
	}

	rejected := []string{
		`say "hi"`, "it's", `back\slash`, "vm1#x", "$HOME/disk", "${HOME}",
		"tab\there", "new\nline", "nul\x00", "soft\u00adhyphen", "zero\u200bwidth",
		"nbsp\u00a0space", "file:disk.qc2", "FILE:disk.qc2", "http://host/disk.qc2",
		"https://host/disk.qc2", "HTTP:x", "tar:/x.tar",
	}

	for _, value := range rejected {
		if err := CheckArg(value); !errors.Is(err, ErrUnsafeArg) {
			t.Errorf("CheckArg(%q) = %v, want ErrUnsafeArg", value, err)
		}
	}
}

// TestCheckedArgsSurviveNestedReparse checks that values CheckArg accepts come
// through intact when minimega rebuilds a nested command from its parsed
// arguments and parses it again, as namespace and mesh send do.
func TestCheckedArgsSurviveNestedReparse(t *testing.T) {
	registerMinimegaPatterns()

	for _, value := range []string{"vm1", "vm 1", "exp_1-a.b", "ünïcödé", "/phenix/images/a b.qc2"} {
		if err := CheckArg(value); err != nil {
			t.Fatalf("CheckArg(%q) = %v", value, err)
		}

		outer, err := minicli.Compile("namespace exp shell echo " + QuoteArg(value))
		if err != nil {
			t.Fatalf("value %q: first parse: %v", value, err)
		}

		inner, err := minicli.Compile(outer.Subcommand.Original)
		if err != nil {
			t.Fatalf("value %q: reparse of %q: %v", value, outer.Subcommand.Original, err)
		}

		if got, want := inner.ListArgs["command"], []string{"echo", value}; !slices.Equal(got, want) {
			t.Errorf("value %q: reparse reads %q, want %q", value, got, want)
		}
	}
}

// TestRejectedArgsDoNotSurviveNestedReparse documents why CheckArg is needed on
// top of QuoteArg: these values pass the first parse once quoted, but the
// rebuilt nested command no longer parses, or parses to something else.
func TestRejectedArgsDoNotSurviveNestedReparse(t *testing.T) {
	registerMinimegaPatterns()

	for _, value := range []string{`a"b`, "vm1#x", `a\b`, "x y\u00ad"} {
		outer, err := minicli.Compile("namespace exp shell echo " + QuoteArg(value))
		if err != nil {
			t.Fatalf("value %q: first parse: %v", value, err)
		}

		inner, err := minicli.Compile(outer.Subcommand.Original)
		if err == nil && slices.Equal(inner.ListArgs["command"], []string{"echo", value}) {
			t.Errorf("value %q unexpectedly survives the reparse; CheckArg may not need to reject it", value)
		}

		if CheckArg(value) == nil {
			t.Errorf("CheckArg(%q) accepted a value that does not survive the reparse", value)
		}
	}
}
