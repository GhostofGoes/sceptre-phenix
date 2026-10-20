package scorch

import (
	"fmt"
)

const (
	statusStart      = "start"
	statusBackground = "background"
	statusFailure    = "failure"
	statusSuccess    = "success"
	statusRunning    = "running"
	statusUnstable   = "unstable"
	statusUnknown    = "unknown"
)

type ComponentUpdate struct {
	Exp     string // experiment name
	CmpName string // component name
	CmpType string // component type
	Run     int    // experiment run
	Loop    int    // current loop
	Count   int    // current loop count
	Stage   string // component stage
	Status  string // component status
	Output  []byte // component output

	done chan struct{}
}

type outputRequest struct {
	key  string
	resp chan outputResponse
}

type outputResponse struct {
	running  bool
	terminal bool
	output   []byte
}

var (
	componentUpdates chan ComponentUpdate //nolint:gochecknoglobals // global state
	outputRequests   chan outputRequest   //nolint:gochecknoglobals // global state

	cmpType map[string]string //nolint:gochecknoglobals // global state
	running map[string]bool   //nolint:gochecknoglobals // global state
	output  map[string][]byte //nolint:gochecknoglobals // global state
)

func UpdateComponent(update ComponentUpdate) {
	if componentUpdates == nil {
		return
	}

	update.done = make(chan struct{})
	componentUpdates <- update

	<-update.done
}

// startComponents makes the state of the component processor and starts it.
func startComponents() {
	componentUpdates = make(chan ComponentUpdate)
	outputRequests = make(chan outputRequest)
	wsRequests = make(chan wsRequest)

	cmpType = make(map[string]string)
	running = make(map[string]bool)
	output = make(map[string][]byte)
	ws = make(map[string]map[string]wsRequest)

	go processComponents()
}

// processComponents is the only user of the component maps: a concurrent
// map read and write ends the process.
func processComponents() {
	for {
		select {
		case req := <-wsRequests:
			addOutputStream(req)
		case update := <-componentUpdates:
			// track updates by exp/run/loop/stage/component
			key := fmt.Sprintf(
				"%s|%d|%d|%s|%s",
				update.Exp,
				update.Run,
				update.Loop,
				update.Stage,
				update.CmpName,
			)

			switch update.Status {
			case statusStart:
				output[key] = nil
				cmpType[key] = update.CmpType
			case statusRunning, statusBackground:
				running[key] = true

				if len(update.Output) != 0 {
					output[key] = append(output[key], update.Output...)
				}

				// stream to any websockets listening for this key
				for id, cli := range ws[key] {
					nw, err := cli.ws.Write(update.Output)
					if err != nil {
						close(cli.done)
						delete(ws[key], id)

						continue
					}

					if nw != len(update.Output) {
						close(cli.done)
						delete(ws[key], id)
					}
				}
			case statusSuccess, statusFailure:
				delete(running, key)

				// notify any websockets for this key that component is done
				for id, cli := range ws[key] {
					_, _ = cli.ws.Write([]byte("***** COMPONENT FINISHED *****"))
					close(cli.done)
					delete(ws[key], id)
				}
			}

			close(update.done)
		case req := <-outputRequests:
			resp := outputResponse{running: running[req.key]} //nolint:exhaustruct // partial initialization

			if resp.running {
				resp.terminal = cmpType[req.key] == "break"
			} else {
				resp.output = output[req.key]
			}

			req.resp <- resp
		}
	}
}
