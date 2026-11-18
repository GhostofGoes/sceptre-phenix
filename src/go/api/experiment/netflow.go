package experiment

import (
	"errors"
	"fmt"
	"net"
	"strconv"
	"strings"
	"sync"
	"time"

	"phenix/store"
	"phenix/util/mm"
	"phenix/util/plog"
)

const (
	netflowChannelBufferSize = 100
	netflowMinFieldCount     = 8
	netflowMaxDatagramSize   = 65535

	// netflowWatchInterval is how often a netflow capture checks the store for
	// the end of its experiment run.
	netflowWatchInterval = 5 * time.Second
)

type Netflow struct {
	mu sync.RWMutex

	Bridge string
	Conn   *net.UDPConn

	// startTime is the start time of the experiment run the capture belongs to.
	startTime string

	// done is closed by Close, and watched when the watcher has returned.
	done      chan struct{}
	closeOnce sync.Once
	watched   chan struct{}

	callbacks map[string]chan map[string]any

	logMu   sync.Mutex
	lastLog map[string]time.Time
}

func NewNetflow(bridge string, conn *net.UDPConn) *Netflow {
	return &Netflow{
		mu:     sync.RWMutex{},
		Bridge: bridge,
		Conn:   conn,

		startTime: "",
		done:      make(chan struct{}),
		closeOnce: sync.Once{},
		watched:   make(chan struct{}),

		callbacks: make(map[string]chan map[string]any),
		logMu:     sync.Mutex{},
		lastLog:   make(map[string]time.Time),
	}
}

func (n *Netflow) NewChannel(id string) chan map[string]any {
	n.mu.Lock()
	defer n.mu.Unlock()

	if n.callbacks == nil {
		return nil
	}

	if _, ok := n.callbacks[id]; ok {
		return nil
	}

	cb := make(chan map[string]any, netflowChannelBufferSize)

	n.callbacks[id] = cb

	return cb
}

func (n *Netflow) DeleteChannel(id string) {
	n.mu.Lock()
	defer n.mu.Unlock()

	if cb, ok := n.callbacks[id]; ok {
		close(cb)

		for range cb { //nolint:revive // draining channel
			// draining channel so it doesn't block anything
		}
	}

	delete(n.callbacks, id)

	n.logMu.Lock()
	delete(n.lastLog, id)
	n.logMu.Unlock()
}

func (n *Netflow) Publish(body map[string]any) {
	n.mu.RLock()
	defer n.mu.RUnlock()

	for id, cb := range n.callbacks {
		// Use a non-blocking send to prevent a slow consumer from blocking the
		// publisher. If the channel's buffer is full (or it's unbuffered and no
		// receiver is ready), the message will be dropped for that specific
		// consumer.
		select {
		case cb <- body:
			// Message sent successfully
		default:
			n.logMu.Lock()
			if time.Since(n.lastLog[id]) > 5*time.Second {
				// Log that we are dropping a packet for this consumer.
				// This helps identify slow or stuck consumers.
				plog.Warn(
					plog.TypeSystem,
					"netflow: dropping packet for consumer, buffer is full",
					"consumer",
					id,
				)
				n.lastLog[id] = time.Now()
			}
			n.logMu.Unlock()
		}
	}
}

func (n *Netflow) Close() {
	n.mu.Lock()
	defer n.mu.Unlock()

	for _, cb := range n.callbacks {
		close(cb)
	}

	n.callbacks = nil
	n.lastLog = nil
	n.closeOnce.Do(func() { close(n.done) })
	_ = n.Conn.Close()
}

var (
	netflows  = make(map[string]*Netflow) //nolint:gochecknoglobals // package level registry
	netflowMu sync.Mutex                  //nolint:gochecknoglobals // package level registry

	ErrNetflowNotStarted     = errors.New("netflow not started for experiment")
	ErrNetflowAlreadyStarted = errors.New("netflow already started for experiment")
	ErrNetflowPhenixBridge   = errors.New("cannot capture netflow on default phenix bridge")
)

func init() { //nolint:gochecknoinits // package level setup
	// Delete netflow captures when experiments are stopped.
	RegisterHook("stop", func(_, name string) {
		netflowMu.Lock()
		defer netflowMu.Unlock()

		if flow, ok := netflows[name]; ok {
			// We don't need to worry about instructing minimega to delete the netflow
			// capture since that will happen as part of the minimega namespace for
			// this experiment being cleared.
			flow.Close()

			delete(netflows, name)
		}
	})
}

// netflowStale reports whether flow belongs to an experiment run that has
// ended: the experiment was deleted, stopped, or started again since the flow
// started. Stop hooks run only in the process that stops an experiment, so
// when the CLI stops it, phenix ui still holds the flow of the run that ended.
// If the store cannot tell, the flow is kept.
func netflowStale(exp string, flow *Netflow) bool {
	spec, err := Get(exp)
	if err != nil {
		if errors.Is(err, store.ErrNotExist) {
			return true
		}

		plog.Warn(plog.TypeSystem, "getting experiment of netflow capture", "exp", exp, "err", err)

		return false
	}

	return !spec.Running() || spec.Status.StartTime() != flow.startTime
}

// dropNetflow closes the flow of an experiment run that has ended and removes
// it from the registry. It sends minimega nothing: clearing the experiment's
// namespace on stop already deleted the capture, and a command would create the
// namespace again. The caller holds netflowMu.
func dropNetflow(exp string, flow *Netflow) {
	plog.Info(plog.TypeSystem, "dropping netflow capture of an experiment run that ended", "exp", exp)

	flow.Close()
	delete(netflows, exp)
}

// currentNetflow returns the netflow capture of the experiment's current run,
// dropping one left from a run that has ended. The caller holds netflowMu.
func currentNetflow(exp string) *Netflow {
	flow, ok := netflows[exp]
	if !ok {
		return nil
	}

	if netflowStale(exp, flow) {
		dropNetflow(exp, flow)

		return nil
	}

	return flow
}

// watchNetflow drops the flow once its experiment run ends, which closes the
// flow's websockets. Nothing else tells this process that another one, such as
// the CLI, stopped the experiment. It returns once the flow is closed.
func watchNetflow(exp string, flow *Netflow, interval time.Duration) {
	defer close(flow.watched)

	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	for {
		select {
		case <-flow.done:
			return
		case <-ticker.C:
			if !netflowStale(exp, flow) {
				continue
			}

			netflowMu.Lock()

			if netflows[exp] == flow {
				dropNetflow(exp, flow)
			}

			netflowMu.Unlock()

			return
		}
	}
}

func GetNetflow(exp string) *Netflow {
	netflowMu.Lock()
	defer netflowMu.Unlock()

	return currentNetflow(exp)
}

func StartNetflow(exp string) error {
	netflowMu.Lock()
	defer netflowMu.Unlock()

	if currentNetflow(exp) != nil {
		return ErrNetflowAlreadyStarted
	}

	spec, err := Get(exp)
	if err != nil {
		return ErrExperimentNotFound
	}

	// A dry run has no bridge or VMs in minimega, which would create the
	// namespace for the capture commands.
	if !spec.Running() || spec.DryRun() {
		return ErrExperimentNotRunning
	}

	if spec.Spec.DefaultBridge() == "phenix" {
		return ErrNetflowPhenixBridge
	}

	cluster, _ := ClusterNodes(exp)

	conn, err := net.ListenUDP("udp4", nil)
	if err != nil {
		return fmt.Errorf("creating UDP listener: %w", err)
	}

	addr := strings.Split(conn.LocalAddr().String(), ":")
	// The mode command is already broadcast by minimega. Sending it through
	// every cluster node can cause overlapping mesh broadcasts while a new
	// capture is being configured.
	if err := mm.MeshSend(exp, mm.Headnode(), "capture netflow mode ascii"); err != nil {
		_ = conn.Close()
		return fmt.Errorf("setting netflow capture mode: %w", err)
	}

	cmd := fmt.Sprintf(
		"capture netflow bridge %s udp %s:%s",
		spec.Spec.DefaultBridge(),
		mm.Headnode(),
		addr[1],
	)
	for _, node := range cluster {
		err = mm.MeshSend(exp, node, cmd)
		if err != nil {
			_ = conn.Close()
			return fmt.Errorf("starting netflow capture on node %s: %w", node, err)
		}
	}

	flow := NewNetflow(spec.Spec.DefaultBridge(), conn)
	flow.startTime = spec.Status.StartTime()
	netflows[exp] = flow

	go watchNetflow(exp, flow, netflowWatchInterval)

	go func() {
		// minimega sends each netflow packet as one datagram of newline-terminated
		// records. Reading per datagram bounds memory and keeps a malformed
		// datagram from bleeding into the next one.
		buf := make([]byte, netflowMaxDatagramSize)

		for {
			n, err := conn.Read(buf)
			if err != nil {
				if !errors.Is(err, net.ErrClosed) {
					plog.Warn(plog.TypeSystem, "reading netflow capture", "exp", exp, "err", err)
				}

				return
			}

			for line := range strings.SplitSeq(string(buf[:n]), "\n") {
				line = strings.TrimSpace(line)
				if line == "" {
					continue
				}

				body, ok := parseNetflowLine(line)
				if !ok {
					plog.Debug(plog.TypeSystem, "dropping malformed netflow line", "exp", exp, "line", line)
					continue
				}

				flow.Publish(body)
			}
		}
	}()

	return nil
}

func parseNetflowLine(line string) (map[string]any, bool) {
	fields := strings.Fields(line)
	if len(fields) < netflowMinFieldCount || fields[4] != "<->" {
		return nil, false
	}

	proto, err := strconv.Atoi(fields[2])
	if err != nil || proto < 0 || proto > 255 {
		return nil, false
	}

	srcHost, srcPortText, err := net.SplitHostPort(fields[3])
	if err != nil || net.ParseIP(srcHost) == nil {
		return nil, false
	}
	srcPort, err := strconv.Atoi(srcPortText)
	if err != nil || srcPort < 0 || srcPort > 65535 {
		return nil, false
	}

	dstHost, dstPortText, err := net.SplitHostPort(fields[5])
	if err != nil || net.ParseIP(dstHost) == nil {
		return nil, false
	}
	dstPort, err := strconv.Atoi(dstPortText)
	if err != nil || dstPort < 0 || dstPort > 65535 {
		return nil, false
	}

	packets, err := strconv.Atoi(fields[6])
	if err != nil || packets < 0 {
		return nil, false
	}
	bytes, err := strconv.Atoi(fields[7])
	if err != nil || bytes < 0 {
		return nil, false
	}

	return map[string]any{
		"proto":   proto,
		"src":     srcHost,
		"sport":   srcPort,
		"dst":     dstHost,
		"dport":   dstPort,
		"packets": packets,
		"bytes":   bytes,
	}, true
}

func StopNetflow(exp string) error {
	netflowMu.Lock()
	defer netflowMu.Unlock()

	flow := currentNetflow(exp)
	if flow == nil {
		return ErrNetflowNotStarted
	}

	cluster, _ := ClusterNodes(exp)

	cmd := "capture netflow delete bridge " + flow.Bridge

	for _, node := range cluster {
		err := mm.MeshSend(exp, node, cmd)
		if err != nil {
			return fmt.Errorf("deleting netflow capture on node %s: %w", node, err)
		}
	}

	flow.Close()
	delete(netflows, exp)

	return nil
}
