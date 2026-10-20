package v1

import (
	"strings"
	"testing"
)

func TestSetVLANAlias(t *testing.T) {
	tests := []struct {
		name    string
		aliases map[string]int
		minVal  int
		maxVal  int
		id      int
		force   bool
		wantErr string // empty when the alias is set
	}{
		{name: "new alias", aliases: map[string]int{}, id: 320},
		{
			// Init adds every topology VLAN with ID 0.
			name:    "topology VLAN without an ID yet",
			aliases: map[string]int{"SW1": 0},
			id:      320,
		},
		{
			name:    "alias with an ID",
			aliases: map[string]int{"SW1": 310},
			id:      320,
			wantErr: "vlan alias SW1 already exists for experiment exp",
		},
		{name: "alias with an ID, forced", aliases: map[string]int{"SW1": 310}, id: 320, force: true},
		{
			name:    "below the range",
			aliases: map[string]int{"SW1": 0},
			minVal:  330,
			id:      320,
			wantErr: "less than experiment min VLAN ID of 330",
		},
		{
			name:    "above the range",
			aliases: map[string]int{"SW1": 0},
			maxVal:  310,
			id:      320,
			wantErr: "vlan ID 320 is at or above experiment max VLAN ID of 310",
		},
		// minimega assigns a range's VLAN IDs from its min up to, but not
		// including, its max.
		{name: "at the min", aliases: map[string]int{"SW1": 0}, minVal: 300, maxVal: 310, id: 300},
		{name: "below the max", aliases: map[string]int{"SW1": 0}, minVal: 300, maxVal: 310, id: 309},
		{
			name:    "at the max",
			aliases: map[string]int{"SW1": 0},
			minVal:  300,
			maxVal:  310,
			id:      310,
			wantErr: "vlan ID 310 is at or above experiment max VLAN ID of 310",
		},
		// Without a range, only the VLAN IDs minimega takes bound the ID.
		{name: "highest VLAN ID", aliases: map[string]int{"SW1": 0}, id: 4095},
		{name: "above 4095", aliases: map[string]int{"SW1": 0}, id: 4096, wantErr: "vlan ID 4096 is outside 1-4095"},
		{name: "negative", aliases: map[string]int{"SW1": 0}, id: -5, wantErr: "vlan ID -5 is outside 1-4095"},
		{name: "zero", aliases: map[string]int{"SW1": 0}, id: 0, wantErr: "vlan ID 0 is outside 1-4095"},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			spec := &ExperimentSpec{
				ExperimentNameF: "exp",
				VLANsF:          &VLANSpec{AliasesF: test.aliases, MinF: test.minVal, MaxF: test.maxVal},
			}

			err := spec.SetVLANAlias("SW1", test.id, test.force)

			if test.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), test.wantErr) {
					t.Fatalf("SetVLANAlias() error = %v, want one containing %q", err, test.wantErr)
				}

				return
			}

			if err != nil {
				t.Fatalf("SetVLANAlias() error = %v", err)
			}

			if got := spec.VLANsF.AliasesF["SW1"]; got != test.id {
				t.Errorf("alias SW1 = %d, want %d", got, test.id)
			}
		})
	}
}

func TestSetVLANRange(t *testing.T) {
	tests := []struct {
		name    string
		aliases map[string]int
		minVal  int
		maxVal  int
		wantErr string // empty when the range is set
	}{
		{
			// Init adds every topology VLAN with ID 0, which any range holds.
			name:    "topology VLANs without an ID yet",
			aliases: map[string]int{"SW1": 0, "SW2": 0},
			minVal:  300,
			maxVal:  310,
		},
		{name: "pinned VLAN inside the range", aliases: map[string]int{"SW1": 305}, minVal: 300, maxVal: 310},
		{name: "pinned VLANs at the min and below the max", aliases: map[string]int{"SW1": 300, "SW2": 309}, minVal: 300, maxVal: 310},
		{
			// minimega never assigns the max, so it is outside the range.
			name:    "pinned VLAN at the max",
			aliases: map[string]int{"SW1": 310},
			minVal:  300,
			maxVal:  310,
			wantErr: "topology VLAN SW1 (VLAN ID 310) is at or above proposed experiment max VLAN ID of 310",
		},
		{
			name:    "pinned VLAN below the range",
			aliases: map[string]int{"SW1": 290},
			minVal:  300,
			maxVal:  310,
			wantErr: "topology VLAN SW1 (VLAN ID 290) is less than proposed experiment min VLAN ID of 300",
		},
		{
			name:    "pinned VLAN above the range",
			aliases: map[string]int{"SW1": 320},
			minVal:  300,
			maxVal:  310,
			wantErr: "topology VLAN SW1 (VLAN ID 320) is at or above proposed experiment max VLAN ID of 310",
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			spec := &ExperimentSpec{ExperimentNameF: "exp", VLANsF: &VLANSpec{AliasesF: test.aliases}}

			err := spec.SetVLANRange(test.minVal, test.maxVal, false)

			if test.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), test.wantErr) {
					t.Fatalf("SetVLANRange() error = %v, want one containing %q", err, test.wantErr)
				}

				return
			}

			if err != nil {
				t.Fatalf("SetVLANRange() error = %v", err)
			}

			if spec.VLANsF.MinF != test.minVal || spec.VLANsF.MaxF != test.maxVal {
				t.Errorf("range = %d-%d, want %d-%d", spec.VLANsF.MinF, spec.VLANsF.MaxF, test.minVal, test.maxVal)
			}
		})
	}
}

// TestExperimentSpecInitRefusesAliasesAndHostsTheScriptWritesUnquoted is a
// regression test: a line break in a VLAN alias or a schedule host passed the
// experiment's initialization and added its own minimega command, such as
// `shell`, to the minimega script.
func TestExperimentSpecInitRefusesAliasesAndHostsTheScriptWritesUnquoted(t *testing.T) {
	for _, test := range []struct {
		name      string
		aliases   map[string]int
		schedules map[string]string
		want      string
	}{
		{name: "valid", aliases: map[string]int{"LAN": 300}, schedules: map[string]string{"web": "compute-0"}},
		{name: "VLAN alias", aliases: map[string]int{"LAN 300\nshell id": 301}, want: "VLAN alias: "},
		{name: "schedule host", schedules: map[string]string{"web": "compute-0\nshell id"}, want: "schedule host of node web: "},
	} {
		t.Run(test.name, func(t *testing.T) {
			spec := ExperimentSpec{
				ExperimentNameF: "exp",
				BaseDirF:        "/phenix/experiments/exp",
				VLANsF:          &VLANSpec{AliasesF: test.aliases},
				SchedulesF:      test.schedules,
			}

			err := spec.Init()

			if test.want == "" {
				if err != nil {
					t.Fatalf("Init() = %v", err)
				}

				return
			}

			if err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("Init() = %v, want one with %q", err, test.want)
			}
		})
	}
}
