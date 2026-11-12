package desktop

import (
	"context"
	"errors"

	"github.com/windshare/windshare/engine"
	"github.com/windshare/windshare/engine/relayconfig"
	"github.com/windshare/windshare/engine/relaysetup"
)

type SetupInput struct {
	ID       string `json:"id"`
	RelayURL string `json:"relay_url"`
	FrontURL string `json:"front_url"`
}

type SetupView struct {
	State           string   `json:"state"`
	ID              string   `json:"id,omitempty"`
	OperationID     string   `json:"operation_id,omitempty"`
	Stage           string   `json:"stage,omitempty"`
	VerificationURL string   `json:"verification_url,omitempty"`
	UserCode        string   `json:"user_code,omitempty"`
	Error           *Failure `json:"error,omitempty"`
}

type setupCommand struct {
	input       SetupInput
	operationID string
}

func (a *Application) configure(request Request) error {
	if a.closing {
		return engine.ErrClosed
	}
	input := request.Setup
	if input == nil || !validOperation(input.ID) {
		return errors.New("setup identity is required")
	}
	if previous, ok := a.setupRequests[input.ID]; ok {
		if previous.input != *input || previous.operationID != request.OperationID {
			return errors.New("setup identity already belongs to another configuration")
		}
		return nil
	}
	if a.setup.State == "running" {
		return errors.New("relay setup is already running")
	}
	if len(a.setupRequests) >= maximumOperations {
		return errors.New("application setup limit reached")
	}
	op, ok := a.operations[request.OperationID]
	if !ok {
		return errors.New("creation operation not found")
	}
	if _, waiting := op.phase.(awaitingConfiguration); !waiting {
		return errors.New("share is no longer waiting for configuration")
	}
	if input.RelayURL == "" && input.FrontURL == "" {
		return errors.New("enter a relay or frontend URL")
	}
	if input.FrontURL != "" {
		if err := relayconfig.ValidateFrontURL(input.FrontURL); err != nil {
			return err
		}
	}
	ctx, cancel := context.WithCancel(context.Background())
	a.cancelSetup = cancel
	a.setup = SetupView{State: "running", ID: input.ID, OperationID: op.id}
	a.setupRequests[input.ID] = setupCommand{input: *input, operationID: op.id}
	a.work.Add(1)
	go a.runSetup(ctx, op.id, *input)
	return nil
}

func (a *Application) runSetup(ctx context.Context, operationID string, input SetupInput) {
	defer a.work.Done()
	err := a.deps.setup(ctx, input, func(progress relaysetup.Progress) {
		a.mu.Lock()
		defer a.mu.Unlock()
		a.setup.Stage, a.setup.VerificationURL, a.setup.UserCode = progress.Stage, progress.VerificationURL, progress.UserCode
		a.snapshotLocked()
	})
	a.mu.Lock()
	defer a.mu.Unlock()
	completed := err == nil && ctx.Err() == nil
	a.cancelSetup()
	a.cancelSetup = nil
	if err != nil {
		a.setup.State, a.setup.Error = "failed", failure("setup_failed", err)
	} else {
		a.setup.State = "finished"
	}
	a.deps.log("host.setup_completed", a.setup)
	op := a.operations[operationID]
	if _, waiting := op.phase.(awaitingConfiguration); waiting && completed && !a.closing {
		// An entered relay is an explicit choice for this intent. The setup
		// service preserves saved active sets, including disabled defaults.
		if input.RelayURL != "" {
			op.relays = []string{input.RelayURL}
		}
	}
	if completed && !a.closing {
		for _, pending := range a.operations {
			if _, waiting := pending.phase.(awaitingConfiguration); waiting {
				a.admit(pending)
			}
		}
	}
	a.snapshotLocked()
}

func nativeSetup(ctx context.Context, input SetupInput, progress func(relaysetup.Progress)) error {
	store := relayconfig.FileStore{}
	if input.FrontURL != "" {
		if _, err := store.Update(ctx, func(config *relayconfig.Config) error {
			config.FrontURL = input.FrontURL
			return nil
		}); err != nil {
			return err
		}
	}
	if input.RelayURL == "" {
		return nil
	}
	service := relaysetup.New(relaysetup.Dependencies{Store: relaysetup.FileStore{Settings: store}})
	_, err := service.Run(ctx, relaysetup.Request{
		URL: input.RelayURL, Operation: relaysetup.AddProfile,
		DeviceName: relaysetup.DefaultDeviceName("WindShare desktop"), NoBrowser: true,
	}, progress)
	return err
}
