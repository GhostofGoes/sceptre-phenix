package broker

import (
	"encoding/base64"
	"encoding/json"
	"errors"
	"strings"

	"phenix/api/vm"
	"phenix/app"
	putil "phenix/util"
	"phenix/util/plog"
	"phenix/util/pubsub"
	bt "phenix/web/broker/brokertypes"
	"phenix/web/util"
)

const (
	brokerChannelBuffer = 1024
	triggerStateError   = "error"
)

var (
	clients    = make(map[*Client]bool)                     //nolint:gochecknoglobals // global state
	broadcast  = make(chan bt.Publish, brokerChannelBuffer) //nolint:gochecknoglobals // global state
	register   = make(chan *Client, brokerChannelBuffer)    //nolint:gochecknoglobals // global state
	unregister = make(chan *Client, brokerChannelBuffer)    //nolint:gochecknoglobals // global state
)

func Start() {
	triggerSub := pubsub.Subscribe("trigger-app")
	delayedSub := pubsub.Subscribe("delayed-start")

	for {
		select {
		case pub := <-triggerSub:
			var (
				trigger, _ = pub.(app.TriggerPublication)
				typ        = "apps/" + trigger.App

				policy   = bt.NewRequestPolicy("experiments/trigger", "create", trigger.Experiment)
				resource = bt.NewResource(typ, trigger.Experiment, trigger.State)
			)

			if trigger.Verb != "" {
				policy.Verb = trigger.Verb
			}

			if trigger.Resource != "" {
				resource.Name = trigger.Resource
			}

			if trigger.State == triggerStateError {
				broadcast <- bt.Publish{RequestPolicy: policy, Resource: resource, Result: errorResult(trigger.Error)}
			} else {
				broadcast <- bt.Publish{RequestPolicy: policy, Resource: resource, Result: nil}
			}
		case pub := <-delayedSub:
			delayed, _ := pub.(string)

			expName, vmName, ok := strings.Cut(delayed, "/")
			if !ok {
				plog.Error(plog.TypeSystem, "unexpected delayed-start publication", "name", delayed)

				continue
			}

			// asks minimega for the VM and a screenshot, so it runs on its own
			// rather than holding up every other client's messages
			go publishDelayedStart(delayed, expName, vmName)
		case cli := <-register:
			clients[cli] = true
		case cli := <-unregister:
			if _, ok := clients[cli]; ok {
				cli.Stop()
				delete(clients, cli)
			}
		case pub := <-broadcast:
			for cli := range clients {
				var (
					policy = pub.RequestPolicy
					allow  bool
				)

				switch {
				case policy == nil:
					allow = true
				case policy.ResourceName == "":
					allow = cli.role.Allowed(policy.Resource, policy.Verb)
				default:
					allow = cli.role.Allowed(policy.Resource, policy.Verb, policy.ResourceName)
				}

				if allow {
					select {
					case cli.publish <- pub:
					default:
						cli.Stop()
						delete(clients, cli)
					}
				}
			}
		}
	}
}

func Broadcast(policy *bt.RequestPolicy, resource *bt.Resource, msg json.RawMessage) {
	broadcast <- bt.Publish{RequestPolicy: policy, Resource: resource, Result: msg}
}

// errorResult is the result of an app error publication: the error's message,
// humanized when it can be.
func errorResult(err error) []byte {
	msg := err.Error()

	var humanized *putil.HumanizedError
	if errors.As(err, &humanized) {
		msg = humanized.Humanize()
	}

	result, _ := json.Marshal(map[string]string{triggerStateError: msg})

	return result
}

// publishDelayedStart tells clients a VM held back by delayed start is now
// running.
func publishDelayedStart(delayed, expName, vmName string) {
	v, err := vm.Get(expName, vmName)
	if err != nil {
		return
	}

	screenshot, err := util.GetScreenshot(expName, vmName, "215")
	if err == nil {
		v.Screenshot = "data:image/png;base64," + base64.StdEncoding.EncodeToString(
			screenshot,
		)
	}

	body, err := marshaler.Marshal(util.VMToProtobuf(expName, *v, nil))
	if err != nil {
		return
	}

	// RBAC resource names for VMs are exp/vm, as in the REST handlers.
	policy := bt.NewRequestPolicy("vms/start", "update", delayed)
	resource := bt.NewResource("experiment/vm", delayed, "start")

	broadcast <- bt.Publish{RequestPolicy: policy, Resource: resource, Result: body}
}
