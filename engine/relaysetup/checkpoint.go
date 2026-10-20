package relaysetup

import (
	"context"
)

func ownsPending(current Snapshot, pending *PendingSetup) error {
	if current.Pending == nil || current.Pending.OperationID != pending.OperationID {
		return ErrSetupSuperseded
	}
	return nil
}

func (service *Service) currentPending(pending *PendingSetup) (Snapshot, error) {
	current, err := service.dependencies.Store.Load()
	if err != nil {
		return Snapshot{}, err
	}
	if err := ownsPending(current, pending); err != nil {
		return Snapshot{}, err
	}
	if current.Pending.phase.stage() != pending.phase.stage() {
		return Snapshot{}, ErrSetupSuperseded
	}
	return current, nil
}

func (service *Service) checkpoint(ctx context.Context, pending *PendingSetup, change func(*PendingSetup) setupPhase) (Snapshot, error) {
	return service.dependencies.Store.Update(ctx, func(current *Snapshot) error {
		if err := ownsPending(*current, pending); err != nil {
			return err
		}
		if current.Pending.phase.stage() != pending.phase.stage() {
			return ErrSetupSuperseded
		}
		current.Pending = current.Pending.withPhase(change(current.Pending))
		return nil
	})
}

func (service *Service) clearPending(ctx context.Context, pending *PendingSetup) error {
	_, err := service.dependencies.Store.Update(ctx, func(current *Snapshot) error {
		if err := ownsPending(*current, pending); err != nil {
			return err
		}
		if current.Pending.phase.stage() != pending.phase.stage() {
			return ErrSetupSuperseded
		}
		current.Pending = nil
		return nil
	})
	return err
}
