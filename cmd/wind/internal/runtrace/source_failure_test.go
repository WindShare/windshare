package runtrace

import (
	"encoding/json"
	"testing"

	"github.com/windshare/windshare/cmd/wind/internal/clievent"
)

func TestSourceFailureTraceKeepsThePathAndReasonAtTaskAndCommandBoundaries(t *testing.T) {
	const path = "selected/文件\nfile.bin"
	failure, err := clievent.NewSourcePathFailure(clievent.FailureSourceMissing, clievent.NewDisplayPath(path))
	if err != nil {
		t.Fatal(err)
	}
	task, _ := clievent.NewEngineTaskID("application:42")
	identity := make([]byte, clievent.IdentityBytes)
	identity[0] = 1
	share, _ := clievent.NewSharingInstanceID(identity)
	sourceEvent, err := clievent.NewEngineTaskObserved(clievent.EngineTaskSpec{
		Command: clievent.CommandShare, Task: task, Stage: clievent.EngineSourceFailed, ShareInstance: share, Failure: failure,
	})
	if err != nil {
		t.Fatal(err)
	}
	commandEvent, err := clievent.NewCommandFailed(clievent.CommandShare, clievent.ExitUsage, failure)
	if err != nil {
		t.Fatal(err)
	}
	for _, event := range []clievent.Event{sourceEvent, commandEvent} {
		record, err := encodeV4(testRunIdentity(1), entryMetadata{sequence: 1}, event)
		if err != nil {
			t.Fatal(err)
		}
		raw, err := json.Marshal(record.Payload)
		if err != nil {
			t.Fatal(err)
		}
		var decoded struct {
			Failure failureV4 `json:"failure"`
		}
		if err := json.Unmarshal(raw, &decoded); err != nil {
			t.Fatal(err)
		}
		if decoded.Failure.Code != "source_missing" || decoded.Failure.MessageKey != "source_missing" || decoded.Failure.SourcePath != path {
			t.Fatalf("source failure lost machine-readable context: %s", raw)
		}
	}
}
