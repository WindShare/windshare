package outputruntime

import (
	"bytes"
	"context"
	"crypto/sha256"
	"errors"

	"github.com/windshare/windshare/core/osfs/internal/checkpointmodel"
	"github.com/windshare/windshare/core/osfs/internal/checkpointstore"
	"github.com/windshare/windshare/core/osfs/internal/destinationauthority"
	"github.com/windshare/windshare/core/osfs/internal/outputcap"
	"github.com/windshare/windshare/core/transfer"
	transferfault "github.com/windshare/windshare/core/transfer/fault"
	"github.com/windshare/windshare/core/transfer/receivecontract"
)

type Operation struct {
	authority *Authority
	key       checkpointmodel.ActiveOperationKey
	mode      outputcap.ExecutionMode
	intent    transfer.ReceiveIntent
	topLevel  *destinationauthority.TopLevelReservation
	claim     destinationauthority.ReservationClaim
	lease     *checkpointstore.OperationRegistryLease
	volatile  *volatileReservationClaimer
	reopened  bool
	opened    bool
	closed    bool
}

func (operation *Operation) ReceiveIntent() (transfer.ReceiveIntent, bool) {
	if operation == nil || operation.closed || operation.intent.IsZero() {
		return transfer.ReceiveIntent{}, false
	}
	return operation.intent, true
}

func (operation *Operation) ExecutionMode() ExecutionMode {
	if operation == nil || operation.closed {
		return ExecutionMode{}
	}
	return ExecutionMode{mode: operation.mode}
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
		err = operation.topLevel.Close()
		operation.topLevel = nil
	}
	if operation.lease != nil {
		err = errors.Join(err, operation.lease.Close())
		operation.lease = nil
	}
	if operation.volatile != nil {
		err = errors.Join(err, operation.volatile.Close())
		operation.volatile = nil
	}
	return err
}

func (authority *Authority) LookupActive(
	ctx context.Context,
	selection transfer.SelectionSpec,
) (ActiveLookup, error) {
	if authority == nil || ctx == nil || selection.IsZero() {
		return ActiveLookup{}, transfer.ErrInvalidOutputBinding
	}
	if err := ctx.Err(); err != nil {
		return ActiveLookup{}, err
	}
	authority.mu.Lock()
	defer authority.mu.Unlock()
	if authority.closed || authority.stage != authorityStageBound ||
		authority.destination == nil || !authority.binding.Valid() || !authority.mode.Valid() {
		return ActiveLookup{}, transfer.ErrInvalidOutputBinding
	}
	key, err := checkpointmodel.NewActiveOperationKeyV1(selection.Digest(), authority.binding.AuthorityRef())
	if err != nil {
		return ActiveLookup{}, err
	}
	if authority.mode == outputcap.ExecutionLiveOnly {
		claimer, err := newVolatileReservationClaimer(authority.binding.AuthorityRef())
		if err != nil {
			return ActiveLookup{}, err
		}
		admission := &heldAdmission{key: key, selection: selection, volatile: claimer}
		authority.admission, authority.stage = admission, authorityStageLookupHeld
		return ActiveLookup{authority: authority, kind: ActiveLookupMiss, admission: admission}, nil
	}
	if authority.mode != outputcap.ExecutionResumable || authority.registry == nil {
		return ActiveLookup{}, transfer.ErrInvalidOutputBinding
	}
	activeAdmission, registryLookup, err := authority.registry.BeginActive(key)
	if err != nil {
		return ActiveLookup{}, checkpointRuntimeError(ctx, "begin active ordinary operation", err)
	}
	switch registryLookup.State() {
	case checkpointstore.ActiveLookupNone:
		admission := &heldAdmission{
			key: key, selection: selection, durable: &activeAdmission,
		}
		authority.admission, authority.stage = admission, authorityStageLookupHeld
		return ActiveLookup{authority: authority, kind: ActiveLookupMiss, admission: admission}, nil
	case checkpointstore.ActiveLookupReopenable:
		operation, attention, err := authority.reopenOperationLocked(ctx, selection, key, &registryLookup)
		if err != nil {
			return ActiveLookup{}, err
		}
		if attention {
			authority.stage = authorityStageLookupStopped
			return ActiveLookup{authority: authority, kind: ActiveLookupNeedsAttention}, nil
		}
		authority.operation, authority.stage = operation, authorityStageOperationReady
		return ActiveLookup{authority: authority, kind: ActiveLookupReopened, operation: operation}, nil
	case checkpointstore.ActiveLookupAlreadyRunning:
		authority.stage = authorityStageLookupStopped
		return ActiveLookup{authority: authority, kind: ActiveLookupAlreadyRunning}, nil
	case checkpointstore.ActiveLookupNeedsAttention:
		authority.stage = authorityStageLookupStopped
		return ActiveLookup{authority: authority, kind: ActiveLookupNeedsAttention}, nil
	case checkpointstore.ActiveLookupAmbiguous:
		authority.stage = authorityStageLookupStopped
		return ActiveLookup{authority: authority, kind: ActiveLookupAmbiguous}, nil
	default:
		return ActiveLookup{}, transfer.ErrInvalidOutputBinding
	}
}

func (authority *Authority) reopenOperationLocked(
	ctx context.Context,
	selection transfer.SelectionSpec,
	key checkpointmodel.ActiveOperationKey,
	lookup *checkpointstore.ActiveLookup,
) (*Operation, bool, error) {
	lease := lookup.TakeLease()
	if lease == nil {
		return nil, false, transfer.ErrInvalidOutputBinding
	}
	record := lookup.Record()
	intent, err := record.VerifyIntent(transfer.DecodeReceiveIntent)
	proof := lookup.RecoveryProof()
	var reservation receivecontract.DestinationReservation
	var direct bool
	if err == nil {
		reservation, direct = intent.MaterializationPlan().DestinationReservation()
	}
	if err != nil || !direct || !sameSelection(intent.SelectionSpec(), selection) ||
		!validNamedReservation(reservation, intent.ArtifactSpec(), authority.binding) ||
		!proof.Valid() || record.ReservationClaim().Token() != [sha256.Size]byte(proof.Claim().Token) ||
		record.ReservationClaim().Generation() != proof.Claim().Generation {
		attentionErr := requireOperationAttention(lease, checkpointmodel.OrdinaryReasonOperationOwnershipUnknown)
		closeErr := lease.Close()
		if attentionErr != nil || closeErr != nil {
			return nil, false, checkpointRuntimeError(ctx, "quarantine invalid ordinary operation", errors.Join(err, attentionErr, closeErr))
		}
		return nil, true, nil
	}
	topLevel, err := authority.destination.ReopenTopLevel(destinationauthority.ExpectedReservation{
		Reservation: reservation, PersistentIdentityClaim: proof.PersistentIdentity(),
		MetadataClaim: proof.Claim(),
	})
	if err != nil {
		attentionErr := requireOperationAttention(lease, checkpointmodel.OrdinaryReasonDestinationOwnershipUnknown)
		closeErr := lease.Close()
		if attentionErr != nil || closeErr != nil {
			return nil, false, errors.Join(
				runtimeOutputError(ctx, transferfault.OutputOwnership, "reopen exact top-level reservation", err),
				checkpointRuntimeError(ctx, "persist reopen attention", errors.Join(attentionErr, closeErr)),
			)
		}
		return nil, true, nil
	}
	operation := &Operation{
		authority: authority, key: key, mode: authority.mode, intent: intent,
		topLevel: topLevel, claim: proof.Claim(), lease: lease, reopened: true,
	}
	if err := operation.validate(authority); err != nil {
		return nil, false, errors.Join(err, topLevel.Close(), lease.Close())
	}
	return operation, false, nil
}

func requireOperationAttention(
	lease *checkpointstore.OperationRegistryLease,
	reason checkpointmodel.OrdinaryClosedReason,
) error {
	if lease == nil {
		return transfer.ErrInvalidOutputBinding
	}
	previous := lease.Record()
	lifecycle, closedReason, err := checkpointmodel.ReduceOrdinaryOperationLifecycle(
		previous.Lifecycle(), checkpointmodel.OrdinaryLifecycleRequireAttention, reason,
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
) (*Operation, error) {
	if authority == nil || ctx == nil || artifact.IsZero() {
		return nil, transfer.ErrInvalidOutputBinding
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	authority.mu.Lock()
	defer authority.mu.Unlock()
	if authority.closed || authority.stage != authorityStageLookupHeld ||
		lookup.authority != authority || lookup.kind != ActiveLookupMiss ||
		lookup.admission == nil || lookup.admission != authority.admission ||
		!validNamedArtifact(artifact) {
		return nil, transfer.ErrInvalidOutputBinding
	}
	operationID, err := newOperationID(authority.random)
	if err != nil {
		return nil, err
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
		err := authority.failReservedAdmissionLocked(ctx, topLevel, errors.Join(planErr, intentErr))
		return nil, err
	}
	claim := topLevel.MetadataClaim()
	operationClaim := claim
	var lease *checkpointstore.OperationRegistryLease
	var volatile *volatileReservationClaimer
	switch authority.mode {
	case outputcap.ExecutionResumable:
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
			return nil, authority.failCommittedAdmissionLocked(
				ctx, topLevel, errors.Join(locatorErr, recordErr),
			)
		}
		lease, err = admission.durable.Create(record, claim)
		if err != nil {
			return nil, authority.failCommittedAdmissionLocked(ctx, topLevel, err)
		}
		operationClaim = destinationauthority.ReservationClaim{
			Token: claim.Token, Generation: locator.Generation(),
		}
	case outputcap.ExecutionLiveOnly:
		volatile = admission.volatile
		admission.volatile = nil
	default:
		return nil, authority.failReservedAdmissionLocked(ctx, topLevel, transfer.ErrInvalidOutputBinding)
	}
	operation := &Operation{
		authority: authority, key: admission.key, mode: authority.mode, intent: intent,
		topLevel: topLevel, claim: operationClaim, lease: lease, volatile: volatile,
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
	return errors.Join(runtimeOutputError(ctx, transferfault.OutputOwnership, "reserve top-level output", cause), stateErr, closeErr)
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
	// Live-only has no durable candidate and can release its volatile claim while
	// its result-root capability is still exact. A resumable reservation is never
	// cleanly rolled back here because name binding already crossed that proof cut.
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
) (transfer.DirectTreeSession, error) {
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
	authority.trace(FilesystemOutputTrace{
		Operation:           TraceRuntimeDecision,
		ReceiveIntentDigest: operation.intent.Digest(),
		ReceiveOperationID:  operation.intent.OperationID(),
		RuntimeComponent:    FilesystemOutputRuntimeSession,
		RuntimeOperation:    FilesystemOutputRuntimeAdmitDestination,
		RuntimeDecision:     FilesystemOutputRuntimeAdmitted,
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
		operation.key.IsZero() || !operation.mode.Valid() || operation.mode != authority.mode ||
		operation.intent.IsZero() || operation.topLevel == nil || !operation.claim.Valid() {
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
	if err != nil || key != operation.key {
		return errors.Join(transfer.ErrInvalidOutputBinding, err)
	}
	switch operation.mode {
	case outputcap.ExecutionResumable:
		// A new reservation retains its reservation-bound claim while the registry
		// advances the durable proof once; an exact reopen already carries that
		// operation-bound generation. No caller may supply any other relationship.
		if topClaim.Generation != operation.claim.Generation &&
			topClaim.Generation+1 != operation.claim.Generation {
			return transfer.ErrInvalidOutputBinding
		}
		if operation.lease == nil {
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
	case outputcap.ExecutionLiveOnly:
		if topClaim != operation.claim ||
			operation.volatile == nil || operation.volatile.closed || operation.lease != nil {
			return transfer.ErrInvalidOutputBinding
		}
	default:
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
