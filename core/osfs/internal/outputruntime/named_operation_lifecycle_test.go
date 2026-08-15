package outputruntime

import (
	"bytes"
	"context"
	"errors"
	"testing"

	"github.com/windshare/windshare/core/catalog"
	"github.com/windshare/windshare/core/osfs/internal/checkpointmodel"
	"github.com/windshare/windshare/core/osfs/internal/checkpointstore"
	"github.com/windshare/windshare/core/osfs/internal/directoryauthority"
	"github.com/windshare/windshare/core/osfs/internal/outputsession"
	"github.com/windshare/windshare/core/transfer"
	"github.com/windshare/windshare/core/transfer/receivecontract"
)

type ordinaryLifecycleFixture struct {
	authority   *Authority
	operation   *Operation
	directories *directoryauthority.Authority
	repository  *checkpointstore.Repository
	recorder    *ordinaryLifecycleRecorder
}

func openOrdinaryLifecycleFixture(t *testing.T, seed byte) ordinaryLifecycleFixture {
	t.Helper()
	authority := newNativeReservationTestAuthority(t, newRuntimeTestRootSpec(t).path)
	if _, err := authority.BindDestination(context.Background()); err != nil {
		t.Fatal(err)
	}
	selection := nativeReservationTestSelection(t, seed)
	artifact, err := receivecontract.NewSingleFileDirectoryTree(
		incrementalTestIdentity16[catalog.FileID](seed+2), "lifecycle.bin", "lifecycle.bin",
	)
	if err != nil {
		t.Fatal(err)
	}
	lookup, err := authority.LookupActive(context.Background(), selection)
	if err != nil {
		t.Fatal(err)
	}
	operation, err := authority.CreateOperation(context.Background(), lookup, artifact)
	if err != nil {
		t.Fatal(err)
	}
	directories, err := directoryauthority.New(operation.topLevel, directoryauthority.Config{})
	if err != nil {
		t.Fatal(err)
	}
	sessionID, err := newOutputSessionID(bytes.NewReader(bytes.Repeat(
		[]byte{seed + 3}, transfer.OutputSessionIdentityBytes,
	)))
	if err != nil {
		t.Fatal(err)
	}
	_, repository, store, err := authority.namedFileExecutor(operation, directories, sessionID)
	if err != nil {
		t.Fatal(err)
	}
	recorder, err := newOrdinaryLifecycleRecorder(operation, store, repository, sessionID)
	if err != nil {
		t.Fatal(err)
	}
	return ordinaryLifecycleFixture{
		authority: authority, operation: operation, directories: directories,
		repository: repository, recorder: recorder,
	}
}

func (fixture *ordinaryLifecycleFixture) close(t *testing.T) {
	t.Helper()
	if fixture.repository != nil {
		if err := fixture.repository.Close(); err != nil {
			t.Fatal(err)
		}
	}
	if fixture.directories != nil {
		if err := fixture.directories.Close(); err != nil {
			t.Fatal(err)
		}
	}
	if fixture.authority != nil {
		if err := fixture.authority.Close(); err != nil {
			t.Fatal(err)
		}
	}
}

func TestOrdinaryLifecycleRecorderSeparatesCommandOutcomeFromDurableCleanup(t *testing.T) {
	var nilRecorder *ordinaryLifecycleRecorder
	if err := nilRecorder.RecordTreeSettlement(
		context.Background(), transfer.DirectTreeSettlementSuccess,
		transfer.DirectTreeOutcomeSuccess, outputsession.TreeSettlementSnapshot{},
	); !errors.Is(err, transfer.ErrInvalidOutputBinding) {
		t.Fatalf("nil recorder = %v", err)
	}
	fixture := openOrdinaryLifecycleFixture(t, 0xd1)
	defer fixture.close(t)

	canceled, cancel := context.WithCancel(context.Background())
	cancel()
	if err := fixture.recorder.RecordTreeSettlement(
		canceled, transfer.DirectTreeSettlementPartial,
		transfer.DirectTreeOutcomePartial, outputsession.TreeSettlementSnapshot{},
	); !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled settlement = %v", err)
	}
	if err := fixture.recorder.RecordTreeSettlement(
		context.Background(), transfer.DirectTreeSettlementKind(0xff),
		transfer.DirectTreeOutcomePartial, outputsession.TreeSettlementSnapshot{},
	); !errors.Is(err, transfer.ErrInvalidOutputSettlement) {
		t.Fatalf("unknown settlement = %v", err)
	}
	for _, kind := range []transfer.DirectTreeSettlementKind{
		transfer.DirectTreeSettlementPartial,
		transfer.DirectTreeSettlementPaused,
	} {
		if err := fixture.recorder.RecordTreeSettlement(
			context.Background(), kind, transfer.DirectTreeOutcomePartial,
			outputsession.TreeSettlementSnapshot{},
		); err != nil {
			t.Fatalf("retryable settlement %d = %v", kind, err)
		}
		if lifecycle := fixture.operation.lease.Record().Lifecycle(); lifecycle != checkpointmodel.OrdinaryOperationActive {
			t.Fatalf("retryable settlement %d ended operation as %d", kind, lifecycle)
		}
	}

	var traces []FilesystemOutputTrace
	fixture.authority.tracer = FilesystemOutputTraceFunc(func(event FilesystemOutputTrace) {
		traces = append(traces, event)
	})
	// Simulate losing the private file-state capability after every final was
	// published. The command remains successful while the durable row records
	// exactly why explicit cleanup is still required.
	fixture.recorder.store = nil
	if err := fixture.recorder.RecordTreeSettlement(
		context.Background(), transfer.DirectTreeSettlementSuccess,
		transfer.DirectTreeOutcomeSuccess, outputsession.TreeSettlementSnapshot{},
	); err != nil {
		t.Fatal(err)
	}
	record := fixture.operation.lease.Record()
	if record.Lifecycle() != checkpointmodel.OrdinaryOperationCleanupPending ||
		record.ClosedReason() != checkpointmodel.OrdinaryReasonCleanupUncertain {
		t.Fatalf("cleanup failure lifecycle = (%d, %d)", record.Lifecycle(), record.ClosedReason())
	}
	if len(traces) != 1 || traces[0].RuntimeDecision != FilesystemOutputRuntimeCleanupPending ||
		!traces[0].Failed {
		t.Fatalf("cleanup trace = %+v", traces)
	}
}

func TestOrdinaryLifecycleRecorderQuarantinesOperationWideFailure(t *testing.T) {
	fixture := openOrdinaryLifecycleFixture(t, 0xe1)
	defer fixture.close(t)
	if err := fixture.recorder.RecordTreeSettlement(
		context.Background(), transfer.DirectTreeSettlementFailed,
		transfer.DirectTreeOutcomeFailed, outputsession.TreeSettlementSnapshot{},
	); err != nil {
		t.Fatal(err)
	}
	record := fixture.operation.lease.Record()
	if record.Lifecycle() != checkpointmodel.OrdinaryOperationNeedsAttention ||
		record.ClosedReason() != checkpointmodel.OrdinaryReasonOperationOwnershipUnknown {
		t.Fatalf("operation failure lifecycle = (%d, %d)", record.Lifecycle(), record.ClosedReason())
	}
}
