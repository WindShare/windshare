package desktop

import (
	"crypto/ed25519"
	"fmt"
	"slices"
	"testing"

	"github.com/windshare/windshare/core/link"
	"github.com/windshare/windshare/engine"
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

func TestProjectTaskPublishesAndClearsBackupURL(t *testing.T) {
	publicKey := ed25519.NewKeyFromSeed(make([]byte, ed25519.SeedSize)).Public().(ed25519.PublicKey)
	capability, err := link.NewSenderAuthenticated(make([]byte, link.ReadSecretBytes), publicKey, nil)
	if err != nil {
		t.Fatal(err)
	}
	capability.NodeAliases = []string{"cn1", "us1"}

	task := &fakeShare{
		id: "engine:1",
		state: engine.ShareSnapshot{
			State:   engine.TaskRunning,
			Startup: engine.ShareReadyReached,
			Ready: engine.ShareReady{
				Capability:        capability,
				Frontend:          "https://ws.example",
				AlternateFrontend: "https://backup.example",
			},
		},
	}
	var view Share
	projectTask(&view, running{task: task})
	wantPrimary, err := capability.URL("https://ws.example")
	if err != nil {
		t.Fatal(err)
	}
	wantBackup, err := capability.URL("https://backup.example")
	if err != nil {
		t.Fatal(err)
	}
	if view.URL != wantPrimary || view.BackupURL != wantBackup {
		t.Fatalf("projected URLs = (%q, %q), want (%q, %q)", view.URL, view.BackupURL, wantPrimary, wantBackup)
	}

	task.state.Ready.AlternateFrontend = ""
	var explicit Share
	projectTask(&explicit, running{task: task})
	if explicit.URL != wantPrimary || explicit.BackupURL != "" {
		t.Fatalf("explicit share URLs = (%q, %q), want (%q, %q)", explicit.URL, explicit.BackupURL, wantPrimary, "")
	}

	task.state.Ready.AlternateFrontend = "https://backup.example"
	task.state.State = engine.TaskStopping
	var stopping Share
	projectTask(&stopping, running{task: task})
	if stopping.URL != "" || stopping.BackupURL != "" {
		t.Fatalf("stopping share retained links: (%q, %q)", stopping.URL, stopping.BackupURL)
	}

	task.state.State = engine.TaskFinished
	task.state.Result = &engine.TaskResult[engine.ShareResult]{Outcome: engine.OutcomeStopped, StopReason: engine.ShareStopped}
	var finished Share
	projectTask(&finished, running{task: task})
	if finished.URL != "" || finished.BackupURL != "" {
		t.Fatalf("finished share retained links: (%q, %q)", finished.URL, finished.BackupURL)
	}
}
