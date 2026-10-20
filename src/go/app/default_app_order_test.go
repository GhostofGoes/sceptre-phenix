package app_test

import (
	"slices"
	"testing"

	"phenix/app"
	"phenix/store"
	"phenix/types"
	v1 "phenix/types/version/v1"
	"phenix/util/pubsub"
)

// documentedDefaultApps is the order docs/content/apps.md lists the default
// apps in.
var documentedDefaultApps = []string{"ntp", "serial", "startup", "vrouter"} //nolint:gochecknoglobals // test constant

func TestDefaultAppsInDocumentedOrder(t *testing.T) {
	for range 20 {
		if got := app.DefaultApps(); !slices.Equal(got, documentedDefaultApps) {
			t.Fatalf("DefaultApps() = %v, want %v", got, documentedDefaultApps)
		}
	}
}

// TestApplyAppsRunsDefaultAppsInDocumentedOrder runs the configure stage
// several times and checks the order the default apps start in, which phenix
// publishes for the web UI.
func TestApplyAppsRunsDefaultAppsInDocumentedOrder(t *testing.T) {
	exp := &types.Experiment{
		Metadata: store.ConfigMetadata{Name: "default-app-order"},
		Spec: &v1.ExperimentSpec{
			BaseDirF:  t.TempDir(),
			TopologyF: &v1.TopologySpec{},
		},
	}

	events := pubsub.Subscribe("trigger-app")
	started := make(chan []string)

	go func() {
		var names []string

		for msg := range events {
			if p, ok := msg.(app.TriggerPublication); ok && p.State == "start" {
				names = append(names, p.App)
			}
		}

		started <- names
	}()

	const runs = 10

	for range runs {
		if err := app.ApplyApps(t.Context(), exp, app.Stage(app.ActionConfigure)); err != nil {
			t.Errorf("ApplyApps(configure) error = %v", err)
		}
	}

	pubsub.Unsubscribe("trigger-app", events)
	close(events)

	got := <-started
	want := slices.Repeat(documentedDefaultApps, runs)

	if !slices.Equal(got, want) {
		t.Fatalf("default apps started in order %v, want %v on every run", got, documentedDefaultApps)
	}
}
