//go:build e2e

package e2e

import (
	"crypto/rand"
	"encoding/base64"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"
	"unicode"
)

// ccWait bounds how long ccExec and guestShell wait for a guest's answer.
const ccWait = 2 * time.Minute

// waitCCClientsWithin is waitCCClients with an explicit boot budget, for
// images that boot slower than the general-purpose guest (desktop images).
func waitCCClientsWithin(t *testing.T, ns string, timeout time.Duration, hostnames ...string) {
	t.Helper()

	eventually(t, timeout, 2*time.Second, fmt.Sprintf("cc clients %v", hostnames), func() (bool, string) {
		rows := mmTable(t, ns, "cc clients")
		have := make([]string, 0, len(rows))

		for _, row := range rows {
			have = append(have, row["hostname"])
		}

		for _, h := range hostnames {
			if !slices.Contains(have, h) {
				return false, fmt.Sprintf("have %v", have)
			}
		}

		return true, ""
	})
}

// ccRun is ccExec waiting up to wait for the answer. When nonce is not empty,
// only a response whose stdout starts with a line holding nonce answers this
// command, and ccRun returns its stdout without that line.
func ccRun(t *testing.T, ns, vm string, wait time.Duration, nonce string, argv []string) string {
	t.Helper()

	for _, arg := range argv {
		if err := ccArgSafe(arg); err != nil {
			t.Fatalf("cc exec in %s: %v", vm, err)
		}
	}

	uuid := strings.ToLower(vmInfo(t, ns)[vm]["uuid"])
	if uuid == "" {
		t.Fatalf("VM %s not found in namespace %s", vm, ns)
	}

	mm(t, ns, "cc filter uuid="+uuid)

	resp, err := mmRun(ns, "cc exec "+quoteArgs(argv))

	mm(t, ns, "clear cc filter")

	if err != nil {
		t.Fatal(err)
	}

	if len(resp) == 0 {
		t.Fatalf("cc exec in %s returned no response", vm)
	}

	id := strings.TrimSpace(string(resp[0].Data))
	if _, err := strconv.Atoi(id); err != nil {
		t.Fatalf("cc exec in %s: command id %q: %v", vm, id, err)
	}

	defer ccDeleteCommand(t, ns, id)

	ccWantFilter(t, ns, id, uuid)

	base := filepath.Join(cfg.imagesDir, ns, "miniccc_responses", id)

	return ccStdout(t, base, uuid, fmt.Sprintf("cc command %s from %s", id, vm), nonce, wait)
}

// ccDeleteCommand deletes cc command id of namespace ns. A command something
// else deleted already (a SCORCH cc reset clears every command) is gone as
// wanted.
func ccDeleteCommand(t *testing.T, ns, id string) {
	t.Helper()

	if _, err := mmRun(ns, "cc delete command "+id); err != nil && !strings.Contains(err.Error(), "command "+id+" not found") {
		t.Errorf("deleting cc command %s of %s: %v", id, ns, err)
	}
}

// ccWantFilter fails the test unless cc command id is for the client with
// the given UUID alone. The cc filter belongs to the namespace, and phenix
// sets it too (ExecC2Command, SCORCH components), so another process can
// change it between the harness's cc filter and its cc exec.
func ccWantFilter(t *testing.T, ns, id, uuid string) {
	t.Helper()

	for _, row := range mmTable(t, ns, "cc commands") {
		if row["id"] != id {
			continue
		}

		if want := "uuid=" + uuid; row["filter"] != want {
			t.Fatalf("cc command %s has the filter %q, want %q: another process set the cc filter of %s while the harness used it",
				id, row["filter"], want, ns)
		}

		return
	}

	t.Fatalf("cc command %s is not listed in namespace %s", id, ns)
}

// ccStdout waits up to wait for the response of the client with the given
// UUID in base, minimega's response directory of one cc command, and returns
// its stdout; what names the command and the VM. minimega counts a response
// before it writes its files (the exit code, then stdout and stderr, each only
// when the command wrote some; responseHandler, internal/ron/server.go), so
// the response is complete once stdout ends with a newline, or one poll after
// the exit code appeared. With a nonce (see ccRun), a stdout that does not
// start with the nonce's line belongs to another command and is waited past.
func ccStdout(t *testing.T, base, uuid, what, nonce string, wait time.Duration) string {
	t.Helper()

	var (
		marker   = nonce + "\n"
		out      string
		exitSeen bool
	)

	eventually(t, wait, time.Second, "response to "+what, func() (bool, string) {
		dir := ccResponseDir(base, uuid)
		if dir == "" {
			return false, "no response from " + uuid + " in " + base
		}

		data, err := os.ReadFile(filepath.Join(dir, "stdout")) //nolint:gosec // minimega's response directory
		if err != nil && !errors.Is(err, fs.ErrNotExist) {
			return false, err.Error()
		}

		out = string(data)

		if nonce != "" && !strings.HasPrefix(out, marker) {
			// This command's exit code is written before its stdout, so an
			// exit code seen now may be another command's.
			exitSeen = false

			return false, fmt.Sprintf("stdout %q is not the answer to this command, which starts with %s", out, nonce)
		}

		if strings.HasSuffix(out, "\n") || exitSeen {
			return true, ""
		}

		_, err = os.Stat(filepath.Join(dir, "exitcode"))
		exitSeen = err == nil

		return false, fmt.Sprintf("stdout %q has no final newline; exit code recorded: %t", out, exitSeen)
	})

	if nonce != "" {
		return strings.TrimPrefix(out, marker)
	}

	return out
}

// ccResponseDir returns the directory under base that holds the response of
// the client with the given UUID, or "" when there is none yet.
func ccResponseDir(base, uuid string) string {
	entries, err := os.ReadDir(base)
	if err != nil {
		return ""
	}

	for _, e := range entries {
		if e.IsDir() && strings.EqualFold(e.Name(), uuid) {
			return filepath.Join(base, e.Name())
		}
	}

	return ""
}

// TestC2CCStdoutNonce checks, without a guest, that ccStdout with a nonce
// waits past the complete response an earlier command with the same ID left
// in the response directory, and returns the answer that replaces it without
// the nonce's line.
func TestC2CCStdoutNonce(t *testing.T) {
	const uuid = "4f1c2e9a-7b3d-4c5e-9f60-1a2b3c4d5e6f"

	for _, tc := range []struct {
		name, answer, want string
	}{
		{"answer with a final newline", "NEW ANSWER\n", "NEW ANSWER\n"},
		{"answer without a final newline", "NEW ANSWER", "NEW ANSWER"},
		{"only the nonce", "", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var (
				base  = t.TempDir()
				dir   = filepath.Join(base, uuid)
				nonce = "e2e-cc-" + rand.Text()
			)

			if err := os.Mkdir(dir, 0o750); err != nil {
				t.Fatal(err)
			}

			for name, data := range map[string]string{"exitcode": "0", "stdout": "OLD ANSWER\n"} {
				if err := os.WriteFile(filepath.Join(dir, name), []byte(data), 0o600); err != nil {
					t.Fatal(err)
				}
			}

			// The answer replaces the stale stdout after a few of ccStdout's
			// one-second polls, in one rename so that no poll reads half of it.
			written := make(chan error, 1)

			go func() {
				time.Sleep(2500 * time.Millisecond)

				tmp := filepath.Join(base, "stdout.new")
				if err := os.WriteFile(tmp, []byte(nonce+"\n"+tc.answer), 0o600); err != nil {
					written <- err

					return
				}

				written <- os.Rename(tmp, filepath.Join(dir, "stdout"))
			}()

			got := ccStdout(t, base, uuid, "a test command from no VM", nonce, 30*time.Second)

			if err := <-written; err != nil {
				t.Fatalf("writing the answer: %v", err)
			}

			if got != tc.want {
				t.Errorf("ccStdout returned %q, want %q", got, tc.want)
			}
		})
	}
}

// quoteArgs joins argv for a minimega command line. minicli parses the line
// twice: minimega rebuilds a namespaced cc command from the parsed arguments,
// quoting only those that contain whitespace, and parses it again
// (namespaceCommands, minimega cmd/minimega/cli.go). quoteArgs quotes the same
// way, so every argument ccArgSafe accepts reaches the guest unchanged.
func quoteArgs(argv []string) string {
	quoted := make([]string, len(argv))

	for i, a := range argv {
		if strings.IndexFunc(a, unicode.IsSpace) >= 0 {
			quoted[i] = strconv.Quote(a)
		} else {
			quoted[i] = a
		}
	}

	return strings.Join(quoted, " ")
}

// guestShell runs a shell script in VM vm and returns its stdout. The script
// first prints a line no other command prints, and only a response that starts
// with it is taken: minimega numbers commands from 1 again after `clear cc
// commands` (a SCORCH cc reset), never deletes a command's responses with it,
// and records a response that arrives after its command was deleted, so the
// response directory of a new command can already hold, or later receive, the
// answer of an earlier command with the same ID.
func guestShell(t *testing.T, ns, vm, script string) string {
	t.Helper()

	return guestShellWithin(t, ns, vm, ccWait, script)
}

// guestShellWithin is guestShell waiting up to wait for the answer, for a
// script that can run longer than ccWait.
func guestShellWithin(t *testing.T, ns, vm string, wait time.Duration, script string) string {
	t.Helper()

	nonce := "e2e-cc-" + rand.Text()

	return ccRun(t, ns, vm, wait, nonce, guestScript("echo "+nonce+"\n"+script))
}

// guestScript returns the argv that runs script with bash in a guest. minimega
// rewrites command arguments before they reach the guest: it expands $name and
// ${...} from its own environment, fetches arguments that look like URLs, and
// drops what follows an unquoted #. So the script travels base64-encoded, which
// contains none of those, and the guest decodes it; minimega leaves $(...)
// alone.
func guestScript(script string) []string {
	enc := base64.StdEncoding.EncodeToString([]byte(script))

	return []string{"bash", "-c", `eval "$(echo ` + enc + ` | base64 -d)"`}
}

// mmExpanded matches what minimega's command preprocessing (os.ExpandEnv in
// cliPreprocess, minimega cmd/minimega/cli.go) replaces with the daemon's
// environment: $name, ${...} and the special parameters.
var mmExpanded = regexp.MustCompile(`\$[A-Za-z0-9_{*#@!?$-]`)

// mmURLArg matches the arguments minimega's preprocessing fetches or unpacks
// instead of passing through.
var mmURLArg = regexp.MustCompile(`^(?i)(file|https?|tar):`)

// ccArgSafe rejects an argument that minimega would rewrite before the guest
// sees it, which would make a guest check pass or fail for the wrong reason.
// $(...) command substitution survives and is allowed. An argument without
// whitespace reaches minimega's second parse unquoted (see quoteArgs), where
// a # ends the command, quotes are dropped and a backslash escapes, and an
// empty argument is dropped; guestScript carries anything else.
func ccArgSafe(arg string) error {
	if m := mmExpanded.FindString(arg); m != "" {
		return fmt.Errorf("argument %q uses %q, which minimega expands from its own environment before the guest sees it", arg, m)
	}

	if mmURLArg.MatchString(arg) {
		return fmt.Errorf("argument %q starts with a URL scheme, which minimega fetches instead of passing through", arg)
	}

	if arg == "" {
		return errors.New("an empty argument, which minimega drops")
	}

	if strings.IndexFunc(arg, unicode.IsSpace) < 0 && strings.ContainsAny(arg, "#\\\"'") {
		return fmt.Errorf("argument %q has a #, quote or backslash and no whitespace, which minimega's parser rewrites", arg)
	}

	if strings.IndexFunc(arg, ccControl) >= 0 {
		return fmt.Errorf("argument %q has a control character, which minimega's parser cannot take", arg)
	}

	return nil
}

// ccControl reports the characters minicli has no escape for: everything
// unprintable but space, tab, newline and carriage return.
func ccControl(r rune) bool {
	return !unicode.IsPrint(r) && r != ' ' && r != '\t' && r != '\n' && r != '\r'
}
