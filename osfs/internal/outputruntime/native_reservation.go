package outputruntime

import (
	"context"
	"errors"
	"io"

	"github.com/windshare/windshare/core/transfer"
	transferfault "github.com/windshare/windshare/core/transfer/fault"
	"github.com/windshare/windshare/core/transfer/receivecontract"
	"github.com/windshare/windshare/osfs/internal/checkpointstore"
	"github.com/windshare/windshare/osfs/internal/destinationauthority"
	"github.com/windshare/windshare/osfs/internal/outputcap"
)

const maximumStableIdentityGenerationAttempts = 8

func (authority *Authority) BindDestination(ctx context.Context) (resultErr error) {
	defer func() {
		resultErr = DiagnoseFailure(FailureDestinationBinding, resultErr)
	}()
	if authority == nil || ctx == nil || authority.platformFactory == nil {
		return transfer.ErrInvalidOutputBinding
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	authority.mu.Lock()
	defer authority.mu.Unlock()
	if authority.closed || authority.stage != authorityStageConstructed {
		return transfer.ErrInvalidOutputBinding
	}
	platform, err := authority.platformFactory(authority.rootPath, authority.createRoot)
	if err != nil {
		return runtimeOutputError(ctx, transferfault.OutputOwnership, "open destination platform", err)
	}
	destination, err := destinationauthority.BindDestination(destinationauthority.BindConfig{
		Platform:            platform,
		DisplayPath:         authority.rootPath,
		RecyclePrivateState: checkpointstore.RecyclePrivateState,
	})
	if err != nil {
		code := transferfault.OutputOwnership
		if errors.Is(err, outputcap.ErrOrdinaryOutputUnsupported) ||
			errors.Is(err, outputcap.ErrRecoverableOutputUnsupported) {
			code = transferfault.OutputUnsupportedFilesystem
		}
		return runtimeOutputError(ctx, code, "bind destination authority", errors.Join(err, platform.Close()))
	}
	binding := destination.Binding()
	var registry *checkpointstore.OperationRegistry
	err = destination.OpenResumableState(func(control outputcap.Directory) (io.Closer, error) {
		opened, openErr := checkpointstore.OpenOperationRegistry(control)
		if openErr != nil {
			return nil, openErr
		}
		registry = &opened
		return registry, nil
	})
	if err != nil {
		primary := DiagnoseFailure(
			FailureDestinationBinding,
			checkpointRuntimeError(ctx, "open ordinary operation registry", err),
		)
		return freezeFailure(primary, destination.Close())
	}
	authority.destination = destination
	authority.binding = binding
	authority.registry = registry
	authority.stage = authorityStageBound
	authority.trace(Trace{
		Operation:               TraceRuntimeDecision,
		RuntimeComponent:        RuntimeSession,
		RuntimeOperation:        RuntimeAdmitDestination,
		RuntimeDecision:         RuntimeAdmitted,
		DestinationCapabilities: binding.Capabilities(),
		DestinationBindingID:    binding.ID(),
		IdentityScheme:          destination.IdentityScheme(),
	})
	return nil
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
	return DiagnoseFailure(FailureClose, err)
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
