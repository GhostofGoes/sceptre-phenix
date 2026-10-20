package scorch

import (
	"strings"
	"sync"
	"testing"

	"phenix/web/rbac"
)

var startProcessors sync.Once //nolint:gochecknoglobals // one set of processors per test binary

// TestComponentOutputStreams is a regression test: component output streams
// were added by one goroutine, which read the output and running maps, while
// another wrote them with each component update and read the map of
// streams, so a client opening a stream of a component printing output could
// end phenix ui with a concurrent map read and write. Run it with -race.
func TestComponentOutputStreams(t *testing.T) {
	startProcessors.Do(func() { Start("") })

	srv := terminalTestServer(t, map[string]rbac.Role{"admin": testRole([2]string{"*", "*"}, [2]string{"*/*", "*"})})

	update := ComponentUpdate{
		Exp: "stream-exp", CmpName: "printer", CmpType: "user", Stage: "start", Status: statusStart,
	}
	UpdateComponent(update)

	update.Status = statusRunning
	update.Output = []byte("line\n")
	UpdateComponent(update)

	var (
		printer sync.WaitGroup
		stop    = make(chan struct{})
	)

	printer.Add(1)

	go func() {
		defer printer.Done()

		for {
			select {
			case <-stop:
				return
			default:
				UpdateComponent(update)
			}
		}
	}()

	for range 50 {
		stream, err := openTerminal(srv, "admin", "api/v1/experiments/stream-exp/scorch/components/0/0/start/printer/ws")
		if err != nil {
			t.Fatalf("opening the component output stream: %v", err)
		}

		buf := make([]byte, 64)

		n, err := stream.Read(buf)
		if err != nil || !strings.Contains(string(buf[:n]), "line") {
			t.Errorf("the stream sent %q (%v), want the component's output", buf[:n], err)
		}

		_ = stream.Close()
	}

	close(stop)
	printer.Wait()

	update.Status = statusSuccess
	update.Output = nil
	UpdateComponent(update)
}
