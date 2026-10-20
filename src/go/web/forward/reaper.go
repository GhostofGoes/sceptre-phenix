package forward

import (
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/hashicorp/go-multierror"

	"phenix/util/mm"
	"phenix/util/plog"
	"phenix/web/broker"
	bt "phenix/web/broker/brokertypes"
	ft "phenix/web/forward/forwardtypes"
)

// tunnelGoneErrors are the errors minimega answers `cc tunnel list <vm>` with
// when the VM, or the miniccc client whose tunnel held the forwards, no longer
// exists. A client that reconnects gets a new, empty tunnel, so the forwards
// are gone in every case.
var tunnelGoneErrors = []string{ //nolint:gochecknoglobals // constant list
	"vm not found",
	"no such client",
	"tunnel has not been initialized",
}

// tunnelGone reports whether every error minimega answered a tunnel listing
// with says the tunnel no longer exists.
func tunnelGone(err error) bool {
	errs := []error{err}

	var merr *multierror.Error
	if errors.As(err, &merr) {
		errs = merr.Errors
	}

	for _, e := range errs {
		gone := false

		for _, msg := range tunnelGoneErrors {
			if strings.Contains(e.Error(), msg) {
				gone = true

				break
			}
		}

		if !gone {
			return false
		}
	}

	return true
}

func forwardExists(l ft.Listener) bool {
	if l.QEMU {
		return true
	}

	tunnels, err := mm.GetTunnels(
		mm.NS(l.Exp),
		mm.VMName(l.VM),
		mm.TunnelDestinationHost(l.DstHost),
		mm.TunnelDestinationPort(l.DstPort),
	)

	switch {
	case len(tunnels) > 0:
		return true
	case err == nil:
		return false
	case tunnelGone(err):
		return false
	default:
		// The listing failed, for example because minimega could not be reached,
		// which says nothing about the tunnel itself.
		plog.Warn(
			plog.TypeSystem,
			"keeping port forward whose minimega tunnel could not be listed",
			"exp", l.Exp,
			"vm", l.VM,
			"host", l.DstHost,
			"port", l.DstPort,
			"err", err,
		)

		return true
	}
}

func deleteForward(l ft.Listener) {
	data := map[string]any{"key": l.ToKey()}
	body, _ := json.Marshal(data)

	broker.Broadcast(
		bt.NewRequestPolicy("vms/forwards", "delete", fmt.Sprintf("%s/%s", l.Exp, l.VM)),
		bt.NewResource("experiment/vm/forward", fmt.Sprintf("%s/%s", l.Exp, l.VM), "delete"),
		body,
	)

	delete(forwards, l.ToKey())
}

func reapForwards() {
	for _, l := range forwards {
		if !forwardExists(l) {
			deleteForward(l)
		}
	}
}
