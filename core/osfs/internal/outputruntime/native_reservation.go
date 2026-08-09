package outputruntime

import (
	"bytes"
	"context"
	"errors"
	"io"

	"github.com/windshare/windshare/core/osfs/internal/checkpointmodel"
	"github.com/windshare/windshare/core/osfs/internal/checkpointstore"
	"github.com/windshare/windshare/core/osfs/internal/outputcap"
	"github.com/windshare/windshare/core/transfer"
	transferfault "github.com/windshare/windshare/core/transfer/fault"
	"github.com/windshare/windshare/core/transfer/receivecontract"
)

const maximumStableIdentityGenerationAttempts = 8

func (authority *Authority) reserveNativeDirectTree(
	ctx context.Context,
	selection transfer.SelectionSpec,
	artifact receivecontract.ArtifactSpec,
) (_ NativeDirectTreeReservation, resultErr error) {
	if authority == nil || ctx == nil || selection.IsZero() || artifact.IsZero() ||
		authority.platformFactory == nil || authority.random == nil {
		return NativeDirectTreeReservation{}, transfer.ErrInvalidReceiveIntent
	}
	layout, tree := artifact.DirectoryTree()
	if !tree || layout.Kind() != receivecontract.DirectoryTreeCatalogRoot {
		return NativeDirectTreeReservation{}, transfer.ErrInvalidOutputBinding
	}
	if err := ctx.Err(); err != nil {
		return NativeDirectTreeReservation{}, err
	}
	resources := &nativeOutputResources{authority: authority}
	defer func() {
		resultErr = errors.Join(resultErr, resources.ReleaseOutputSession(context.Background()))
	}()
	platform, err := authority.acquireCertifiedNativePlatform(ctx, transfer.ReceiveIntentDigest{}, resources)
	if err != nil {
		return NativeDirectTreeReservation{}, err
	}
	guard, err := platform.platform.AcquirePublicOperationGuard()
	if err != nil {
		return NativeDirectTreeReservation{}, runtimeOutputError(ctx, transferfault.OutputOwnership, "acquire native reservation guard", err)
	}
	defer func() { resultErr = errors.Join(resultErr, guard.Close()) }()
	if err := validateReservationGuard(platform.platform, guard); err != nil {
		return NativeDirectTreeReservation{}, runtimeOutputError(ctx, transferfault.OutputOwnership, "validate native reservation guard", err)
	}
	namespace, _, err := initializeNativeCheckpointNamespace(platform.platform, platform.authorityRef)
	if err != nil {
		if checkpointOwnershipUncertain(err) {
			return NativeDirectTreeReservation{kind: NativeDirectTreeNeedsAttention}, nil
		}
		return NativeDirectTreeReservation{}, checkpointRuntimeError(ctx, "initialize checkpoint namespace", err)
	}
	resources.namespace = &namespace
	compatible, err := checkpointmodel.NewCLICompatibleOperationKey(selection, artifact, platform.authorityRef)
	if err != nil {
		return NativeDirectTreeReservation{}, err
	}
	lookup, err := namespace.LookupCompatible(compatible)
	if err != nil {
		return NativeDirectTreeReservation{}, checkpointRuntimeError(ctx, "lookup compatible native operation", err)
	}
	operations := lookup.Operations()
	if lookup.OwnershipUncertain() || len(operations) > 1 {
		return NativeDirectTreeReservation{kind: NativeDirectTreeNeedsAttention}, nil
	}
	if len(operations) == 1 {
		intent, err := operations[0].VerifyIntent(transfer.DecodeReceiveIntent)
		if err != nil || !sameSelectionAndArtifact(intent, selection, artifact) ||
			validateIntentForCertifiedRoot(intent, platform.authorityRef) != nil {
			return NativeDirectTreeReservation{kind: NativeDirectTreeNeedsAttention}, nil
		}
		return NativeDirectTreeReservation{kind: NativeDirectTreeReopened, intent: intent}, nil
	}
	intent, record, err := authority.newNativeDirectTreeIntent(selection, artifact, compatible, platform.authorityRef)
	if err != nil {
		return NativeDirectTreeReservation{}, err
	}
	lease, err := namespace.AcquireOperation(intent.OperationID(), intent.Digest(), intent.BindingDigest())
	if err != nil {
		return NativeDirectTreeReservation{}, checkpointRuntimeError(ctx, "acquire new native operation", err)
	}
	resources.lease = &lease
	repository, err := lease.OpenOrCreateRepository()
	if err != nil {
		return NativeDirectTreeReservation{}, checkpointRuntimeError(ctx, "create native operation repository", err)
	}
	resources.repository = &repository
	reservation, _ := intent.MaterializationPlan().DestinationReservation()
	if err := repository.InstallOperation(record, reservation.CanonicalBytes()); err != nil {
		return NativeDirectTreeReservation{}, checkpointRuntimeError(ctx, "install native operation", err)
	}
	frozen, err := checkpointmodel.NewReceiveLifecycleState(checkpointmodel.LifecycleStateSpec{
		OperationID: intent.OperationID(), ReceiveIntent: intent.Digest(),
		StateGeneration: 1, Phase: checkpointmodel.LifecycleIntentFrozen,
	})
	if err != nil {
		return NativeDirectTreeReservation{}, err
	}
	if err := repository.CreateLifecycleState(frozen); err != nil {
		return NativeDirectTreeReservation{}, checkpointRuntimeError(ctx, "create native operation lifecycle", err)
	}
	if err := lease.RegisterLookup(record); err != nil {
		return NativeDirectTreeReservation{}, checkpointRuntimeError(ctx, "register compatible native operation", err)
	}
	return NativeDirectTreeReservation{kind: NativeDirectTreeReserved, intent: intent}, nil
}

func checkpointOwnershipUncertain(err error) bool {
	var checkpointErr *checkpointstore.Error
	return errors.As(err, &checkpointErr) && checkpointErr.Code() == checkpointstore.ErrorOwnershipMismatch
}

func validateReservationGuard(platform outputcap.Platform, guard outputcap.PublicOperationGuard) error {
	if platform == nil || platform.Root() == nil || guard == nil || guard.Root() == nil {
		return outputcap.ErrUnsafeNamespace
	}
	same, err := platform.Root().SameDirectory(guard.Root())
	if err != nil || !same {
		return errors.Join(outputcap.ErrUnsafeNamespace, err)
	}
	return validateOutputCreateAuthority(guard.Root())
}

func sameSelectionAndArtifact(
	intent transfer.ReceiveIntent,
	selection transfer.SelectionSpec,
	artifact receivecontract.ArtifactSpec,
) bool {
	return !intent.IsZero() && bytes.Equal(intent.SelectionSpec().CanonicalBytes(), selection.CanonicalBytes()) &&
		bytes.Equal(intent.ArtifactSpec().CanonicalBytes(), artifact.CanonicalBytes())
}

func (authority *Authority) newNativeDirectTreeIntent(
	selection transfer.SelectionSpec,
	artifact receivecontract.ArtifactSpec,
	compatible checkpointmodel.CompatibleOperationKey,
	authorityRef receivecontract.AuthorityRef,
) (transfer.ReceiveIntent, checkpointmodel.ReceiveOperation, error) {
	operation, err := newOperationID(authority.random)
	if err != nil {
		return transfer.ReceiveIntent{}, checkpointmodel.ReceiveOperation{}, err
	}
	reservationID, err := newReservationID(authority.random)
	if err != nil {
		return transfer.ReceiveIntent{}, checkpointmodel.ReceiveOperation{}, err
	}
	reservation, err := receivecontract.NewNativeContainerRootReservation(operation, reservationID, artifact, authorityRef)
	if err != nil {
		return transfer.ReceiveIntent{}, checkpointmodel.ReceiveOperation{}, err
	}
	plan, err := receivecontract.NewDirectTreePlan(artifact, reservation)
	if err != nil {
		return transfer.ReceiveIntent{}, checkpointmodel.ReceiveOperation{}, err
	}
	intent, err := transfer.NewReceiveIntent(selection, artifact, plan)
	if err != nil {
		return transfer.ReceiveIntent{}, checkpointmodel.ReceiveOperation{}, err
	}
	reopen, err := checkpointmodel.CLIReopenKey(compatible)
	if err != nil {
		return transfer.ReceiveIntent{}, checkpointmodel.ReceiveOperation{}, err
	}
	record, err := checkpointmodel.NewReceiveOperation(intent, reopen)
	return intent, record, err
}

func newOperationID(random io.Reader) (receivecontract.OperationID, error) {
	for range maximumStableIdentityGenerationAttempts {
		raw, err := randomStableIdentity(random)
		if err != nil {
			return receivecontract.OperationID{}, err
		}
		if operation, err := receivecontract.OperationIDFromBytes(raw); err == nil {
			return operation, nil
		}
	}
	return receivecontract.OperationID{}, transfer.ErrInvalidOutputBinding
}

func newReservationID(random io.Reader) (receivecontract.DestinationReservationID, error) {
	for range maximumStableIdentityGenerationAttempts {
		raw, err := randomStableIdentity(random)
		if err != nil {
			return receivecontract.DestinationReservationID{}, err
		}
		if reservation, err := receivecontract.DestinationReservationIDFromBytes(raw); err == nil {
			return reservation, nil
		}
	}
	return receivecontract.DestinationReservationID{}, transfer.ErrInvalidOutputBinding
}

func randomStableIdentity(random io.Reader) ([]byte, error) {
	if random == nil {
		return nil, transfer.ErrInvalidOutputBinding
	}
	raw := make([]byte, receivecontract.StableIdentityBytes)
	if _, err := io.ReadFull(random, raw); err != nil {
		return nil, err
	}
	return raw, nil
}
