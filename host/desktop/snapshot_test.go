package desktop

import (
	"fmt"
	"slices"
	"testing"
)

// Operation IDs are opaque; "share:10" sorts between "share:1" and "share:2".
// Insertion must neither re-log unchanged shares nor hide a real transition.
func TestShareStateLogsOnlyChangedSharesWhenInsertionShiftsOrder(t *testing.T) {
	for _, test := range []struct {
		name           string
		cancelExisting bool
		want           []string
	}{
		{name: "insertion only", want: []string{"share:10"}},
		{name: "insertion and existing transition", cancelExisting: true, want: []string{"share:10", "share:3"}},
	} {
		t.Run(test.name, func(t *testing.T) {
			var logged []string
			a := newApplication("session", dependencies{
				log: func(event string, value any) {
					if event == "host.share_state" {
						logged = append(logged, value.(map[string]any)["operation_id"].(string))
					}
				},
			})
			for n := 1; n <= 9; n++ {
				id := fmt.Sprintf("share:%d", n)
				a.operations[id] = &operation{id: id, phase: admission{}}
			}
			a.snapshot()
			logged = nil

			// Both changes must reach the same snapshot to exercise identity
			// matching while an existing operation moves in the sorted list.
			if test.cancelExisting {
				a.operations["share:3"].phase = admission{cancelling: true}
			}
			reply := a.execute(Request{Command: "cancel", OperationID: "share:10"})
			if reply.Error != nil {
				t.Fatalf("insert cancelled operation: %+v", reply.Error)
			}
			if !slices.Equal(logged, test.want) {
				t.Fatalf("host.share_state logged %v; want %v", logged, test.want)
			}
		})
	}
}
