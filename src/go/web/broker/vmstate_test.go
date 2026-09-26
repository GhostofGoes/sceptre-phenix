package broker

import (
	"testing"

	bt "phenix/web/broker/brokertypes"
)

func TestTrackVMState(t *testing.T) {
	c := &Client{vms: []vmScope{
		{exp: "exp", name: "vm1", running: false},
		{exp: "exp", name: "vm2", running: true},
		{exp: "other", name: "vm1", running: false},
	}}

	c.trackVMState(bt.Publish{Resource: bt.NewResource("experiment/vm", "exp/vm1", "start")})
	c.trackVMState(bt.Publish{Resource: bt.NewResource("experiment/vm", "exp/vm2", "stop")})
	// ignored: another resource type, a bare name, and another action
	c.trackVMState(bt.Publish{Resource: bt.NewResource("experiment", "exp/vm2", "start")})
	c.trackVMState(bt.Publish{Resource: bt.NewResource("experiment/vm", "vm2", "start")})
	c.trackVMState(bt.Publish{Resource: bt.NewResource("experiment/vm", "exp/vm2", "restarting")})
	c.trackVMState("not a publish")

	want := []bool{true, false, false}
	for i, v := range c.vms {
		if v.running != want[i] {
			t.Errorf("%s/%s running = %v, want %v", v.exp, v.name, v.running, want[i])
		}
	}
}
