package fileexecution

import (
	"context"
	"errors"
	"testing"

	"github.com/windshare/windshare/core/transfer"
	"github.com/windshare/windshare/osfs/internal/checkpointmodel"
)

type deliveryHistoryRepository struct {
	CheckpointRepository
	record checkpointmodel.Record
	found  bool
	err    error
	query  checkpointmodel.CheckpointLineageSpec
}

func (repository *deliveryHistoryRepository) LookupDeliveredRecord(
	_ context.Context,
	lineage checkpointmodel.CheckpointLineageSpec,
) (checkpointmodel.Record, bool, error) {
	repository.query = lineage
	return repository.record, repository.found, repository.err
}

func TestDeliveryHistoryTraceCorrelatesRecoveredReceiptsAndFailures(t *testing.T) {
	transaction, checkpoints, _, _ := newCoveragePartialFileTransaction(t, 1)
	ctx := context.Background()
	if err := transaction.WriteRange(ctx, 0, []byte{1}); err != nil {
		t.Fatal(err)
	}
	if _, err := transaction.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	record := checkpoints.record
	lineage, err := record.CheckpointLineageSpec()
	if err != nil {
		t.Fatal(err)
	}
	lookupFailure := fileStateError(errors.New("receipt read failed"))
	for _, test := range []struct {
		name    string
		found   bool
		err     error
		outcome TraceOutcome
	}{
		{name: "published receipt", found: true, outcome: TraceReconciled},
		{name: "no receipt"},
		{name: "read failure", err: lookupFailure, outcome: TraceNoChange},
	} {
		t.Run(test.name, func(t *testing.T) {
			var events []TraceEvent
			repository := &deliveryHistoryRepository{record: record, found: test.found, err: test.err}
			engine := transaction.engine
			engine.checkpoints = repository
			// Historical delivery must not reopen the public file or acquire write
			// authority; it remains valid after the user moves that file away.
			engine.directories, engine.platform = nil, nil
			engine.trace = TraceSinkFunc(func(event TraceEvent) { events = append(events, event) })
			delivery, found, err := engine.LookupDelivery(ctx, record.FileID(), record.CanonicalPath())
			if found != test.found || !errors.Is(err, test.err) || repository.query != lineage {
				t.Fatalf("delivery lookup = found %t err %v lineage %+v", found, err, repository.query)
			}
			if found {
				want := transfer.HistoricalDelivery{OperationID: record.OperationID(), FileID: record.FileID(), FileRevision: record.FileRevision(), MaterializationPath: record.CanonicalPath(), Size: record.ExactSize()}
				if delivery != want {
					t.Fatalf("historical delivery = %+v, want %+v", delivery, want)
				}
			} else if delivery != (transfer.HistoricalDelivery{}) {
				t.Fatalf("missing receipt manufactured delivery: %+v", delivery)
			}
			if test.outcome == 0 {
				if len(events) != 0 {
					t.Fatalf("missing receipt manufactured recovery evidence: %+v", events)
				}
				return
			}
			if len(events) != 1 {
				t.Fatalf("delivery lookup emitted %d events, want one", len(events))
			}
			event := events[0]
			if event.Operation != TraceDeliveryReceipt || event.Outcome != test.outcome ||
				event.OperationID != record.OperationID() || event.IntentDigest != engine.intent.Digest() ||
				event.SessionID != engine.sessionID || event.FileID != record.FileID() || event.Sequence == 0 {
				t.Fatalf("delivery recovery lost its decision or identity: %+v", event)
			}
			if event.Fault.Valid() != (test.err != nil) {
				t.Fatalf("delivery recovery fault = %+v for error %v", event.Fault, test.err)
			}
			if test.found {
				if event.Previous != checkpointmodel.PhasePublished || event.Next != checkpointmodel.PhasePublished {
					t.Fatalf("historical delivery changed publication state: %+v", event)
				}
			} else if event.Previous != 0 || event.Next != 0 {
				t.Fatalf("failed receipt read invented a publication state: %+v", event)
			}
		})
	}
}
