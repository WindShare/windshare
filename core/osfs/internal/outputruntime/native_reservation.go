package outputruntime

import (
	"bytes"
	"context"
	"errors"
	"io"

	"github.com/windshare/windshare/core/osfs/internal/checkpointstore"
	"github.com/windshare/windshare/core/osfs/internal/destinationauthority"
	"github.com/windshare/windshare/core/osfs/internal/outputcap"
	"github.com/windshare/windshare/core/transfer"
	transferfault "github.com/windshare/windshare/core/transfer/fault"
	"github.com/windshare/windshare/core/transfer/receivecontract"
)

const maximumStableIdentityGenerationAttempts = 8

type ExecutionMode struct {
	mode outputcap.ExecutionMode
}

func (mode ExecutionMode) Resumable() bool { return mode.mode == outputcap.ExecutionResumable }
func (mode ExecutionMode) LiveOnly() bool  { return mode.mode == outputcap.ExecutionLiveOnly }
func (mode ExecutionMode) Valid() bool {
	return mode.mode == outputcap.ExecutionResumable || mode.mode == outputcap.ExecutionLiveOnly
}

func (authority *Authority) BindDestination(ctx context.Context) (ExecutionMode, error) {
	if authority == nil || ctx == nil || authority.platformFactory == nil {
		return ExecutionMode{}, transfer.ErrInvalidOutputBinding
	}
	if err := ctx.Err(); err != nil {
		return ExecutionMode{}, err
	}
	authority.mu.Lock()
	defer authority.mu.Unlock()
	if authority.closed || authority.stage != authorityStageConstructed {
		return ExecutionMode{}, transfer.ErrInvalidOutputBinding
	}
	platform, err := authority.platformFactory(authority.rootPath, authority.createRoot)
	if err != nil {
		return ExecutionMode{}, runtimeOutputError(ctx, transferfault.OutputOwnership, "open destination platform", err)
	}
	destination, err := destinationauthority.BindDestination(destinationauthority.BindConfig{
		Platform: platform, DisplayPath: authority.rootPath,
		OpenLiveCleanupJournal: openNativeLiveCleanupJournal,
	})
	if err != nil {
		return ExecutionMode{}, runtimeOutputError(ctx, transferfault.OutputOwnership, "bind destination authority", err)
	}
	binding := destination.Binding()
	mode, err := binding.ExecutionMode()
	if err != nil {
		return ExecutionMode{}, errors.Join(
			runtimeOutputError(ctx, transferfault.OutputOwnership, "select destination execution mode", err),
			destination.Close(),
		)
	}
	var registry *checkpointstore.OperationRegistry
	if mode == outputcap.ExecutionResumable {
		err = destination.OpenResumableState(func(control outputcap.Directory) (io.Closer, error) {
			opened, openErr := checkpointstore.OpenOperationRegistry(control)
			if openErr != nil {
				return nil, openErr
			}
			registry = &opened
			return registry, nil
		})
		if err != nil {
			return ExecutionMode{}, errors.Join(
				checkpointRuntimeError(ctx, "open ordinary operation registry", err),
				destination.Close(),
			)
		}
	}
	authority.destination = destination
	authority.binding = binding
	authority.mode = mode
	authority.registry = registry
	authority.stage = authorityStageBound
	return ExecutionMode{mode: mode}, nil
}

func (authority *Authority) Close() error {
	if authority == nil {
		return nil
	}
	authority.mu.Lock()
	if authority.closed {
		authority.mu.Unlock()
		return nil
	}
	authority.closed = true
	operation := authority.operation
	admission := authority.admission
	destination := authority.destination
	authority.operation = nil
	authority.admission = nil
	authority.destination = nil
	authority.registry = nil
	authority.binding = destinationauthority.Binding{}
	authority.mode = 0
	authority.stage = authorityStageLookupStopped
	authority.mu.Unlock()

	var err error
	if operation != nil {
		err = errors.Join(err, operation.close())
	}
	if admission != nil {
		err = errors.Join(err, admission.close())
	}
	if destination != nil {
		err = errors.Join(err, destination.Close())
	}
	return err
}

// reserveNativeDirectTree is the temporary public-facade adapter used until the
// caller supplies its bounded output shape directly. It deliberately lowers the
// old unbounded catalog-root request before persistence so every actual operation
// is owned by the same named ordinary lifecycle.
func (authority *Authority) reserveNativeDirectTree(
	ctx context.Context,
	selection transfer.SelectionSpec,
	artifact receivecontract.ArtifactSpec,
) (NativeDirectTreeReservation, error) {
	if authority == nil || ctx == nil || selection.IsZero() || artifact.IsZero() {
		return NativeDirectTreeReservation{}, transfer.ErrInvalidReceiveIntent
	}
	layout, tree := artifact.DirectoryTree()
	if !tree || layout.Kind() != receivecontract.DirectoryTreeCatalogRoot {
		return NativeDirectTreeReservation{}, transfer.ErrInvalidOutputBinding
	}
	if _, err := authority.BindDestination(ctx); err != nil {
		return NativeDirectTreeReservation{}, err
	}
	lookup, err := authority.LookupActive(ctx, selection)
	if err != nil {
		return NativeDirectTreeReservation{}, err
	}
	var operation *Operation
	kind := NativeDirectTreeReopened
	switch lookup.Kind() {
	case ActiveLookupMiss:
		bounded, err := receivecontract.NewResultRootDirectoryTree(
			receivecontract.NewSyntheticSelectionResultRoot(),
		)
		if err != nil {
			return NativeDirectTreeReservation{}, err
		}
		operation, err = authority.CreateOperation(ctx, lookup, bounded)
		if err != nil {
			return NativeDirectTreeReservation{}, err
		}
		kind = NativeDirectTreeReserved
	case ActiveLookupReopened:
		operation = lookup.Operation()
	case ActiveLookupAlreadyRunning, ActiveLookupNeedsAttention, ActiveLookupAmbiguous:
		return NativeDirectTreeReservation{kind: NativeDirectTreeNeedsAttention}, nil
	default:
		return NativeDirectTreeReservation{}, transfer.ErrInvalidOutputBinding
	}
	intent, ok := operation.ReceiveIntent()
	if !ok {
		return NativeDirectTreeReservation{}, transfer.ErrInvalidOutputBinding
	}
	return NativeDirectTreeReservation{kind: kind, intent: intent}, nil
}

// openNativeOutput accepts only the exact frozen intent returned by the facade
// reservation. The retained Operation is the sole capability path into the
// ordinary output session; no legacy checkpoint namespace is reopened here.
func (authority *Authority) openNativeOutput(
	ctx context.Context,
	intent transfer.ReceiveIntent,
) (transfer.DirectTreeSession, error) {
	if authority == nil || ctx == nil || intent.IsZero() {
		return nil, transfer.ErrInvalidOutputBinding
	}
	authority.mu.Lock()
	operation := authority.operation
	authority.mu.Unlock()
	frozen, ok := operation.ReceiveIntent()
	if !ok || !bytes.Equal(frozen.CanonicalBytes(), intent.CanonicalBytes()) {
		return nil, transfer.ErrInvalidOutputBinding
	}
	return authority.OpenOperation(ctx, operation)
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
