package relaysetup

import (
	"context"

	"github.com/windshare/windshare/connectivity/senderrelay"
	"github.com/windshare/windshare/engine/relayconfig"
)

func ownsPending(current relayconfig.Config, pending *relayconfig.PendingSetup) error {
	if current.Pending == nil || current.Pending.OperationID != pending.OperationID {
		return ErrSetupSuperseded
	}
	return nil
}

func (service *Service) currentPending(pending *relayconfig.PendingSetup) (relayconfig.Config, error) {
	current, err := service.dependencies.Store.Load()
	if err != nil {
		return relayconfig.Config{}, err
	}
	if err := ownsPending(current, pending); err != nil {
		return relayconfig.Config{}, err
	}
	if current.Pending.State != pending.State {
		return relayconfig.Config{}, ErrSetupSuperseded
	}
	return current, nil
}

func (service *Service) checkpoint(ctx context.Context, pending *relayconfig.PendingSetup, change func(*relayconfig.PendingSetup)) (relayconfig.Config, error) {
	return service.dependencies.Store.Update(ctx, func(current *relayconfig.Config) error {
		if err := ownsPending(*current, pending); err != nil {
			return err
		}
		if current.Pending.State != pending.State {
			return ErrSetupSuperseded
		}
		change(current.Pending)
		return nil
	})
}

func (service *Service) clearPending(ctx context.Context, pending *relayconfig.PendingSetup) error {
	_, err := service.dependencies.Store.Update(ctx, func(current *relayconfig.Config) error {
		if err := ownsPending(*current, pending); err != nil {
			return err
		}
		if current.Pending.State != pending.State {
			return ErrSetupSuperseded
		}
		current.Pending = nil
		if current.Profiles == nil {
			current.Profiles = []senderrelay.Candidate{}
		}
		return nil
	})
	return err
}
