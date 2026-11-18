package tmpl_test

import (
	"bytes"
	"reflect"
	"sort"
	"testing"

	"gopkg.in/yaml.v3"

	"phenix/tmpl"
)

// renderYAML renders the named template with data and decodes the result,
// failing the test unless it is a YAML mapping.
func renderYAML(t *testing.T, name string, data any) map[string]any {
	t.Helper()

	var buf bytes.Buffer

	if err := tmpl.GenerateFromTemplate(name, data, &buf); err != nil {
		t.Fatalf("GenerateFromTemplate(%s) error = %v", name, err)
	}

	var config map[string]any

	if err := yaml.Unmarshal(buf.Bytes(), &config); err != nil {
		t.Fatalf("%s does not render valid YAML: %v\n%s", name, err, buf.String())
	}

	return config
}

// TestElasticsearchConfigServesPlainHTTPOnElastic8 is a regression test: the
// soh Elastic server's elasticsearch.yml left security at the Elasticsearch 8
// default, which with a non-loopback bind fails the TLS bootstrap check, and
// left the TLS layers unset while the package's keystore holds their passwords,
// so Elasticsearch 8 did not start.
func TestElasticsearchConfigServesPlainHTTPOnElastic8(t *testing.T) {
	t.Parallel()

	data := struct {
		Hostname       string
		ExperimentName string
	}{Hostname: "soh-elasticsearch-server", ExperimentName: "exp"}

	config := renderYAML(t, "elasticsearch.yml.tmpl", data)

	want := map[string]any{
		"cluster.name":                         "exp",
		"node.name":                            "soh-elasticsearch-server",
		"network.host":                         "0.0.0.0",
		"http.port":                            9200,
		"xpack.security.enabled":               false,
		"xpack.security.http.ssl.enabled":      false,
		"xpack.security.transport.ssl.enabled": false,
		"cluster.initial_master_nodes":         []any{"soh-elasticsearch-server"},
	}

	for key, value := range want {
		if got, ok := config[key]; !ok || !reflect.DeepEqual(got, value) {
			t.Errorf("%s = %#v, want %#v", key, got, value)
		}
	}

	// Elasticsearch refuses single-node discovery together with
	// cluster.initial_master_nodes.
	if got, ok := config["discovery.type"]; ok {
		t.Errorf("discovery.type = %#v, want it unset next to cluster.initial_master_nodes", got)
	}

	// Elasticsearch 7 rejects the enrollment setting; 8 defaults it to false.
	if got, ok := config["xpack.security.enrollment.enabled"]; ok {
		t.Errorf("xpack.security.enrollment.enabled = %#v, want it unset", got)
	}
}

// TestKibanaConfigHasOnlySettingsKibana8Accepts is a regression test: the soh
// Elastic server's kibana.yml set kibana.index, kibana.defaultAppId and
// elasticsearch.startupTimeout, which Kibana 8 removed and refuses to start on.
func TestKibanaConfigHasOnlySettingsKibana8Accepts(t *testing.T) {
	t.Parallel()

	config := renderYAML(t, "kibana.yml.tmpl", "soh-elasticsearch-server")

	keys := make([]string, 0, len(config))

	for key := range config {
		keys = append(keys, key)
	}

	sort.Strings(keys)

	want := []string{
		"elasticsearch.hosts",
		"elasticsearch.pingTimeout",
		"elasticsearch.requestTimeout",
		"server.host",
		"server.name",
		"server.port",
	}

	if !reflect.DeepEqual(keys, want) {
		t.Errorf("kibana.yml sets %v, want only %v", keys, want)
	}

	if got := config["server.name"]; got != "soh-elasticsearch-server" {
		t.Errorf("server.name = %#v, want the Elastic server's hostname", got)
	}

	if got, want := config["elasticsearch.hosts"], []any{"http://localhost:9200"}; !reflect.DeepEqual(got, want) {
		t.Errorf("elasticsearch.hosts = %#v, want %#v", got, want)
	}
}

// TestPacketbeatConfigCapturesWithoutKibanaAndReportsDeltas is a regression
// test: the soh monitors' packetbeat.yml loaded Kibana dashboards, and
// Packetbeat 8 exits before it captures when Kibana does not answer; and its
// flow reports carried running totals, which the soh app adds up, so long
// flows were counted many times over.
func TestPacketbeatConfigCapturesWithoutKibanaAndReportsDeltas(t *testing.T) {
	t.Parallel()

	data := struct {
		ElasticServer string
		Hostname      string
	}{ElasticServer: "10.117.4.200", Hostname: "rtu-1-soh-monitor"}

	config := renderYAML(t, "packetbeat.yml.tmpl", data)

	want := map[string]any{
		"packetbeat.interfaces.device": "eth1",
		"packetbeat.interfaces.type":   "pcap",
		"packetbeat.flows": map[string]any{
			"enabled":                   true,
			"timeout":                   "30s",
			"period":                    "10s",
			"enable_delta_flow_reports": true,
		},
		"name":                     "rtu-1-soh-monitor",
		"setup.dashboards.enabled": false,
		"setup.kibana":             map[string]any{"host": "http://10.117.4.200:5601"},
		"output.elasticsearch": map[string]any{
			"hosts":    []any{"10.117.4.200:9200"},
			"protocol": "http",
		},
	}

	for key, value := range want {
		if got, ok := config[key]; !ok || !reflect.DeepEqual(got, value) {
			t.Errorf("%s = %#v, want %#v", key, got, value)
		}
	}
}
