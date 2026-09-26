package forward

import (
	"encoding/json"
	"fmt"
	"strconv"
	"strings"

	"phenix/util/mm"
	"phenix/web/broker"
	bt "phenix/web/broker/brokertypes"
	ft "phenix/web/forward/forwardtypes"
)

// forwardExists reports whether the tunnel behind a forward is still open,
// given the VM's tunnels as listed by mm.GetTunnels without any destination
// filter. It matches the destination the way the minimega filters GetTunnels
// would otherwise apply do: case-insensitively, and only for the parts set.
func forwardExists(l ft.Listener, tunnels []map[string]string) bool {
	if l.QEMU {
		return true
	}

	for _, row := range tunnels {
		if l.DstHost != "" && !strings.EqualFold(row["dst"], l.DstHost) {
			continue
		}

		if l.DstPort != 0 && row["dst port"] != strconv.Itoa(l.DstPort) {
			continue
		}

		return true
	}

	return false
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
	// Forwards through the same VM share one tunnel listing.
	tunnels := make(map[string][]map[string]string)

	for _, l := range forwards {
		var vmTunnels []map[string]string

		if !l.QEMU {
			key := l.Exp + "\x00" + l.VM

			listed, ok := tunnels[key]
			if !ok {
				listed = mm.GetTunnels(mm.NS(l.Exp), mm.VMName(l.VM))
				tunnels[key] = listed
			}

			vmTunnels = listed
		}

		if !forwardExists(l, vmTunnels) {
			deleteForward(l)
		}
	}
}
