package broker

import (
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"net/http"
	"runtime/debug"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/gorilla/websocket"
	"google.golang.org/protobuf/encoding/protojson"
	"inet.af/netaddr"

	"phenix/api/experiment"
	"phenix/api/vm"
	"phenix/util/cache"
	"phenix/util/mm"
	"phenix/util/plog"
	bt "phenix/web/broker/brokertypes"
	webcache "phenix/web/cache"
	"phenix/web/middleware"
	"phenix/web/proto"
	"phenix/web/rbac"
	"phenix/web/util"
)

var marshaler = protojson.MarshalOptions{EmitUnpopulated: true} //nolint:gochecknoglobals // global marshaler

type vmScope struct {
	exp  string
	name string
}

const (
	writeWait                   = 10 * time.Second
	pongWait                    = 60 * time.Second
	pingPeriodNumerator         = 9
	pingPeriodDenominator       = 10
	pingPeriod                  = (pongWait * pingPeriodNumerator) / pingPeriodDenominator
	maxMsgSize                  = 2048
	socketBufferSize            = 4096
	publishChannelBuffer        = 256
	screenshotTickerInterval    = 5 * time.Second
	defaultBrokerScreenshotSize = "200"

	// maxVMListPage bounds the page number and size of a VM list request.
	maxVMListPage = 1 << 20
)

var (
	newline  = []byte{'\n'}        //nolint:gochecknoglobals // global constant
	upgrader = websocket.Upgrader{ //nolint:gochecknoglobals,exhaustruct // global upgrader
		ReadBufferSize:  socketBufferSize,
		WriteBufferSize: socketBufferSize,
	}
)

type Client struct {
	role   rbac.Role
	conn   *websocket.Conn
	connMu sync.Mutex

	publish chan any
	done    chan struct{}
	once    sync.Once

	// Track the VMs this client currently has in view, if any, so we know
	// what screenshots need to periodically be pushed to the client over
	// the WebSocket connection.
	vms  []vmScope
	vmMu sync.RWMutex

	// The size of the screenshots the client is sent, as it last asked;
	// empty for the default size.
	screenshotSize string
	sizeMu         sync.Mutex
}

func NewClient(role rbac.Role, conn *websocket.Conn) *Client {
	return &Client{ //nolint:exhaustruct // partial initialization
		role:    role,
		conn:    conn,
		publish: make(chan any, publishChannelBuffer),
		done:    make(chan struct{}),
	}
}

func (c *Client) Go() {
	register <- c

	go c.write()
	go c.read()
	go c.setScreenshotsTicker()
}

func (c *Client) Stop() {
	c.once.Do(c.stop)
}

func (c *Client) stop() {
	unregister <- c

	close(c.done)

	c.connMu.Lock()
	defer c.connMu.Unlock()

	err := c.conn.WriteMessage(websocket.CloseMessage, []byte{})
	if err != nil {
		plog.Warn(plog.TypeSystem, "closing client connection", "err", err)
	}

	_ = c.conn.Close()
}

// vmListOptions returns the sort column and order and the page number and
// size of an experiment/vms list request's payload. A missing field leaves the
// list unsorted, in descending order or unpaged; a field of the wrong type, or
// a page number or size that is not a whole number from 0 to maxVMListPage,
// is an error.
func vmListOptions(payload map[string]any) (string, bool, int, int, error) {
	var (
		sort, sortOK = payload["sort_column"].(string)
		asc, ascOK   = payload["sort_asc"].(bool)
	)

	if !sortOK && payload["sort_column"] != nil {
		return "", false, 0, 0, fmt.Errorf("sort_column %v is not a string", payload["sort_column"])
	}

	if !ascOK && payload["sort_asc"] != nil {
		return "", false, 0, 0, fmt.Errorf("sort_asc %v is not a Boolean", payload["sort_asc"])
	}

	paging := make([]int, 0, 2) //nolint:mnd // page number and size

	for _, key := range []string{"page_number", "page_size"} {
		v, ok := payload[key].(float64)
		if !ok && payload[key] != nil {
			return "", false, 0, 0, fmt.Errorf("%s %v is not a number", key, payload[key])
		}

		if v < 0 || v > maxVMListPage || v != math.Trunc(v) {
			return "", false, 0, 0, fmt.Errorf("%s %v is not a whole number from 0 to %d", key, v, maxVMListPage)
		}

		paging = append(paging, int(v))
	}

	return sort, asc, paging[0], paging[1], nil
}

// recoverPanic logs a panic of one of the client's goroutines, with its
// stack, so that the panic ends only this client, which the goroutine's
// deferred Stop then stops, and not phenix ui. The goroutine must defer it
// directly, after its Stop.
func (c *Client) recoverPanic(task string) {
	if r := recover(); r != nil {
		plog.Error(plog.TypeSystem, task, "panic", r, "stack", string(debug.Stack()))
	}
}

//nolint:cyclop,funlen,gocyclo // complex logic
func (c *Client) read() { //nolint:maintidx // complex logic
	defer c.Stop()
	defer c.recoverPanic("handling WebSocket client request")

	c.conn.SetReadLimit(maxMsgSize)

	err := c.conn.SetReadDeadline(time.Now().Add(pongWait))
	if err != nil {
		plog.Error(plog.TypeSystem, "setting read deadline for client connection", "err", err)

		return
	}

	ponger := func(string) error {
		err := c.conn.SetReadDeadline(time.Now().Add(pongWait))
		if err != nil {
			plog.Error(
				plog.TypeSystem,
				"setting read deadline in pong handler for client connection",
				"err",
				err,
			)

			return err
		}

		return nil
	}

	c.conn.SetPongHandler(ponger)

	for {
		select {
		case <-c.done:
			return
		default:
			_, msg, err := c.conn.ReadMessage()
			if err != nil {
				if websocket.IsUnexpectedCloseError(
					err,
					websocket.CloseGoingAway,
					websocket.CloseAbnormalClosure,
				) {
					plog.Debug(plog.TypeSystem, "reading from WebSocket client", "err", err)
				}

				return
			}

			var req bt.Request
			if err := json.Unmarshal(msg, &req); err != nil {
				plog.Error(plog.TypeSystem, "cannot unmarshal request JSON", "err", err)

				continue
			}

			switch req.Resource.Type {
			case "experiment/vms":
			case "metadata/screenshot":
				var payload map[string]string

				err := json.Unmarshal(req.Payload, &payload)
				if err != nil {
					plog.Error(
						plog.TypeSystem,
						"cannot unmarshal WebSocket request payload JSON",
						"err",
						err,
					)

					continue
				}

				if size, ok := payload["size"]; ok {
					size, err := util.ScreenshotSize(size)
					if err != nil {
						plog.Warn(plog.TypeSystem, "refusing screenshot resolution", "err", err)

						continue
					}

					plog.Debug(plog.TypeSystem, "updated screenshot resolution", "size", size)
					c.setScreenshotSize(size)

					c.updateScreenshots()
				}

				continue
			case "experiment/topology":
				// TODO: check RBAC permissions?
				switch req.Resource.Action {
				case "search":
					var query map[string]string

					if err := json.Unmarshal(req.Payload, &query); err != nil {
						plog.Error(plog.TypeSystem, "cannot unmarshal request payload", "err", err)

						continue
					}

					// TODO: handle multiple query terms (how? AND or OR?)
					// Do the same as in web/experiment.go@SearchExperimentTopology
					term := query["term"]
					if term == "" {
						term = "hostname"
					}

					value := query["value"]
					if value == "" {
						plog.Error(plog.TypeSystem, "missing search value for term", "term", term)

						continue
					}

					cacheKey := fmt.Sprintf("experiment|%s|search", req.Resource.Name)

					val, ok := cache.Get(cacheKey)
					if !ok {
						// warm the cache (again?)
						if _, err := vm.Topology(req.Resource.Name, nil); err != nil {
							plog.Error(
								plog.TypeSystem,
								"getting experiment topology",
								"exp",
								req.Resource.Name,
								"err",
								err,
							)

							continue
						}

						val, _ = cache.Get(cacheKey)
					}

					var (
						search, _ = val.(vm.TopologySearch)
						nodes     []int
					)

					switch strings.ToLower(term) {
					case "hostname":
						if node, ok := search.Hostname[value]; ok {
							nodes = []int{node}
						}
					case "disk":
						nodes = search.Disk[value]
					case "node-type":
						nodes = search.Type[value]
					case "os-type":
						nodes = search.OSType[value]
					case "label":
						nodes = search.Label[value]
					case "annotation":
						nodes = search.Annotation[value]
					case "vlan":
						nodes = search.VLAN[value]
					case "ip":
						if net, err := netaddr.ParseIPPrefix(value); err == nil {
							for k, v := range search.IP {
								ip, ipErr := netaddr.ParseIP(k)
								if ipErr != nil {
									continue
								}

								if net.Contains(ip) {
									nodes = append(nodes, v...)
								}
							}
						} else {
							nodes = search.IP[value]
						}
					}

					results := map[string]any{
						"term":  term,
						"value": value,
						"results": map[string]any{
							"nodes": nodes,
						},
					}

					body, err := json.Marshal(results)
					if err != nil {
						plog.Error(
							plog.TypeSystem,
							"marshaling search results for WebSocket client",
							"err",
							err,
						)

						continue
					}

					c.publish <- bt.Publish{ //nolint:exhaustruct // partial initialization
						Resource: bt.NewResource("experiment/topology", req.Resource.Name, "search"),
						Result:   body,
					}

					continue
				default:
					plog.Error(
						plog.TypeSystem,
						"unexpected WebSocket request resource action for experiment/topology resource type",
						"action",
						req.Resource.Action,
					)

					continue
				}
			default:
				plog.Error(
					plog.TypeSystem,
					"unexpected WebSocket request resource type",
					"type",
					req.Resource.Type,
				)

				continue
			}

			switch req.Resource.Action {
			case "list":
			default:
				plog.Error(
					plog.TypeSystem,
					"unexpected WebSocket request resource action",
					"action",
					req.Resource.Action,
				)

				continue
			}

			var payload map[string]any
			if err := json.Unmarshal(req.Payload, &payload); err != nil {
				plog.Error(
					plog.TypeSystem,
					"cannot unmarshal WebSocket request payload JSON",
					"err",
					err,
				)

				continue
			}

			if !c.role.Allowed("vms", "list") {
				plog.Warn(plog.TypeSecurity, "client access to vms/list forbidden")

				continue
			}

			expName := req.Resource.Name

			sort, asc, page, size, err := vmListOptions(payload)
			if err != nil {
				plog.Error(plog.TypeSystem, "invalid WebSocket VM list request", "exp", expName, "err", err)

				continue
			}

			exp, err := experiment.Get(expName)
			if err != nil {
				plog.Error(
					plog.TypeSystem,
					"getting experiment for WebSocket client",
					"exp",
					expName,
					"err",
					err,
				)

				continue
			}

			vms, err := vm.List(expName)

			switch {
			case errors.Is(err, vm.ErrPartialList):
				plog.Warn(plog.TypeSystem, "sending the VMs the other hosts listed", "exp", expName, "err", err)
			case err != nil:
				plog.Error(
					plog.TypeSystem,
					"getting list of VMs for experiment",
					"exp",
					expName,
					"err",
					err,
				)

				continue
			}

			// A Boolean expression tree is built and the fields that
			// should be searched are determined based on the search string
			clientFilter, _ := payload["filter"].(string)
			filterTree := mm.BuildTree(clientFilter)

			// If `show_dnb` was not provided client-side, `showDNB` will be false,
			// which is the default we want.
			showDNB, _ := payload["show_dnb"].(bool)

			allowed := mm.VMs{}

			for _, vm := range vms {
				// If the VM is marked as do not boot, and we're not showing VMs marked as
				// such, continue on to the next VM right away.
				if vm.DoNotBoot && !showDNB {
					continue
				}

				// If the filter supplied could not be
				// parsed, do not add the VM
				if len(clientFilter) > 0 {
					if filterTree == nil {
						continue
					} else if !filterTree.Evaluate(&vm) {
						// If the search string could be parsed,
						// determine if the VM should be included
						continue
					}
				}

				if c.role.Allowed("vms", "list", fmt.Sprintf("%s/%s", expName, vm.Name)) {
					if vm.Running {
						screenshot, err := util.GetScreenshot(expName, vm.Name, c.getScreenshotSize())
						if err != nil {
							plog.Error(
								plog.TypeSystem,
								"getting screenshot for WebSocket client",
								"err",
								err,
							)
						} else {
							vm.Screenshot = "data:image/png;base64," + base64.StdEncoding.EncodeToString(
								screenshot,
							)
						}
					}

					allowed = append(allowed, vm)
				}
			}

			if sort != "" {
				allowed.SortBy(sort, asc)
			}

			totalBeforePaging := len(allowed)

			if page != 0 && size != 0 {
				allowed = allowed.Paginate(page, size)
			}

			c.vmMu.Lock()

			c.vms = nil

			for _, v := range allowed {
				c.vms = append(c.vms, vmScope{exp: expName, name: v.Name})
			}

			c.vmMu.Unlock()

			resp := &proto.VMList{
				Total: uint32(totalBeforePaging), //nolint:gosec // integer overflow conversion int -> uint32
				Vms:   make([]*proto.VM, len(allowed)),
			}
			for i, v := range allowed {
				resp.Vms[i] = util.VMToProtobuf(expName, v, exp.Spec.Topology())
			}

			body, err := marshaler.Marshal(resp)
			if err != nil {
				plog.Error(
					plog.TypeSystem,
					"marshaling experiment VMs for WebSocket client",
					"exp",
					exp,
					"err",
					err,
				)

				continue
			}

			c.publish <- bt.Publish{ //nolint:exhaustruct // partial initialization
				Resource: bt.NewResource("experiment/vms", expName, "list"),
				Result:   body,
			}
		}
	}
}

func (c *Client) write() {
	ticker := time.NewTicker(pingPeriod)

	defer ticker.Stop()
	defer c.Stop()
	defer c.recoverPanic("publishing to WebSocket client")

	for {
		select {
		case <-c.done:
			return
		case msg := <-c.publish:
			err := c.publisher(msg)
			if err != nil {
				plog.Error(plog.TypeSystem, "publishing message to client", "err", err)
			}
		case <-ticker.C:
			err := c.conn.SetWriteDeadline(time.Now().Add(writeWait))
			if err != nil {
				plog.Error(
					plog.TypeSystem,
					"setting write deadline for client connection",
					"err",
					err,
				)

				return
			}

			err = c.conn.WriteMessage(websocket.PingMessage, nil)
			if err != nil {
				plog.Error(plog.TypeSystem, "pinging client connection", "err", err)

				return
			}
		}
	}
}

func (c *Client) publisher(msg any) error {
	c.connMu.Lock()
	defer c.connMu.Unlock()

	if err := c.conn.SetWriteDeadline(time.Now().Add(writeWait)); err != nil {
		return fmt.Errorf("setting write deadline for client connection: %w", err)
	}

	w, err := c.conn.NextWriter(websocket.TextMessage)
	if err != nil {
		return fmt.Errorf("getting next writer for client connection: %w", err)
	}

	defer func() { _ = w.Close() }()

	b, err := json.Marshal(msg)
	if err != nil {
		plog.Error(plog.TypeSystem, "marshaling message to be published", "err", err)

		return nil
	}

	if _, err := w.Write(b); err != nil {
		return fmt.Errorf("writing message to client connection: %w", err)
	}

	for range len(c.publish) {
		if _, err := w.Write(newline); err != nil {
			return fmt.Errorf("writing newline to client connection: %w", err)
		}

		msg := <-c.publish

		b, err := json.Marshal(msg)
		if err != nil {
			plog.Error(plog.TypeSystem, "marshaling message to be published", "err", err)

			continue
		}

		if _, err := w.Write(b); err != nil {
			return fmt.Errorf("writing message to client connection: %w", err)
		}
	}

	return nil
}

func (c *Client) setScreenshotsTicker() {
	ticker := time.NewTicker(screenshotTickerInterval)

	defer ticker.Stop()
	defer c.Stop()
	defer c.recoverPanic("updating WebSocket client screenshots")

	for {
		select {
		case <-c.done:
			return
		case <-ticker.C:
			c.updateScreenshots()
		}
	}
}

func (c *Client) updateScreenshots() {
	names := make(map[string][]string)

	c.vmMu.RLock()

	for _, v := range c.vms {
		names[v.exp] = append(names[v.exp], v.name)
	}

	c.vmMu.RUnlock()

	size := c.getScreenshotSize()

	for exp, vms := range names {
		for _, vm := range vms {
			screenshot, err := util.GetScreenshot(exp, vm, size)
			if err != nil {
				// A client may still show an experiment that has stopped, or is a
				// dry run; none of its VMs has a screenshot until it is listed again.
				// A stop in progress can still fail, so the VMs of an experiment
				// being stopped stay in view, without screenshots until it ends.
				if errors.Is(err, experiment.ErrExperimentNotRunning) {
					if !webcache.IsExperimentStopping(exp) {
						c.forgetVMs(exp)
					}

					break
				}

				if errors.Is(err, mm.ErrVMNotFound) || errors.Is(err, mm.ErrVMNotRunning) {
					continue
				}

				if errors.Is(err, mm.ErrScreenshotNotFound) {
					continue
				}

				plog.Error(plog.TypeSystem, "getting screenshot for WebSocket client", "err", err)

				continue
			}

			encoded := "data:image/png;base64," + base64.StdEncoding.EncodeToString(screenshot)

			marshalled, err := json.Marshal(util.WithRoot("screenshot", encoded))
			if err != nil {
				plog.Error(
					plog.TypeSystem,
					"marshaling VM screenshot for WebSocket client",
					"vm",
					vm,
					"err",
					err,
				)

				continue
			}

			c.publish <- bt.Publish{ //nolint:exhaustruct // partial initialization
				Resource: bt.NewResource("experiment/vm/screenshot", fmt.Sprintf("%s/%s", exp, vm), "update"),
				Result:   marshalled,
			}
		}
	}
}

// getScreenshotSize returns the size of the screenshots the client is sent.
func (c *Client) getScreenshotSize() string {
	c.sizeMu.Lock()
	defer c.sizeMu.Unlock()

	if c.screenshotSize == "" {
		return defaultBrokerScreenshotSize
	}

	return c.screenshotSize
}

// setScreenshotSize sets the size of the screenshots the client is sent.
func (c *Client) setScreenshotSize(size string) {
	c.sizeMu.Lock()
	defer c.sizeMu.Unlock()

	c.screenshotSize = size
}

// forgetVMs drops the VMs of experiment exp from the VMs the client has in
// view.
func (c *Client) forgetVMs(exp string) {
	c.vmMu.Lock()
	defer c.vmMu.Unlock()

	c.vms = slices.DeleteFunc(c.vms, func(v vmScope) bool { return v.exp == exp })
}

func ServeWS(w http.ResponseWriter, r *http.Request) {
	upgrader.CheckOrigin = func(*http.Request) bool { return true }

	conn, err := upgrader.Upgrade(w, r, nil)
	if err != nil {
		plog.Error(plog.TypeSystem, "upgrading connection to WebSocket", "err", err)

		return
	}

	role, _ := r.Context().Value(middleware.ContextKeyRole).(rbac.Role)

	NewClient(role, conn).Go()
}
