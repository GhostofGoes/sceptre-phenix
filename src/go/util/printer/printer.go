package printer

import (
	"encoding/json"
	"fmt"
	"io"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/olekukonko/tablewriter"
	"github.com/olekukonko/tablewriter/renderer"
	"github.com/olekukonko/tablewriter/tw"

	"phenix/store"
	"phenix/types"
	"phenix/util/mm"
)

const (
	// wrapWidth matches the column width tablewriter v0 wrapped cells at.
	wrapWidth            = 30
	imageConfigFixedCols = 7
	headerName           = "Name"
	headerValue          = "Value"
)

// newTable returns a table that renders with ASCII borders, matching the
// output of earlier phenix releases. Header auto-formatting is disabled because
// it splits words like "VCPUs" apart; renderTable upper-cases headers instead.
// When wrap is true, row cells wrap at wrapWidth.
func newTable(writer io.Writer, wrap bool, opts ...tablewriter.Option) *tablewriter.Table {
	defaults := []tablewriter.Option{
		tablewriter.WithRenderer(
			//nolint:exhaustruct // partial initialization
			renderer.NewBlueprint(tw.Rendition{Symbols: tw.NewSymbols(tw.StyleASCII)}),
		),
		tablewriter.WithHeaderAutoFormat(tw.Off),
	}

	if wrap {
		defaults = append(defaults, tablewriter.WithRowMaxWidth(wrapWidth))
	} else {
		defaults = append(defaults, tablewriter.WithRowAutoWrap(tw.WrapNone))
	}

	return tablewriter.NewTable(writer, append(defaults, opts...)...)
}

// renderTable writes the header and rows to the table, then renders it.
func renderTable(table *tablewriter.Table, header []string, rows [][]string) error {
	upper := make([]string, len(header))

	for i, h := range header {
		upper[i] = strings.ToUpper(h)
	}

	table.Header(upper)

	if err := table.Bulk(rows); err != nil {
		return fmt.Errorf("adding rows to table: %w", err)
	}

	if err := table.Render(); err != nil {
		return fmt.Errorf("rendering table: %w", err)
	}

	return nil
}

// PrintTableOfConfigs writes the given configs to the given writer as an ASCII
// table. The table headers are set to Kind, Version, Name, and Created.
func PrintTableOfConfigs(writer io.Writer, configs store.Configs) error {
	rows := make([][]string, 0, len(configs))

	for _, c := range configs {
		rows = append(rows, []string{c.Kind, c.Version, c.Metadata.Name, c.Metadata.Created})
	}

	return renderTable(newTable(writer, true), []string{"Kind", "Version", headerName, "Created"}, rows)
}

// PrintTableOfExperiments writes the given experiments to the given writer as
// an ASCII table. The table headers are set to Name, Topology, Scenario,
// Started, VM Count, VLAN Count, and Apps.
func PrintTableOfExperiments(writer io.Writer, exps ...types.Experiment) error {
	rows := make([][]string, 0, len(exps))

	for _, exp := range exps {
		apps := make([]string, 0, len(exp.Apps()))

		for _, app := range exp.Apps() {
			apps = append(apps, app.Name())
		}

		rows = append(rows, []string{
			exp.Spec.ExperimentName(),
			exp.Metadata.Annotations["topology"],
			exp.Metadata.Annotations["scenario"],
			exp.Status.StartTime(),
			strconv.Itoa(len(exp.Spec.Topology().Nodes())),
			strconv.Itoa(len(exp.Spec.VLANs().Aliases())),
			strings.Join(apps, ", "),
		})
	}

	return renderTable(
		newTable(writer, true),
		[]string{headerName, "Topology", "Scenario", "Started", "VM Count", "VLAN Count", "Apps"},
		rows,
	)
}

// PrintTableOfVMs writes the given VMs to the given writer as an ASCII table.
func PrintTableOfVMs(writer io.Writer, includeTaps bool, vms ...mm.VM) error {
	switch len(vms) {
	case 0:
		return nil
	case 1:
		header, rows := buildSingleVMTable(includeTaps, vms[0])

		return renderTable(
			newTable(writer, false, tablewriter.WithRowAlignment(tw.AlignLeft)),
			header,
			rows,
		)
	default:
		header, rows := buildMultipleVMTable(includeTaps, vms...)

		return renderTable(newTable(writer, false), header, rows)
	}
}

func buildMultipleVMTable(includeTaps bool, vms ...mm.VM) ([]string, [][]string) {
	header := []string{
		"Host",
		headerName,
		"Running",
		"Disk",
		"Interfaces",
		"Uptime",
		"Memory",
		"VCPUs",
		"OS Type",
	}
	if includeTaps {
		header = append(header, "Taps")
	}

	rows := make([][]string, 0, len(vms))

	for _, vm := range vms {
		var (
			running = strconv.FormatBool(vm.Running)
			ifaces  = make([]string, 0, len(vm.Networks))
			uptime  string
		)

		for idx, nw := range vm.Networks {
			ifaces = append(ifaces, fmt.Sprintf("ID: %d, IP: %s, VLAN: %s", idx, vm.IPv4[idx], nw))
		}

		if vm.Running {
			uptime = (time.Duration(vm.Uptime) * time.Second).String()
		}

		row := []string{
			vm.Host,
			vm.Name,
			running,
			vm.Disk,
			strings.Join(ifaces, "\n"),
			uptime,
			strconv.Itoa(vm.RAM),
			strconv.Itoa(vm.CPUs),
			vm.OSType,
		}
		if includeTaps {
			row = append(row, strings.Join(vm.Taps, ", "))
		}

		rows = append(rows, row)
	}

	return header, rows
}

func buildSingleVMTable(includeTaps bool, vm mm.VM) ([]string, [][]string) {
	var (
		ifaces   = make([]string, 0, len(vm.Networks))
		uptime   string
		metadata []byte
		labels   []string
	)

	for idx, nw := range vm.Networks {
		ifaces = append(ifaces, fmt.Sprintf("ID: %d, IP: %s, VLAN: %s", idx, vm.IPv4[idx], nw))
	}

	if vm.Running {
		uptime = (time.Duration(vm.Uptime) * time.Second).String()
	}

	if len(vm.Metadata) > 0 {
		metadata, _ = json.MarshalIndent(vm.Metadata, "", "  ")
	}

	if len(vm.Labels) > 0 {
		for lbl, val := range vm.Labels {
			labels = append(labels, fmt.Sprintf("%s: %s", lbl, val))
		}
	}

	rows := [][]string{
		{"Host", vm.Host},
		{headerName, vm.Name},
		{"Running", strconv.FormatBool(vm.Running)},
		{"Disk", vm.Disk},
		{"Interfaces", strings.Join(ifaces, "\n")},
		{"Uptime", uptime},
		{"VCPUs", strconv.Itoa(vm.CPUs)},
		{"Memory", strconv.Itoa(vm.RAM)},
		{"OS Type", vm.OSType},
	}
	if includeTaps {
		rows = append(rows, []string{"Taps", strings.Join(vm.Taps, ", ")})
	}

	rows = append(rows,
		[]string{"Labels", strings.Join(labels, ", ")},
		[]string{"Metadata", string(metadata)},
	)

	return []string{"Setting", headerValue}, rows
}

func PrintTableOfImageConfigs(writer io.Writer, optional []string, imgs ...types.Image) error {
	cols := make([]string, 0, imageConfigFixedCols+len(optional))

	cols = append(cols, headerName, "Size", "Variant", "Release", "Overlays", "Packages", "Scripts")
	cols = append(cols, optional...)

	rows := make([][]string, 0, len(imgs))

	for _, img := range imgs {
		scripts := make([]string, 0, len(img.Spec.Scripts))

		for s := range img.Spec.Scripts {
			scripts = append(scripts, s)
		}

		row := []string{
			img.Metadata.Name,
			img.Spec.Size,
			img.Spec.Variant,
			img.Spec.Release,
			strings.Join(img.Spec.Overlays, "\n"),
			strings.Join(img.Spec.Packages, "\n"),
			strings.Join(scripts, "\n"),
		}

		for _, col := range optional {
			switch col {
			case "Format":
				row = append(row, string(img.Spec.Format))
			case "Compressed":
				row = append(row, strconv.FormatBool(img.Spec.Compress))
			case "Mirror":
				row = append(row, img.Spec.Mirror)
			}
		}

		rows = append(rows, row)
	}

	return renderTable(newTable(writer, true), cols, rows)
}

func PrintTableOfVLANAliases(writer io.Writer, info map[string]map[string]int) error {
	experiments := make([]string, 0, len(info))

	for exp := range info {
		experiments = append(experiments, exp)
	}

	sort.Strings(experiments)

	var rows [][]string

	for _, exp := range experiments {
		aliases := make([]string, 0, len(info[exp]))

		for alias := range info[exp] {
			aliases = append(aliases, alias)
		}

		sort.Strings(aliases)

		for _, alias := range aliases {
			rows = append(rows, []string{exp, alias, strconv.Itoa(info[exp][alias])})
		}
	}

	return renderTable(newTable(writer, true), []string{"Experiment", "VLAN Alias", "VLAN ID"}, rows)
}

func PrintTableOfVLANRanges(writer io.Writer, info map[string][2]int) error {
	experiments := make([]string, 0, len(info))

	for exp := range info {
		experiments = append(experiments, exp)
	}

	sort.Strings(experiments)

	rows := make([][]string, 0, len(experiments))

	for _, exp := range experiments {
		r := fmt.Sprintf("%d - %d", info[exp][0], info[exp][1])

		rows = append(rows, []string{exp, r})
	}

	return renderTable(newTable(writer, true), []string{"Experiment", "VLAN Range"}, rows)
}

func PrintTableOfSubnetCaptures(writer io.Writer, captures []mm.Capture) error {
	rows := make([][]string, 0, len(captures))

	for _, capture := range captures {
		rows = append(
			rows,
			[]string{capture.VM, strconv.Itoa(capture.Interface), capture.Filepath},
		)
	}

	return renderTable(
		newTable(writer, true),
		[]string{headerName, "Interface Index", "File Path"},
		rows,
	)
}

func PrintTableOfSettings(writer io.Writer, settings []types.Setting) error {
	rows := make([][]string, 0, len(settings))

	for _, setting := range settings {
		rows = append(rows, []string{
			setting.Spec.Name,
			setting.Spec.Category,
			setting.Spec.Value,
		})
	}

	table := newTable(
		writer,
		true,
		tablewriter.WithRowAlignmentConfig(tw.CellAlignment{ //nolint:exhaustruct // partial initialization
			PerColumn: []tw.Align{tw.AlignLeft, tw.AlignLeft, tw.AlignRight},
		}),
	)

	return renderTable(table, []string{headerName, "Category", headerValue}, rows)
}

func PrintTableOfRuntimeSettings(writer io.Writer, settings map[string]any) error {
	var data [][]string

	var flatten func(prefix string, m map[string]any)
	flatten = func(prefix string, m map[string]any) {
		for k, v := range m {
			newPrefix := k
			if prefix != "" {
				newPrefix = prefix + "." + k
			}
			switch val := v.(type) {
			case map[string]any:
				flatten(newPrefix, val)
			default:
				data = append(data, []string{newPrefix, fmt.Sprintf("%v", val)})
			}
		}
	}

	flatten("", settings)

	// Sort data by key
	sort.Slice(data, func(i, j int) bool {
		return data[i][0] < data[j][0]
	})

	table := newTable(writer, false, tablewriter.WithRowAlignment(tw.AlignLeft))

	return renderTable(table, []string{"Key", headerValue}, data)
}
