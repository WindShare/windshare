package wire_test

import (
	"math"
	"testing"

	appobs "github.com/windshare/windshare/engine/observation"
	"github.com/windshare/windshare/engine/observation/wire"
)

func TestLossSnapshotsKeepProducerAndCumulativeSemantics(t *testing.T) {
	fact := appobs.LossSnapshot{
		WorkflowKind: appobs.WorkflowShare, Source: appobs.LossSenderAttemptCapacity,
		Category: appobs.ObserverLossSenderAttempt, Reason: appobs.ObserverLossStreamCapacity, Count: math.MaxUint64,
	}
	first, err := wire.Encode(fact)
	if err != nil {
		t.Fatal(err)
	}
	repeated, err := wire.Encode(fact)
	if err != nil {
		t.Fatal(err)
	}
	payload := first.Payload.(wire.LossSnapshotPayload)
	if first.Event != "observation_loss_snapshot" || first.Workflow != "share" ||
		payload.Source != "sender_attempt_capacity" || payload.CumulativeCount != "18446744073709551615" ||
		repeated.Payload != first.Payload {
		t.Fatalf("loss evidence changed: %+v %+v", first, repeated)
	}
	fact.Source = 0
	if _, err := wire.Encode(fact); err == nil {
		t.Fatal("accepted loss with no producer")
	}
}
