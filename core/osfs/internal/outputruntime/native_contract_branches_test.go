package outputruntime

import (
	"bytes"
	"context"
	"errors"
	"io"
	"testing"

	"github.com/windshare/windshare/core/catalog"
	"github.com/windshare/windshare/core/osfs/internal/destinationauthority"
	"github.com/windshare/windshare/core/osfs/internal/outputcap"
	"github.com/windshare/windshare/core/osfs/internal/outputsession"
	"github.com/windshare/windshare/core/transfer"
	transferfault "github.com/windshare/windshare/core/transfer/fault"
	"github.com/windshare/windshare/core/transfer/ordinaryoutput"
	"github.com/windshare/windshare/core/transfer/receivecontract"
)

type createAuthorityFailureDirectory struct {
	outputcap.Directory
	err error
}

func (directory createAuthorityFailureDirectory) ValidateCreateAuthority() error {
	return directory.err
}

func TestNativeAuthorityGuardsRejectUnstagedOrCanceledCapabilities(t *testing.T) {
	ctx := context.Background()
	var nilAuthority *Authority
	if _, err := nilAuthority.BindDestination(ctx); !errors.Is(err, transfer.ErrInvalidOutputBinding) {
		t.Fatalf("nil bind = %v", err)
	}
	withoutFactory, _ := New(Config{})
	if _, err := withoutFactory.BindDestination(ctx); !errors.Is(err, transfer.ErrInvalidOutputBinding) {
		t.Fatalf("factory-free bind = %v", err)
	}
	canceled, cancel := context.WithCancel(ctx)
	cancel()
	authority := newNativeReservationTestAuthority(t, newRuntimeTestRootSpec(t).path)
	if _, err := authority.BindDestination(canceled); !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled bind = %v", err)
	}
	if _, err := authority.LookupActive(ctx, transfer.SelectionSpec{}); !errors.Is(err, transfer.ErrInvalidOutputBinding) {
		t.Fatalf("zero selection lookup = %v", err)
	}
	if _, err := authority.BindDestination(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err := authority.BindDestination(ctx); !errors.Is(err, transfer.ErrInvalidOutputBinding) {
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
	if _, err := authority.OpenDirectTree(ctx, transfer.ReceiveIntent{}); !errors.Is(err, transfer.ErrInvalidOutputBinding) {
		t.Fatalf("zero facade intent = %v", err)
	}
}

func TestNativeIdentityAndAdapterBranchesRemainFailClosed(t *testing.T) {
	if (ExecutionMode{mode: outputcap.ExecutionResumable}).Valid() != true ||
		(ExecutionMode{mode: outputcap.ExecutionLiveOnly}).Valid() != true ||
		(ExecutionMode{}).Valid() {
		t.Fatal("execution mode validity changed")
	}
	for kind := ActiveLookupMiss; kind <= ActiveLookupAmbiguous; kind++ {
		if !kind.Valid() {
			t.Fatalf("lookup kind %d is invalid", kind)
		}
	}
	if ActiveLookupKind(0).Valid() || (&Operation{}).ExecutionMode().Valid() {
		t.Fatal("zero runtime vocabulary became valid")
	}
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
	if operation := (ActiveLookup{kind: ActiveLookupMiss}).Operation(); operation != nil {
		t.Fatalf("miss exposed operation %T", operation)
	}
	if err := (&Operation{}).close(); err != nil {
		t.Fatalf("empty operation close = %v", err)
	}
	var nilAdmission *heldAdmission
	if err := nilAdmission.prepare(receivecontract.OperationID{}); !errors.Is(err, transfer.ErrInvalidOutputBinding) {
		t.Fatalf("nil admission prepare = %v", err)
	}
	expected := &Operation{}
	if operation := (ActiveLookup{kind: ActiveLookupReopened, operation: expected}).Operation(); operation != expected {
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
	if session, err := (cryptographicOutputSessionIDs{}).NewOutputSessionID(); err != nil || session.IsZero() {
		t.Fatalf("cryptographic session identity = (%x, %v)", session.Bytes(), err)
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
	if got := ordinaryLifecycleInterface(nil); got != nil {
		t.Fatalf("nil lifecycle projected as %T", got)
	}
	if _, err := newOrdinaryLifecycleRecorder(nil, nil, nil, transfer.OutputSessionID{}); !errors.Is(err, transfer.ErrInvalidOutputBinding) {
		t.Fatalf("invalid lifecycle recorder = %v", err)
	}
	if _, err := newOperationDestinationBinder(destinationauthority.ReservedEntry{}); !errors.Is(err, transfer.ErrInvalidOutputBinding) {
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
	if observation, err := (*liveFileExecutor)(nil).BeginFile(context.Background(), outputsession.FileClaim{}); err == nil ||
		observation.Cut != outputsession.MutationNoChange {
		t.Fatalf("nil live executor = (%d, %v)", observation.Cut, err)
	}

	failure := errors.New("create authority rejected")
	if err := validateOutputCreateAuthority(nil); err != nil {
		t.Fatalf("optional create validator = %v", err)
	}
	if err := validateOutputCreateAuthority(createAuthorityFailureDirectory{err: failure}); !errors.Is(err, failure) {
		t.Fatalf("create validator failure = %v", err)
	}
	if filesystemOutputCertificationFromState(outputcap.CertificationLinuxExt4ProcessRestart) !=
		FilesystemOutputCertificationLinuxExt4ProcessRestart ||
		filesystemOutputCertificationFromState(outputcap.CertificationWindowsNTFSProcessRestart) !=
			FilesystemOutputCertificationWindowsNTFSProcessRestart ||
		filesystemOutputCertificationFromState(outputcap.CertificationID("")) != "" {
		t.Fatal("filesystem certification projection changed")
	}
	var traces []FilesystemOutputTrace
	FilesystemOutputTraceFunc(func(event FilesystemOutputTrace) {
		traces = append(traces, event)
	}).TraceFilesystemOutput(FilesystemOutputTrace{Operation: TraceRuntimeDecision})
	FilesystemOutputTraceFunc(nil).TraceFilesystemOutput(FilesystemOutputTrace{})
	runtimeAuthority := &Authority{tracer: FilesystemOutputTraceFunc(func(event FilesystemOutputTrace) {
		traces = append(traces, event)
	})}
	runtimeAuthority.trace(FilesystemOutputTrace{Operation: TraceSessionOpened})
	(*Authority)(nil).trace(FilesystemOutputTrace{})
	if len(traces) != 2 {
		t.Fatalf("trace callbacks = %d", len(traces))
	}
}

func TestVolatileReservationClaimsSerializeAndRollbackExactNames(t *testing.T) {
	operationID, _ := receivecontract.OperationIDFromBytes(bytes.Repeat(
		[]byte{0x71}, receivecontract.StableIdentityBytes,
	))
	reservationID, _ := receivecontract.DestinationReservationIDFromBytes(bytes.Repeat(
		[]byte{0x72}, receivecontract.StableIdentityBytes,
	))
	authorityRef, _ := receivecontract.AuthorityRefFromBytes(bytes.Repeat(
		[]byte{0x73}, receivecontract.AuthorityRefBytes,
	))
	resultRoot := receivecontract.NewSyntheticSelectionResultRoot()
	artifact, err := receivecontract.NewResultRootDirectoryTree(resultRoot)
	if err != nil {
		t.Fatal(err)
	}
	reservation, err := receivecontract.NewNativeNamedEntryReservation(
		operationID, reservationID, artifact, authorityRef, resultRoot.Name(), 0,
	)
	if err != nil {
		t.Fatal(err)
	}
	spec := destinationauthority.ReservationClaimSpec{
		CanonicalNameKey: resultRoot.Name(), OperationID: operationID, ReservationID: reservationID,
		EntryKind: reservation.EntryKind(), RequestedName: resultRoot.Name(), ReservedName: resultRoot.Name(),
	}
	claimer, err := newVolatileReservationClaimer(authorityRef)
	if err != nil {
		t.Fatal(err)
	}
	defer claimer.Close()
	handle, outcome, err := claimer.BeginReservation(spec)
	if err != nil || outcome != destinationauthority.ReservationMetadataClaimCommitted {
		t.Fatalf("volatile claim = (%d, %v)", outcome, err)
	}
	if outcome, err = handle.BindReservation(reservation); err != nil ||
		outcome != destinationauthority.ReservationMetadataClaimCommitted {
		t.Fatalf("bind volatile reservation = (%d, %v)", outcome, err)
	}
	if outcome, err = handle.BindDirectoryIdentity([]byte("identity")); err != nil ||
		outcome != destinationauthority.ReservationMetadataClaimCommitted {
		t.Fatalf("bind volatile identity = (%d, %v)", outcome, err)
	}
	if !handle.Claim().Valid() {
		t.Fatal("volatile claim did not advance")
	}
	competitor, _ := newVolatileReservationClaimer(authorityRef)
	defer competitor.Close()
	if collided, outcome, err := competitor.BeginReservation(spec); err != nil || collided != nil ||
		outcome != destinationauthority.ReservationMetadataClaimCollision {
		t.Fatalf("volatile collision = (%T, %d, %v)", collided, outcome, err)
	}
	if outcome, err := handle.Rollback(); err != nil ||
		outcome != destinationauthority.ReservationMetadataClaimCommitted {
		t.Fatalf("volatile rollback = (%d, %v)", outcome, err)
	}
	replacement, outcome, err := competitor.BeginReservation(spec)
	if err != nil || replacement == nil || outcome != destinationauthority.ReservationMetadataClaimCommitted {
		t.Fatalf("post-rollback claim = (%T, %d, %v)", replacement, outcome, err)
	}
	if err := replacement.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := newVolatileReservationClaimer(receivecontract.AuthorityRef{}); !errors.Is(err, transfer.ErrInvalidOutputBinding) {
		t.Fatalf("zero volatile authority = %v", err)
	}
	wrapped := &heldAdmission{volatile: claimer}
	if err := wrapped.close(); err != nil || wrapped.volatile != nil {
		t.Fatalf("held volatile admission close = %v", err)
	}
	var nilAdmission *heldAdmission
	if err := nilAdmission.close(); err != nil {
		t.Fatalf("nil held admission close = %v", err)
	}
	var nilDurableHandle *admissionReservationClaimHandle
	if nilDurableHandle.Claim().Valid() {
		t.Fatal("nil durable wrapper exposed a claim")
	}
	if _, err := nilDurableHandle.BindReservation(reservation); !errors.Is(err, transfer.ErrInvalidOutputBinding) {
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
	var nilClaimer *volatileReservationClaimer
	if _, _, err := nilClaimer.BeginReservation(spec); !errors.Is(err, transfer.ErrInvalidOutputBinding) {
		t.Fatalf("nil volatile claim = %v", err)
	}
	var nilHandle *volatileReservationClaimHandle
	if !nilHandle.Claim().Valid() {
		// The zero claim is intentionally invalid.
	} else {
		t.Fatal("nil volatile handle exposed a valid claim")
	}
	if _, err := nilHandle.BindReservation(reservation); !errors.Is(err, transfer.ErrInvalidOutputBinding) {
		t.Fatalf("nil volatile bind = %v", err)
	}
	if _, err := nilHandle.BindDirectoryIdentity([]byte("identity")); !errors.Is(err, transfer.ErrInvalidOutputBinding) {
		t.Fatalf("nil volatile identity = %v", err)
	}
	if _, err := nilHandle.Rollback(); !errors.Is(err, transfer.ErrInvalidOutputBinding) {
		t.Fatalf("nil volatile rollback = %v", err)
	}
	if err := nilHandle.Close(); err != nil {
		t.Fatalf("nil volatile close = %v", err)
	}
}
