package desktop

import (
	"context"
	"fmt"
	"testing"

	"github.com/windshare/windshare/engine"
	"github.com/windshare/windshare/engine/relayconfig"
	"github.com/windshare/windshare/engine/turn"
)

// Desktop operation IDs are "share:N"; snapshots sort them lexically, so
// "share:10" lands between "share:1" and "share:2". A new share must not make
// every unchanged share after its sorted position look like a state change.
func TestShareStateLogsOnlyChangedSharesWhenInsertionShiftsOrder(t *testing.T) {
	var logged []string
	a := newApplication("session", dependencies{
		start: func(context.Context, engine.ShareRequest) (shareTask, error) {
			return nil, fmt.Errorf("unexpected start")
		},
		close:       func(context.Context) error { return nil },
		configure:   func(turn.Settings) error { return nil },
		load:        func() (relayconfig.Config, error) { return relayconfig.Config{}, nil },
		cancelSetup: func(context.Context, string) error { return nil },
		log: func(event string, value any) {
			if view, ok := value.(Share); ok && event == "host.share_state" {
				logged = append(logged, view.OperationID)
			}
		},
	})
	t.Cleanup(func() { _ = a.close() })
	for n := 1; n <= 9; n++ {
		a.execute(Request{Command: "cancel", OperationID: fmt.Sprintf("share:%d", n)})
	}
	logged = nil
	a.execute(Request{Command: "cancel", OperationID: "share:10"})
	if len(logged) != 1 || logged[0] != "share:10" {
		t.Fatalf("host.share_state logged %d unchanged or changed shares %v; want only [share:10]", len(logged), logged)
	}
}
