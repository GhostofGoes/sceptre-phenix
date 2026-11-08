package app

import (
	"errors"
	"fmt"
	"strconv"
	"strings"

	"github.com/mitchellh/mapstructure"

	"phenix/types"
)

const (
	// AutotunnelAnnotation lists the port forwards phenix ui keeps to a node.
	AutotunnelAnnotation = "phenix/startup-autotunnel"

	// AutotunnelOwner is the user phenix ui creates startup autotunnels as.
	AutotunnelOwner = "bot"

	// autotunnelVNC as an entry's destination port forwards to the VM's VNC
	// console, which needs no C2 client.
	autotunnelVNC = "VNC"

	// autotunnelVNCPort is the destination port a VNC forward records.
	autotunnelVNCPort = 5900

	tunnelConfigPartsPortOnly     = 1
	tunnelConfigPartsPortHost     = 2
	tunnelConfigPartsPortHostDest = 3
)

// Autotunnel is one entry of a node's phenix/startup-autotunnel annotation:
// a port forward phenix ui keeps to the VM for as long as the experiment runs.
type Autotunnel struct {
	Experiment string
	VM         string
	Sport      string
	Dhost      string
	Dport      string
}

// VNC reports whether the autotunnel forwards to the VM's VNC console, which
// needs no C2 client.
func (t Autotunnel) VNC() bool {
	return strings.EqualFold(t.Dport, autotunnelVNC)
}

// DstPort returns the destination port the autotunnel's forward records.
func (t Autotunnel) DstPort() int {
	if t.VNC() {
		return autotunnelVNCPort
	}

	port, _ := strconv.Atoi(t.Dport)

	return port
}

// LogArgs returns the autotunnel's structured log fields.
func (t Autotunnel) LogArgs(extra ...any) []any {
	return append(
		[]any{"exp", t.Experiment, "vm", t.VM, "sport", t.Sport, "dhost", t.Dhost, "dport", t.Dport},
		extra...,
	)
}

// Autotunnels returns the startup autotunnels the VMs of exp ask for, and an
// error for each annotation or entry it ignores: one that cannot be parsed,
// and one whose VM, destination host and destination port repeat an earlier
// entry's, since phenix ui keys a forward by those and its owner.
func Autotunnels(exp *types.Experiment) ([]Autotunnel, []error) {
	var (
		tunnels []Autotunnel
		errs    []error
		seen    = make(map[string]string)
	)

	for _, node := range exp.Spec.Topology().Nodes() {
		if node.External() {
			continue
		}

		annotation, ok := node.GetAnnotation(AutotunnelAnnotation)
		if !ok {
			continue
		}

		vm := node.General().Hostname()

		var entries []string

		if err := mapstructure.Decode(annotation, &entries); err != nil {
			errs = append(errs, fmt.Errorf("vm %s: parsing %s annotation: %w", vm, AutotunnelAnnotation, err))

			continue
		}

		for _, entry := range entries {
			tunnel, err := parseAutotunnel(exp.Metadata.Name, vm, entry)
			if err != nil {
				errs = append(errs, fmt.Errorf("vm %s: entry %q: %w", vm, entry, err))

				continue
			}

			key := fmt.Sprintf("%s:%s:%d", vm, tunnel.Dhost, tunnel.DstPort())
			if first, dup := seen[key]; dup {
				errs = append(errs, fmt.Errorf("vm %s: entry %q: forwards to the destination of entry %q", vm, entry, first))

				continue
			}

			seen[key] = entry
			tunnels = append(tunnels, tunnel)
		}
	}

	return tunnels, errs
}

// parseAutotunnel parses an entry of the form sport[[:dhost]:dport].
func parseAutotunnel(exp, vm, entry string) (Autotunnel, error) {
	tunnel := Autotunnel{Experiment: exp, VM: vm, Dhost: "127.0.0.1"} //nolint:exhaustruct // set below

	tokens := strings.Split(entry, ":")

	switch len(tokens) {
	case tunnelConfigPartsPortOnly:
		tunnel.Sport, tunnel.Dport = tokens[0], tokens[0]
	case tunnelConfigPartsPortHost:
		tunnel.Sport, tunnel.Dport = tokens[0], tokens[1]
	case tunnelConfigPartsPortHostDest:
		tunnel.Sport, tunnel.Dhost, tunnel.Dport = tokens[0], tokens[1], tokens[2]
	default:
		return tunnel, fmt.Errorf("want sport, sport:dport or sport:dhost:dport, got %d fields", len(tokens))
	}

	if !validPort(tunnel.Sport) {
		return tunnel, fmt.Errorf("invalid source port %q", tunnel.Sport)
	}

	if tunnel.Dhost == "" {
		return tunnel, errors.New("empty destination host")
	}

	if !tunnel.VNC() && !validPort(tunnel.Dport) {
		return tunnel, fmt.Errorf("invalid destination port %q", tunnel.Dport)
	}

	return tunnel, nil
}

func validPort(s string) bool {
	port, err := strconv.Atoi(s)

	return err == nil && port > 0 && port <= 65535
}
