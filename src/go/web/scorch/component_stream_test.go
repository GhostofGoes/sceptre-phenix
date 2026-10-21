package scorch

import (
	"bytes"
	"errors"
	"io"
	"strings"
	"sync"
	"testing"
	"time"
	"unicode/utf8"

	"golang.org/x/net/websocket"

	"phenix/web/rbac"
)

var startProcessors sync.Once //nolint:gochecknoglobals // one set of processors per test binary

// useProcessors starts the SCORCH processors once per test binary, with a
// write deadline for output streams short enough for a test to wait out.
func useProcessors() {
	startProcessors.Do(func() {
		streamWriteWait = 500 * time.Millisecond

		Start("")
	})
}

// TestComponentOutputStreams is a regression test: component output streams
// were added by one goroutine, which read the output and running maps, while
// another wrote them with each component update and read the map of
// streams, so a client opening a stream of a component printing output could
// end phenix ui with a concurrent map read and write. Run it with -race.
func TestComponentOutputStreams(t *testing.T) {
	useProcessors()

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

// readStream reads stream onto got until got ends with want, and returns
// got.
func readStream(t *testing.T, stream *websocket.Conn, got []byte, want string) []byte {
	t.Helper()

	_ = stream.SetReadDeadline(time.Now().Add(5 * time.Second))

	buf := make([]byte, 4096)

	for !bytes.HasSuffix(got, []byte(want)) {
		n, err := stream.Read(buf)

		got = append(got, buf[:n]...)

		if err != nil {
			t.Fatalf("reading the stream after %q, want %q: %v", got, want, err)
		}
	}

	return got
}

// TestComponentOutputStreamOrder checks that a stream sends the output a
// component printed before it opened, then the output it prints after, then
// the notice that it finished, and ends.
func TestComponentOutputStreamOrder(t *testing.T) {
	useProcessors()

	srv := terminalTestServer(t, map[string]rbac.Role{"admin": testRole([2]string{"*", "*"}, [2]string{"*/*", "*"})})

	update := ComponentUpdate{
		Exp: "order-exp", CmpName: "printer", CmpType: "user", Stage: "start", Status: statusStart,
	}
	UpdateComponent(update)

	update.Status = statusRunning
	update.Output = []byte("replay\n")
	UpdateComponent(update)

	stream, err := openTerminal(srv, "admin", "api/v1/experiments/order-exp/scorch/components/0/0/start/printer/ws")
	if err != nil {
		t.Fatalf("opening the component output stream: %v", err)
	}

	defer func() { _ = stream.Close() }()

	got := readStream(t, stream, nil, "replay\n")

	update.Output = []byte("live\n")
	UpdateComponent(update)

	got = readStream(t, stream, got, "live\n")

	update.Status = statusSuccess
	update.Output = nil
	UpdateComponent(update)

	got = readStream(t, stream, got, "***** COMPONENT FINISHED *****")

	if want := "replay\nlive\n***** COMPONENT FINISHED *****"; string(got) != want {
		t.Errorf("the stream sent %q, want %q", got, want)
	}

	if n, err := stream.Read(make([]byte, 64)); !errors.Is(err, io.EOF) {
		t.Errorf("after the notice the stream sent %d bytes (%v), want it to end", n, err)
	}
}

// TestComponentOutputStreamFrames checks that a stream sends a large output
// whole, in frames no larger than streamFrameSize that each hold valid
// UTF-8, as the text frames a browser accepts must.
func TestComponentOutputStreamFrames(t *testing.T) {
	useProcessors()

	srv := terminalTestServer(t, map[string]rbac.Role{"admin": testRole([2]string{"*", "*"}, [2]string{"*/*", "*"})})

	// The odd first byte puts each frame boundary inside a two-byte rune.
	want := "a" + strings.Repeat("é", 512*1024)

	update := ComponentUpdate{Exp: "frames-exp", CmpName: "printer", CmpType: "user", Stage: "start", Status: statusStart}
	UpdateComponent(update)

	update.Status = statusRunning
	update.Output = []byte(want)
	UpdateComponent(update)

	update.Status = statusSuccess
	update.Output = nil
	UpdateComponent(update)

	stream, err := openTerminal(srv, "admin", "api/v1/experiments/frames-exp/scorch/components/0/0/start/printer/ws")
	if err != nil {
		t.Fatalf("opening the component output stream: %v", err)
	}

	defer func() { _ = stream.Close() }()

	_ = stream.SetReadDeadline(time.Now().Add(10 * time.Second))

	var got strings.Builder

	for {
		var frame string

		if err := websocket.Message.Receive(stream, &frame); err != nil {
			t.Fatalf("receiving a frame after %d bytes: %v", got.Len(), err)
		}

		if frame == "***** COMPONENT FINISHED *****" {
			break
		}

		if len(frame) > streamFrameSize || !utf8.ValidString(frame) {
			t.Fatalf("frame of %d bytes, valid UTF-8 %t; want at most %d bytes of valid UTF-8",
				len(frame), utf8.ValidString(frame), streamFrameSize)
		}

		got.WriteString(frame)
	}

	if got.String() != want {
		t.Errorf("the stream sent %d bytes, want the %d bytes of output", got.Len(), len(want))
	}
}
