package scorch

import (
	"fmt"
	"io"
	"time"
	"unicode/utf8"

	"golang.org/x/net/websocket"
)

const (
	// componentFinished ends the output stream of a component that finished.
	componentFinished = "***** COMPONENT FINISHED *****"

	// streamFrameSize caps the frames of a component output stream, so
	// streamWriteWait bounds how long a client takes to read a frame, not a
	// whole replay.
	streamFrameSize = 64 * 1024
)

// streamWriteWait bounds each frame written to a component output stream: a
// client that has not taken it by then is dropped. Tests shorten it.
var streamWriteWait = 10 * time.Second //nolint:gochecknoglobals // tests shorten it

// streamUpdate is what a component output stream has to send: out, the
// component's output so far in generation gen of its output, and whether
// the component has finished.
type streamUpdate struct {
	out      []byte
	gen      int
	finished bool
}

// streamClient is the websocket client of a component output stream. The
// goroutine of its connection writes to it, so a client that stops reading
// never holds up processComponents.
type streamClient struct {
	key     string
	id      string
	updates chan streamUpdate // capacity 1: the latest update processComponents offers
}

// offer gives c the latest update without blocking, replacing one c has not
// taken yet: within a generation the output only grows, so the latest
// update holds all of an older one. Only processComponents sends on
// c.updates, so the send after the receive cannot block.
func (c *streamClient) offer(u streamUpdate) {
	select {
	case <-c.updates:
	default:
	}

	c.updates <- u
}

var (
	ws         map[string]map[string]*streamClient //nolint:gochecknoglobals // global state
	wsRequests chan *streamClient                  //nolint:gochecknoglobals // global state
	wsLeaves   chan *streamClient                  //nolint:gochecknoglobals // global state

	basePath string //nolint:gochecknoglobals // global state
)

// outputUpdate is the update of the component output streams of key. Its
// output is a full slice of the cached output: later appends write past it,
// so a stream goroutine may read it while processComponents appends.
func outputUpdate(key string, finished bool) streamUpdate {
	out := output[key]

	return streamUpdate{out: out[:len(out):len(out)], gen: outputGen[key], finished: finished}
}

// addOutputStream adds the websocket client of a component output stream,
// or, if the component is no longer running, gives it the whole output and
// ends it. processComponents calls it, so the component maps have one user.
func addOutputStream(cli *streamClient) {
	// Component is no longer running, so update and end this client.
	if !running[cli.key] {
		cli.offer(outputUpdate(cli.key, true))

		return
	}

	if _, ok := ws[cli.key]; !ok {
		ws[cli.key] = make(map[string]*streamClient)
	}

	ws[cli.key][cli.id] = cli

	// If there's already data, send it to the new client.
	if len(output[cli.key]) > 0 {
		cli.offer(outputUpdate(cli.key, false))
	}
}

// removeOutputStream removes the client of a component output stream whose
// connection has ended. processComponents calls it.
func removeOutputStream(cli *streamClient) {
	clients, ok := ws[cli.key]
	if !ok {
		return
	}

	delete(clients, cli.id)

	if len(clients) == 0 {
		delete(ws, cli.key)
	}
}

// writeStream writes b to conn in text frames of at most streamFrameSize
// bytes, each within streamWriteWait. A frame ends before a UTF-8 character
// starts, so a frame of valid text stays valid; a browser closes the
// connection on a frame that is not.
func writeStream(conn *websocket.Conn, b []byte) error {
	for len(b) > 0 {
		n := min(len(b), streamFrameSize)

		if n < len(b) {
			for i := n; i > n-utf8.UTFMax && i > 0; i-- {
				if utf8.RuneStart(b[i]) {
					n = i

					break
				}
			}
		}

		if err := conn.SetWriteDeadline(time.Now().Add(streamWriteWait)); err != nil {
			return fmt.Errorf("setting the write deadline: %w", err)
		}

		nw, err := conn.Write(b[:n])
		if err != nil {
			return fmt.Errorf("writing component output: %w", err)
		}

		if nw != n {
			return io.ErrShortWrite
		}

		b = b[n:]
	}

	return nil
}

// streamOutput writes the updates processComponents offers cli to conn until
// the component finishes, a write fails or the client goes away.
func streamOutput(conn *websocket.Conn, cli *streamClient) {
	// The client sends nothing; reading ends when it goes away.
	gone := make(chan struct{})

	go func() {
		defer close(gone)

		_, _ = io.Copy(io.Discard, conn)
	}()

	wsRequests <- cli

	defer func() { wsLeaves <- cli }()

	var sent, gen int

	for {
		select {
		case <-gone:
			return
		case u := <-cli.updates:
			// A new generation is the output of the component started again.
			if u.gen != gen {
				gen, sent = u.gen, 0
			}

			if writeStream(conn, u.out[sent:]) != nil {
				return
			}

			sent = len(u.out)

			if u.finished {
				_ = writeStream(conn, []byte(componentFinished))

				return
			}
		}
	}
}

func Start(base string) {
	basePath = base

	startComponents()

	go processPipelines()
}
