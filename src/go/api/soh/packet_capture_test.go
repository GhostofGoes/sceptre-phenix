package soh

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"phenix/store"
	"phenix/types"
	"phenix/util/mm"
)

// packetCaptureExperiment returns an experiment, based in a temporary
// directory, whose soh app captures rtu-1's IF0 with the Elastic server at
// address.
func packetCaptureExperiment(t *testing.T, address string) *types.Experiment {
	t.Helper()

	exp, err := types.DecodeExperimentFromConfig(store.Config{
		Version:  "phenix.sandia.gov/v1",
		Kind:     "Experiment",
		Metadata: store.ConfigMetadata{Name: "soh-capture"},
		Spec: map[string]any{
			"experimentName": "soh-capture",
			"baseDir":        t.TempDir(),
			"topology": map[string]any{
				"nodes": []map[string]any{{
					"type":     "VirtualMachine",
					"general":  map[string]any{"hostname": "rtu-1"},
					"hardware": map[string]any{"os_type": "linux", "drives": []map[string]any{{"image": "bennu.qc2"}}},
					"network": map[string]any{"interfaces": []map[string]any{{
						"name": "IF0", "type": "ethernet", "vlan": "EXP", "proto": "static",
						"address": "10.117.4.101", "mask": 24,
					}}},
				}},
			},
			"scenario": map[string]any{
				"apps": []map[string]any{{"name": "soh", "metadata": map[string]any{
					"c2Timeout": "1s",
					"packetCapture": map[string]any{
						"elasticImage":    "elasticsearch.qc2",
						"packetBeatImage": "packetbeat.qc2",
						"elasticServer": map[string]any{
							"hostname":  "soh-elasticsearch-server",
							"ipAddress": address,
							"vlan":      "EXP",
						},
						"captureHosts": map[string]any{"rtu-1": []any{"IF0"}},
					},
				}}},
			},
		},
	})
	if err != nil {
		t.Fatalf("decoding experiment: %v", err)
	}

	return exp
}

// TestSOHRefusesElasticServerAddressesItCannotUse is a regression test: the
// soh app ignored the error of parsing packetCapture.elasticServer.ipAddress,
// so an address without a prefix length panicked in Configure and
// deployCapture, and an IPv6 address panicked when the monitors' addresses
// counted up from it.
func TestSOHRefusesElasticServerAddressesItCannotUse(t *testing.T) {
	t.Parallel()

	for _, address := range []string{"", "10.117.4.200", "elastic/24", "fd00::200/64", "::ffff:10.117.4.200/120"} {
		t.Run(address, func(t *testing.T) {
			t.Parallel()

			exp := packetCaptureExperiment(t, address)

			var err error

			func() {
				defer func() {
					if r := recover(); r != nil {
						err = fmt.Errorf("panicked: %v", r)
					}
				}()

				err = newSOH().Configure(context.Background(), exp)
			}()

			if err == nil || !strings.Contains(err.Error(), "packetCapture.elasticServer.ipAddress") {
				t.Errorf("Configure() error = %v, want one naming packetCapture.elasticServer.ipAddress", err)
			}

			if servers := exp.Spec.Topology().FindNodesWithLabels("soh-elastic-server"); len(servers) != 0 {
				t.Errorf("Configure() added the Elastic server node with ipAddress %q", address)
			}
		})
	}
}

// TestSOHConfigureAddsElasticServerOnMGMT checks the Elastic server node soh
// adds to the topology: on the MGMT VLAN, whatever elasticServer.vlan says,
// with the address and prefix length of elasticServer.ipAddress, and with the
// Elasticsearch and Kibana configs soh renders for it.
func TestSOHConfigureAddsElasticServerOnMGMT(t *testing.T) {
	t.Parallel()

	exp := packetCaptureExperiment(t, "10.117.4.200/24")

	if err := newSOH().Configure(context.Background(), exp); err != nil {
		t.Fatalf("Configure() error = %v", err)
	}

	servers := exp.Spec.Topology().FindNodesWithLabels("soh-elastic-server")
	if len(servers) != 1 {
		t.Fatalf("Configure() added %d Elastic server nodes, want 1", len(servers))
	}

	ifaces := servers[0].Network().Interfaces()
	if len(ifaces) != 1 {
		t.Fatalf("the Elastic server has %d interfaces, want 1", len(ifaces))
	}

	iface := ifaces[0]
	if iface.VLAN() != "MGMT" || iface.Address() != "10.117.4.200" || iface.Mask() != 24 {
		t.Errorf(
			"the Elastic server's interface is %s/%d on VLAN %s, want 10.117.4.200/24 on MGMT",
			iface.Address(), iface.Mask(), iface.VLAN(),
		)
	}

	for _, name := range []string{"elasticsearch.yml", "kibana.yml"} {
		file := filepath.Join(exp.Spec.BaseDir(), "startup", "soh-elasticsearch-server-"+name)

		if _, err := os.Stat(file); err != nil {
			t.Errorf("Configure() did not render the Elastic server's %s: %v", name, err)
		}
	}
}

// inactiveC2MM is a minimega double whose C2 clients never become active.
type inactiveC2MM struct {
	mm.MM

	execs *atomic.Int32
}

func (m inactiveC2MM) ExecC2Command(...mm.C2Option) (string, error) {
	m.execs.Add(1)

	return "", fmt.Errorf("cannot execute command: %w", mm.ErrC2ClientNotActive)
}

// TestSOHFlowQueryGivesUpOnAnInactiveServer is a regression test: the soh
// app's flow query retried forever, ignoring its context, while the Elastic
// server's C2 client was not active, so a server whose miniccc never
// registered kept the post-start stage from ever finishing.
func TestSOHFlowQueryGivesUpOnAnInactiveServer(t *testing.T) { //nolint:paralleltest // replaces mm.DefaultMM
	original := mm.DefaultMM

	t.Cleanup(func() { mm.DefaultMM = original }) //nolint:reassign // restore test double

	execs := new(atomic.Int32)

	mm.DefaultMM = inactiveC2MM{execs: execs} //nolint:reassign // install test double

	exp := packetCaptureExperiment(t, "10.117.4.200/24")
	s := newSOH()

	if err := s.Configure(context.Background(), exp); err != nil {
		t.Fatalf("Configure() error = %v", err)
	}

	done := make(chan struct{})

	go func() {
		defer close(done)

		s.getFlows(context.Background(), exp)
	}()

	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatalf("the flow query still retries after 10s and %d attempts", execs.Load())
	}

	if got := execs.Load(); got != 1 {
		t.Errorf("the flow query ran query-flows.sh %d times, want 1", got)
	}
}
