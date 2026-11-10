package runtrace

import (
	"encoding/json"
	"reflect"
	"testing"

	"github.com/windshare/windshare/cmd/wind/internal/clievent"
	appobs "github.com/windshare/windshare/engine/observation"
	wire "github.com/windshare/windshare/engine/observation/wire"
)

func TestSourceFailureTraceKeepsContextAtTaskAndCommandBoundaries(t *testing.T) {
	const first, second = "selected/文件\nfile.bin", "other/文件\nfile.bin"
	missing, err := appobs.NewSourcePathFailure(appobs.FailureSourceMissing, appobs.NewDisplayPath(first))
	if err != nil {
		t.Fatal(err)
	}
	collision, err := appobs.NewSelectionCollisionFailure(appobs.NewDisplayPath(first), appobs.NewDisplayPath(second))
	if err != nil {
		t.Fatal(err)
	}
	for _, test := range []struct {
		name    string
		failure appobs.Failure
		want    wire.Failure
	}{
		{"missing source", missing, wire.Failure{Code: "source_missing", MessageKey: "source_missing", SourcePath: first}},
		{"selection collision", collision, wire.Failure{
			Code: "selection_collision", MessageKey: "selection_collision",
			SelectionCollision: &wire.SelectionCollision{FirstPath: first, SecondPath: second},
		}},
	} {
		t.Run(test.name, func(t *testing.T) {
			task, _ := appobs.NewEngineTaskID("application:42")
			sourceEvent, err := appobs.NewEngineTaskObserved(appobs.EngineTaskSpec{
				Workflow: appobs.WorkflowShare, Task: task, Stage: appobs.EngineTaskSettled, Outcome: appobs.EngineOutcomeFailed,
				FailureClass: appobs.EngineFailureUsage, Failure: test.failure,
			})
			if err != nil {
				t.Fatal(err)
			}
			commandEvent, err := clievent.NewCommandFailed(clievent.CommandShare, clievent.ExitUsage, test.failure)
			if err != nil {
				t.Fatal(err)
			}
			for _, event := range []clievent.Event{sourceEvent, commandEvent} {
				record, err := encode(testRunIdentity(1), entryMetadata{sequence: 1}, event)
				if err != nil {
					t.Fatal(err)
				}
				raw, err := json.Marshal(record.Payload)
				if err != nil {
					t.Fatal(err)
				}
				var decoded struct {
					Failure wire.Failure `json:"failure"`
				}
				if err := json.Unmarshal(raw, &decoded); err != nil {
					t.Fatal(err)
				}
				if !reflect.DeepEqual(decoded.Failure, test.want) {
					t.Fatalf("source failure lost machine-readable context: %s", raw)
				}
			}
		})
	}
}
