package outputruntime

import (
	"bytes"
	"context"
	"crypto/sha256"
	"errors"
	"sync"

	"github.com/windshare/windshare/core/transfer"
	transferfault "github.com/windshare/windshare/core/transfer/fault"
	"github.com/windshare/windshare/core/transfer/receivecontract"
	"github.com/windshare/windshare/osfs/internal/checkpointmodel"
	"github.com/windshare/windshare/osfs/internal/checkpointstore"
	"github.com/windshare/windshare/osfs/internal/destinationauthority"
	"github.com/windshare/windshare/osfs/internal/namestore"
	"github.com/windshare/windshare/osfs/internal/outputnames"
)

type Operation struct {
	authority    *Authority
	key          checkpointmodel.ActiveOperationKey
	intent       transfer.ReceiveIntent
	topLevel     *destinationauthority.TopLevelReservation
	claim        destinationauthority.ReservationClaim
	lease        *checkpointstore.OperationRegistryLease
	reopened     bool
	opened       bool
	closed       bool
	names        *outputnames.Mapper
	nameStore    *namestore.Store
	nameReportMu sync.Mutex
	nameReport   namestore.ReportSummary
}

func (operation *Operation) ReceiveIntent() (transfer.ReceiveIntent, bool) {
	if operation == nil || operation.closed || operation.intent.IsZero() {
		return transfer.ReceiveIntent{}, false
	}
	return operation.intent, true
}

func (operation *Operation) close() error {
	if operation == nil || operation.closed {
		return nil
	}
	operation.closed = true
	// The public capability closes before releasing operation/name ownership, so
	// another admission cannot observe a half-owned operation.
	var err error
	if operation.topLevel != nil {
		err = errors.Join(err, operation.topLevel.Close())
		operation.topLevel = nil
	}
	if operation.lease != nil {
		err = errors.Join(err, operation.lease.Close())
		operation.lease = nil
	}
	return err
}

func (authority *Authority) LookupActive(
	ctx context.Context,
	selection transfer.SelectionSpec,
) (ActiveLookup, error) {
	return authority.SelectOperation(ctx, selection, receivecontract.AutomaticOperation())
}

func (authority *Authority) SelectOperation(
	ctx context.Context,
	selection transfer.SelectionSpec,
	selector receivecontract.OperationSelection,
) (result ActiveLookup, resultErr error) {
	defer func() {
		if resultErr != nil && !errors.Is(resultErr, ErrShareStopped) {
			resultErr = DiagnoseFailure(FailureLookup, resultErr)
		}
	}()
	if authority == nil || ctx == nil || selection.IsZero() || !selector.Valid() {
		return ActiveLookup{}, transfer.ErrInvalidOutputBinding
	}
	if err := ctx.Err(); err != nil {
		return ActiveLookup{}, err
	}
	authority.mu.Lock()
	defer authority.mu.Unlock()
	if authority.closed || authority.stage != authorityStageBound ||
		authority.destination == nil || !authority.binding.Valid() || authority.registry == nil {
		return ActiveLookup{}, transfer.ErrInvalidOutputBinding
	}
	key, err := checkpointmodel.NewActiveOperationKeyV1(selection.Digest(), authority.binding.AuthorityRef())
	if err != nil {
		return ActiveLookup{}, err
	}
	var operationID receivecontract.OperationID
	if selector.Kind() == receivecontract.OperationRedownload {
		operationID, err = newOperationID(authority.random)
		if err != nil {
			return ActiveLookup{}, err
		}
		key = checkpointmodel.IsolatedOperationKey(operationID)
	}
	var activeAdmission checkpointstore.ActiveAdmission
	var registryLookup checkpointstore.ActiveLookup
	if selector.Kind() == receivecontract.OperationByID {
		registryLookup, err = authority.registry.LookupOperation(selector.ID(), selection)
		key = registryLookup.Record().ActiveOperationKey()
	} else {
		activeAdmission, registryLookup, err = authority.registry.BeginActive(key)
	}
	if err != nil {
		return ActiveLookup{}, DiagnoseFailure(
			FailureLookup,
			checkpointRuntimeError(ctx, "begin active ordinary operation", err),
		)
	}
	switch registryLookup.State() {
	case checkpointstore.ActiveLookupSelectionMismatch:
		authority.stage = authorityStageLookupStopped
		authority.traceSelectedOperationMismatch(registryLookup.Record(), registryLookup.Intent())
		return ActiveLookup{authority: authority, kind: LookupNotFound}, nil
	case checkpointstore.ActiveLookupNone:
		if selector.Kind() == receivecontract.OperationByID {
			authority.stage = authorityStageLookupStopped
			return ActiveLookup{authority: authority, kind: LookupNotFound}, nil
		}
		admission := &heldAdmission{
			key: key, selection: selection, durable: &activeAdmission, operationID: operationID,
		}
		authority.admission, authority.stage = admission, authorityStageLookupHeld
		return ActiveLookup{authority: authority, kind: LookupCreateReady, admission: admission}, nil
	case checkpointstore.ActiveLookupReopenable:
		operation, attention, err := authority.reopenOperationLocked(ctx, selection, key, &registryLookup)
		if err != nil {
			return ActiveLookup{}, DiagnoseFailure(FailureOpen, err)
		}
		if attention != AttentionNone {
			authority.stage = authorityStageLookupStopped
			return ActiveLookup{
				authority: authority, kind: LookupNeedsAttention, attention: attention,
			}, nil
		}
		authority.operation, authority.stage = operation, authorityStageOperationReady
		return ActiveLookup{authority: authority, kind: LookupReopened, operation: operation}, nil
	case checkpointstore.ActiveLookupShareStopped:
		authority.stage = authorityStageLookupStopped
		intent := registryLookup.Intent()
		if intent.IsZero() || !sameSelection(intent.SelectionSpec(), selection) {
			return ActiveLookup{}, transfer.ErrInvalidOutputBinding
		}
		return ActiveLookup{}, ErrShareStopped
	case checkpointstore.ActiveLookupAlreadyRunning:
		authority.stage = authorityStageLookupStopped
		return ActiveLookup{authority: authority, kind: LookupAlreadyRunning}, nil
	case checkpointstore.ActiveLookupNeedsAttention:
		attention := attentionReasonFromClosed(registryLookup.Record().ClosedReason())
		if !attention.Valid() {
			return ActiveLookup{}, DiagnoseFailure(FailureLookup, transfer.ErrInvalidOutputBinding)
		}
		authority.stage = authorityStageLookupStopped
		return ActiveLookup{
			authority: authority, kind: LookupNeedsAttention, attention: attention,
		}, nil
	case checkpointstore.ActiveLookupAmbiguous:
		authority.stage = authorityStageLookupStopped
		return ActiveLookup{authority: authority, kind: LookupAmbiguous}, nil
	default:
		return ActiveLookup{}, transfer.ErrInvalidOutputBinding
	}
}

func (authority *Authority) reopenOperationLocked(
	ctx context.Context,
	selection transfer.SelectionSpec,
	key checkpointmodel.ActiveOperationKey,
	lookup *checkpointstore.ActiveLookup,
) (*Operation, AttentionReason, error) {
	lease := lookup.TakeLease()
	if lease == nil {
		return nil, AttentionNone, transfer.ErrInvalidOutputBinding
	}
	record := lookup.Record()
	intent := lookup.Intent()
	var err error
	if intent.IsZero() {
		err = checkpointmodel.ErrInvalidOrdinaryOperation
	}
	proof := lookup.RecoveryProof()
	var reservation receivecontract.DestinationReservation
	var direct bool
	if err == nil {
		reservation, direct = intent.MaterializationPlan().DestinationReservation()
	}
	// A selection mismatch here is a binding or index mismatch, not evidence
	// that the retained operation lost ownership of its destination.
	if err == nil && !sameSelection(intent.SelectionSpec(), selection) {
		return nil, AttentionNone, errors.Join(transfer.ErrInvalidOutputBinding, lease.Close())
	}
	if err != nil || !direct ||
		!validNamedReservation(reservation, intent.ArtifactSpec(), authority.binding) ||
		!proof.Valid() || record.ReservationClaim().Token() != [sha256.Size]byte(proof.Claim().Token) ||
		record.ReservationClaim().Generation() != proof.Claim().Generation {
		attentionErr := requireOperationAttention(lease, checkpointmodel.OrdinaryReasonOperationOwnershipUnknown)
		closeErr := lease.Close()
		if attentionErr != nil || closeErr != nil {
			return nil, AttentionNone, checkpointRuntimeError(ctx, "quarantine invalid ordinary operation", errors.Join(err, attentionErr, closeErr))
		}
		return nil, AttentionOperation, nil
	}
	topLevel, err := authority.destination.ReopenTopLevel(destinationauthority.ExpectedReservation{
		Reservation: reservation, PersistentIdentityClaim: proof.PersistentIdentity(),
		MetadataClaim: proof.Claim(),
	})
	if err != nil {
		attentionErr := requireOperationAttention(lease, checkpointmodel.OrdinaryReasonDestinationOwnershipUnknown)
		closeErr := lease.Close()
		if attentionErr != nil || closeErr != nil {
			return nil, AttentionNone, errors.Join(
				runtimeOutputError(ctx, transferfault.OutputOwnership, "reopen exact top-level reservation", err),
				checkpointRuntimeError(ctx, "persist reopen attention", errors.Join(attentionErr, closeErr)),
			)
		}
		return nil, AttentionDestination, nil
	}
	operation := &Operation{
		authority: authority, key: key, intent: intent,
		topLevel: topLevel, claim: proof.Claim(), lease: lease, reopened: true,
	}
	if err := operation.validate(authority); err != nil {
		return nil, AttentionNone, errors.Join(err, topLevel.Close(), lease.Close())
	}
	return operation, AttentionNone, nil
}

func requireOperationAttention(
	lease *checkpointstore.OperationRegistryLease,
	reason checkpointmodel.OrdinaryClosedReason,
) error {
	return transitionOutputOperation(lease, checkpointmodel.OrdinaryLifecycleRequireAttention, reason)
}

func transitionOutputOperation(
	lease *checkpointstore.OperationRegistryLease,
	event checkpointmodel.OrdinaryLifecycleEvent,
	reason checkpointmodel.OrdinaryClosedReason,
) error {
	if lease == nil {
		return transfer.ErrInvalidOutputBinding
	}
	previous := lease.Record()
	lifecycle, closedReason, err := checkpointmodel.ReduceOrdinaryOperationLifecycle(
		previous.Lifecycle(), event, reason,
	)
	if err != nil {
		return err
	}
	next, err := checkpointmodel.NextOrdinaryOperationRecord(
		previous,
		checkpointmodel.NextOrdinaryOperationRecordSpec{
			Lifecycle: lifecycle, Lease: checkpointmodel.OrdinaryLeaseHeld, ClosedReason: closedReason,
		},
	)
	if err != nil {
		return err
	}
	return lease.Replace(previous, next)
}

func (authority *Authority) CreateOperation(
	ctx context.Context,
	lookup ActiveLookup,
	artifact receivecontract.ArtifactSpec,
) (_ *Operation, resultErr error) {
	defer func() {
		resultErr = DiagnoseFailure(FailureCreate, resultErr)
	}()
	if authority == nil || ctx == nil || artifact.IsZero() {
		return nil, transfer.ErrInvalidOutputBinding
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	authority.mu.Lock()
	defer authority.mu.Unlock()
	if authority.closed || authority.stage != authorityStageLookupHeld ||
		lookup.authority != authority || lookup.kind != LookupCreateReady ||
		lookup.admission == nil || lookup.admission != authority.admission ||
		!validNamedArtifact(artifact) {
		return nil, transfer.ErrInvalidOutputBinding
	}
	operationID := lookup.admission.operationID
	if operationID.IsZero() {
		var err error
		operationID, err = newOperationID(authority.random)
		if err != nil {
			return nil, err
		}
	}
	reservationID, err := newReservationID(authority.random)
	if err != nil {
		return nil, err
	}
	admission := authority.admission
	if err := admission.prepare(operationID); err != nil {
		closeErr := admission.close()
		authority.admission = nil
		authority.stage = authorityStageBound
		return nil, errors.Join(checkpointRuntimeError(ctx, "prepare ordinary admission candidate", err), closeErr)
	}
	topLevel, err := authority.destination.ReserveTopLevel(destinationauthority.ReservationRequest{
		OperationID: operationID, ReservationID: reservationID, Artifact: artifact, Metadata: admission,
	})
	if err != nil {
		return nil, authority.failPreparedAdmissionLocked(ctx, err)
	}
	reservation := topLevel.CanonicalReservation()
	plan, planErr := receivecontract.NewDirectTreePlan(artifact, reservation)
	intent, intentErr := transfer.NewReceiveIntent(admission.selection, artifact, plan)
	if planErr != nil || intentErr != nil {
		return nil, authority.failReservedAdmissionLocked(ctx, topLevel, errors.Join(planErr, intentErr))
	}
	claim := topLevel.MetadataClaim()
	if admission.durable == nil {
		return nil, authority.failReservedAdmissionLocked(ctx, topLevel, transfer.ErrInvalidOutputBinding)
	}
	locator, locatorErr := checkpointmodel.NewReservationClaimLocator(
		[sha256.Size]byte(claim.Token), claim.Generation+1,
	)
	record, recordErr := checkpointmodel.NewOrdinaryOperationRecord(checkpointmodel.OrdinaryOperationRecordSpec{
		ActiveKey: admission.key, Intent: intent, ReservationClaim: locator,
		LifecycleGeneration: 1, Lifecycle: checkpointmodel.OrdinaryOperationActive,
		Lease: checkpointmodel.OrdinaryLeaseHeld, ClosedReason: checkpointmodel.OrdinaryReasonNone,
	})
	if locatorErr != nil || recordErr != nil {
		return nil, authority.failCommittedAdmissionLocked(ctx, topLevel, errors.Join(locatorErr, recordErr))
	}
	lease, err := admission.durable.Create(record, claim)
	if err != nil {
		return nil, authority.failCommittedAdmissionLocked(ctx, topLevel, err)
	}
	operationClaim := destinationauthority.ReservationClaim{
		Token: claim.Token, Generation: locator.Generation(),
	}
	operation := &Operation{
		authority: authority, key: admission.key, intent: intent,
		topLevel: topLevel, claim: operationClaim, lease: lease,
	}
	if err := operation.validate(authority); err != nil {
		closeErr := operation.close()
		authority.admission = nil
		authority.stage = authorityStageLookupStopped
		return nil, errors.Join(err, closeErr)
	}
	authority.admission, authority.operation = nil, operation
	authority.stage = authorityStageOperationReady
	return operation, nil
}

func (authority *Authority) failPreparedAdmissionLocked(ctx context.Context, cause error) error {
	admission := authority.admission
	if admission == nil {
		return cause
	}
	var stateErr error
	if admission.durable != nil {
		if errors.Is(cause, destinationauthority.ErrReservationIndeterminate) {
			stateErr = admission.durable.RequireAttention()
		} else {
			stateErr = admission.durable.RollbackCandidate()
		}
	}
	closeErr := admission.close()
	authority.admission = nil
	authority.stage = authorityStageBound
	code := transferfault.OutputOwnership
	// A rejected contract does not imply lost ownership when reservation and
	// cleanup both have known outcomes. Uncertain mutations still need attention.
	if errors.Is(cause, destinationauthority.ErrInvalidReservation) &&
		!errors.Is(cause, destinationauthority.ErrReservationIndeterminate) && stateErr == nil && closeErr == nil {
		code = transferfault.OutputContract
	}
	return errors.Join(runtimeOutputError(ctx, code, "reserve top-level output", cause), stateErr, closeErr)
}

func (authority *Authority) failCommittedAdmissionLocked(
	ctx context.Context,
	topLevel *destinationauthority.TopLevelReservation,
	cause error,
) error {
	admission := authority.admission
	var stateErr, closeErr error
	if admission != nil && admission.durable != nil {
		// Create may fail after operation/claim installation or active publication.
		// Preserve candidate evidence when it still exists; an already-consumed
		// admission rejects this transition and the original error remains primary.
		stateErr = admission.durable.RequireAttention()
	}
	if topLevel != nil {
		closeErr = topLevel.Close()
	}
	if admission != nil {
		closeErr = errors.Join(closeErr, admission.close())
	}
	authority.admission = nil
	authority.stage = authorityStageBound
	return errors.Join(runtimeOutputError(ctx, transferfault.OutputOwnership, "publish ordinary operation", cause), stateErr, closeErr)
}

func (authority *Authority) failReservedAdmissionLocked(
	ctx context.Context,
	topLevel *destinationauthority.TopLevelReservation,
	cause error,
) error {
	admission := authority.admission
	var stateErr, closeErr error
	if admission != nil && admission.durable != nil {
		stateErr = admission.durable.RequireAttention()
	}
	if topLevel != nil {
		closeErr = topLevel.Close()
	}
	if admission != nil {
		closeErr = errors.Join(closeErr, admission.close())
	}
	authority.admission = nil
	authority.stage = authorityStageBound
	return errors.Join(runtimeOutputError(ctx, transferfault.OutputOwnership, "freeze ordinary operation", cause), stateErr, closeErr)
}

func (authority *Authority) OpenOperation(
	ctx context.Context,
	operation *Operation,
) (_ transfer.DirectTreeSession, resultErr error) {
	defer func() {
		if resultErr != nil && !errors.Is(resultErr, ErrShareStopped) {
			resultErr = DiagnoseFailure(FailureOpen, resultErr)
		}
	}()
	if authority == nil || ctx == nil || operation == nil {
		return nil, transfer.ErrInvalidOutputBinding
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	authority.mu.Lock()
	if authority.closed || authority.stage != authorityStageOperationReady ||
		authority.operation != operation || operation.opened {
		authority.mu.Unlock()
		return nil, transfer.ErrInvalidOutputBinding
	}
	if operation.lease != nil && operation.lease.Record().Lifecycle() == checkpointmodel.OrdinaryOperationShareStopped {
		authority.mu.Unlock()
		return nil, ErrShareStopped
	}
	if err := operation.validate(authority); err != nil {
		var stateErr error
		if operation.lease != nil {
			stateErr = requireOperationAttention(
				operation.lease, checkpointmodel.OrdinaryReasonOperationOwnershipUnknown,
			)
		}
		closeErr := operation.close()
		authority.operation = nil
		authority.stage = authorityStageLookupStopped
		authority.mu.Unlock()
		return nil, errors.Join(
			runtimeOutputError(ctx, transferfault.OutputOwnership, "validate frozen output operation", err),
			checkpointRuntimeError(ctx, "persist invalid frozen operation attention", stateErr),
			closeErr,
		)
	}
	if err := validateNamedOperationIntent(operation.intent, operation.topLevel.ReservedEntry()); err != nil {
		authority.mu.Unlock()
		return nil, runtimeOutputError(
			ctx, transferfault.OutputOwnership, "validate named output coordinates", err,
		)
	}
	operation.opened = true
	authority.mu.Unlock()
	authority.trace(Trace{
		Operation:           TraceRuntimeDecision,
		ReceiveIntentDigest: operation.intent.Digest(),
		ReceiveOperationID:  operation.intent.OperationID(),
		RuntimeComponent:    RuntimeSession,
		RuntimeOperation:    RuntimeAdmitDestination,
		RuntimeDecision:     RuntimeAdmitted,
	})

	session, err := authority.openNamedOperation(ctx, operation)
	if err != nil {
		authority.mu.Lock()
		if authority.operation == operation && !operation.closed {
			operation.opened = false
		}
		authority.mu.Unlock()
		return nil, err
	}
	return session, nil
}

func (operation *Operation) validate(authority *Authority) error {
	if operation == nil || operation.closed || authority == nil || operation.authority != authority ||
		operation.key.IsZero() || operation.intent.IsZero() || operation.topLevel == nil ||
		!operation.claim.Valid() || operation.lease == nil {
		return transfer.ErrInvalidOutputBinding
	}
	reservation, direct := operation.intent.MaterializationPlan().DestinationReservation()
	topClaim := operation.topLevel.MetadataClaim()
	if !direct || !validNamedReservation(reservation, operation.intent.ArtifactSpec(), authority.binding) ||
		reservation.Digest() != operation.topLevel.CanonicalReservation().Digest() ||
		!topClaim.Valid() || topClaim.Token != operation.claim.Token {
		return transfer.ErrInvalidOutputBinding
	}
	key, err := checkpointmodel.NewActiveOperationKeyV1(
		operation.intent.SelectionSpec().Digest(), authority.binding.AuthorityRef(),
	)
	if err != nil || key != operation.key && checkpointmodel.IsolatedOperationKey(operation.intent.OperationID()) != operation.key {
		return errors.Join(transfer.ErrInvalidOutputBinding, err)
	}
	// A new reservation retains its reservation-bound claim while the registry
	// advances the durable proof once; an exact reopen already carries that
	// operation-bound generation. No caller may supply any other relationship.
	if topClaim.Generation != operation.claim.Generation &&
		topClaim.Generation+1 != operation.claim.Generation {
		return transfer.ErrInvalidOutputBinding
	}
	record := operation.lease.Record()
	if !record.Valid() ||
		record.ActiveOperationKey() != operation.key ||
		record.ReceiveIntentDigest() != operation.intent.Digest() ||
		record.ReservationClaim().Token() != [sha256.Size]byte(operation.claim.Token) ||
		record.ReservationClaim().Generation() != operation.claim.Generation {
		return transfer.ErrInvalidOutputBinding
	}
	return nil
}

func validNamedArtifact(artifact receivecontract.ArtifactSpec) bool {
	layout, ok := artifact.DirectoryTree()
	if !ok {
		return false
	}
	return layout.Kind() == receivecontract.DirectoryTreeSingleFile ||
		layout.Kind() == receivecontract.DirectoryTreeResultRoot
}

func validNamedReservation(
	reservation receivecontract.DestinationReservation,
	artifact receivecontract.ArtifactSpec,
	binding destinationauthority.Binding,
) bool {
	return !reservation.IsZero() && validNamedArtifact(artifact) && binding.Valid() &&
		reservation.Kind() == receivecontract.ReservationNamedContainerEntry &&
		reservation.AuthorityKind() == receivecontract.AuthorityNativeContainer &&
		reservation.AuthorityRef() == binding.AuthorityRef() &&
		reservation.ArtifactDigest() == artifact.Digest()
}

func sameSelection(left, right transfer.SelectionSpec) bool {
	return !left.IsZero() && !right.IsZero() &&
		bytes.Equal(left.CanonicalBytes(), right.CanonicalBytes())
}
