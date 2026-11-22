package outputruntime

import (
	"bytes"
	"context"
	"errors"
	"io"
	"testing"

	"github.com/windshare/windshare/core/catalog"
	"github.com/windshare/windshare/core/transfer"
	transferfault "github.com/windshare/windshare/core/transfer/fault"
	"github.com/windshare/windshare/core/transfer/ordinaryoutput"
	"github.com/windshare/windshare/core/transfer/receivecontract"
	"github.com/windshare/windshare/osfs/internal/destinationauthority"
	"github.com/windshare/windshare/osfs/internal/outputsession"
)

func TestNativeAuthorityGuardsRejectUnstagedOrCanceledCapabilities(t *testing.T) {
	ctx := context.Background()
	var nilAuthority *Authority
	if err := nilAuthority.BindDestination(ctx); !errors.Is(err, transfer.ErrInvalidOutputBinding) {
		t.Fatalf("nil bind = %v", err)
	}
	withoutFactory, _ := New(Config{})
	if err := withoutFactory.BindDestination(ctx); !errors.Is(err, transfer.ErrInvalidOutputBinding) {
		t.Fatalf("factory-free bind = %v", err)
	}
	canceled, cancel := context.WithCancel(ctx)
	cancel()
	authority := newNativeReservationTestAuthority(t, newRuntimeTestRootSpec(t).path)
	if err := authority.BindDestination(canceled); !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled bind = %v", err)
	}
	if _, err := authority.LookupActive(ctx, transfer.SelectionSpec{}); !errors.Is(err, transfer.ErrInvalidOutputBinding) {
		t.Fatalf("zero selection lookup = %v", err)
	}
	if err := authority.BindDestination(ctx); err != nil {
		t.Fatal(err)
	}
	if err := authority.BindDestination(ctx); !errors.Is(err, transfer.ErrInvalidOutputBinding) {
		t.Fatalf("repeat bind = %v", err)
	}
	selection := nativeReservationTestSelection(t, 0x61)
	if _, err := authority.LookupActive(canceled, selection); !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled lookup = %v", err)
	}
	lookup, err := authority.LookupActive(ctx, selection)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := authority.CreateOperation(canceled, lookup, receivecontract.ArtifactSpec{}); !errors.Is(err, transfer.ErrInvalidOutputBinding) {
		t.Fatalf("zero artifact create = %v", err)
	}
	artifact, err := receivecontract.NewSingleFileDirectoryTree(
		incrementalTestIdentity16[catalog.FileID](0x63), "guard.bin", "guard.bin",
	)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := authority.CreateOperation(canceled, lookup, artifact); !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled create = %v", err)
	}
	if _, err := authority.OpenOperation(ctx, nil); !errors.Is(err, transfer.ErrInvalidOutputBinding) {
		t.Fatalf("nil operation open = %v", err)
	}
	if err := authority.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := authority.LookupActive(ctx, selection); !errors.Is(err, transfer.ErrInvalidOutputBinding) {
		t.Fatalf("closed lookup = %v", err)
	}
}

func TestNativeIdentityAndAdapterBranchesRemainFailClosed(t *testing.T) {
	ambiguous, err := transferfault.NewOutput(
		transferfault.ScopeFileLocal, transferfault.OutputMutationAmbiguous,
	)
	if err != nil {
		t.Fatal(err)
	}
	if successfulOrExecutionCut(nil) != outputsession.MutationStable ||
		executionMutationCut(errors.New("private failure")) != outputsession.MutationNoChange ||
		executionMutationCut(transferfault.Wrap(ambiguous, errors.New("uncertain publish"))) != outputsession.MutationAmbiguous {
		t.Fatal("file execution mutation cut changed")
	}
	if operation := (ActiveLookup{kind: LookupCreateReady}).Operation(); operation != nil {
		t.Fatalf("miss exposed operation %T", operation)
	}
	if err := (&Operation{}).close(); err != nil {
		t.Fatalf("empty operation close = %v", err)
	}
	var nilAdmission *heldAdmission
	if err := nilAdmission.prepare(receivecontract.OperationID{}); !errors.Is(err, transfer.ErrInvalidOutputBinding) {
		t.Fatalf("nil admission prepare = %v", err)
	}
	if err := nilAdmission.close(); err != nil {
		t.Fatalf("nil held admission close = %v", err)
	}
	expected := &Operation{}
	if operation := (ActiveLookup{kind: LookupReopened, operation: expected}).Operation(); operation != expected {
		t.Fatal("reopened lookup lost exact operation")
	}
	if _, err := randomStableIdentity(nil); !errors.Is(err, transfer.ErrInvalidOutputBinding) {
		t.Fatalf("nil identity entropy = %v", err)
	}
	if _, err := randomStableIdentity(bytes.NewReader([]byte{1})); !errors.Is(err, io.ErrUnexpectedEOF) {
		t.Fatalf("short identity entropy = %v", err)
	}
	if _, err := newOperationID(bytes.NewReader(make([]byte, maximumStableIdentityGenerationAttempts*receivecontract.StableIdentityBytes))); !errors.Is(err, transfer.ErrInvalidOutputBinding) {
		t.Fatalf("zero operation entropy = %v", err)
	}
	if _, err := newReservationID(nil); !errors.Is(err, transfer.ErrInvalidOutputBinding) {
		t.Fatalf("nil reservation entropy = %v", err)
	}
	if _, err := newOutputSessionID(nil); !errors.Is(err, transfer.ErrInvalidOutputBinding) {
		t.Fatalf("nil session entropy = %v", err)
	}
	if _, err := newOutputSessionID(bytes.NewReader([]byte{1})); !errors.Is(err, io.ErrUnexpectedEOF) {
		t.Fatalf("short session entropy = %v", err)
	}
	if session, err := newOutputSessionID(bytes.NewReader(bytes.Repeat([]byte{1}, transfer.OutputSessionIdentityBytes))); err != nil ||
		session.IsZero() {
		t.Fatalf("session identity = (%x, %v)", session.Bytes(), err)
	}
	if _, err := newOrdinaryLifecycleRecorder(nil, nil, nil, transfer.OutputSessionID{}); !errors.Is(err, transfer.ErrInvalidOutputBinding) {
		t.Fatalf("invalid lifecycle recorder = %v", err)
	}
	if _, err := newOperationDestinationBinder(nil); !errors.Is(err, transfer.ErrInvalidOutputBinding) {
		t.Fatalf("invalid destination binder = %v", err)
	}
	if _, err := (operationDestinationBinder{}).BindArtifactPath(
		ordinaryoutput.ArtifactPath{},
	); !errors.Is(err, transfer.ErrInvalidOutputBinding) {
		t.Fatalf("invalid artifact binding = %v", err)
	}
	if _, err := operationSessionCapabilities(nil); !errors.Is(err, transfer.ErrInvalidOutputBinding) {
		t.Fatalf("nil session capabilities = %v", err)
	}
	if err := validateNamedOperationIntent(transfer.ReceiveIntent{}, destinationauthority.ReservedEntry{}); !errors.Is(err, transfer.ErrInvalidOutputBinding) {
		t.Fatalf("invalid named intent = %v", err)
	}
	if observation, err := (fileExecutionAdapter{}).BeginFile(context.Background(), outputsession.FileClaim{}); err == nil ||
		observation.Cut != outputsession.MutationNoChange {
		t.Fatalf("nil file adapter = (%d, %v)", observation.Cut, err)
	}

	var nilDurableHandle *admissionReservationClaimHandle
	if nilDurableHandle.Claim().Valid() {
		t.Fatal("nil durable wrapper exposed a claim")
	}
	if _, err := nilDurableHandle.BindReservation(receivecontract.DestinationReservation{}); !errors.Is(err, transfer.ErrInvalidOutputBinding) {
		t.Fatalf("nil durable reservation bind = %v", err)
	}
	if _, err := nilDurableHandle.BindDirectoryIdentity([]byte("identity")); !errors.Is(err, transfer.ErrInvalidOutputBinding) {
		t.Fatalf("nil durable identity bind = %v", err)
	}
	if _, err := nilDurableHandle.Rollback(); !errors.Is(err, transfer.ErrInvalidOutputBinding) {
		t.Fatalf("nil durable rollback = %v", err)
	}
	if err := nilDurableHandle.Close(); err != nil {
		t.Fatalf("nil durable wrapper close = %v", err)
	}

	var traces []Trace
	TraceFunc(func(event Trace) {
		traces = append(traces, event)
	}).TraceOutput(Trace{Operation: TraceRuntimeDecision})
	TraceFunc(nil).TraceOutput(Trace{})
	runtimeAuthority := &Authority{tracer: TraceFunc(func(event Trace) {
		traces = append(traces, event)
	})}
	runtimeAuthority.trace(Trace{Operation: TraceCheckpointReconciled})
	(*Authority)(nil).trace(Trace{})
	if len(traces) != 2 {
		t.Fatalf("trace callbacks = %d", len(traces))
	}
}
