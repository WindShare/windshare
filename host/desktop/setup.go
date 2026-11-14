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
	State           string                    `json:"state"`
	ID              string                    `json:"id,omitempty"`
	OperationID     string                    `json:"operation_id,omitempty"`
	RelayURL        string                    `json:"relay_url,omitempty"`
	Stage           string                    `json:"stage,omitempty"`
	VerificationURL string                    `json:"verification_url,omitempty"`
	UserCode        string                    `json:"user_code,omitempty"`
	FrontendSaved   bool                      `json:"frontend_saved"`
	RelaySaved      bool                      `json:"relay_saved"`
	Recovery        relaysetup.RecoveryAction `json:"recovery,omitempty"`
	Error           *Failure                  `json:"error,omitempty"`
}

func (view SetupView) busy() bool { return view.State == "running" || view.State == "cancelling" }

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
	if a.setup.busy() {
		return errors.New("relay setup is already running")
	}
	if a.setup.State == "cancel_failed" || a.setup.Recovery == relaysetup.RecoveryCancel || a.setup.Recovery == relaysetup.RecoveryRetryCancel {
		return errors.New("cancel the unfinished relay setup before starting another")
	}
	if a.setup.State == "failed" && a.setup.Recovery == relaysetup.RecoveryContinue && a.setup.OperationID != request.OperationID {
		return errors.New("finish or cancel the pending relay setup first")
	}
	if len(a.setupRequests) >= maximumOperations {
		return errors.New("application setup limit reached")
	}
	if request.OperationID != "" {
		op, ok := a.operations[request.OperationID]
		if !ok {
			return errors.New("creation operation not found")
		}
		if _, ended := op.phase.(terminal); ended {
			return errors.New("creation operation has finished")
		}
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
	a.interruptSetup = cancel
	a.setup = SetupView{State: "running", ID: input.ID, OperationID: request.OperationID, RelayURL: input.RelayURL}
	a.setupRequests[input.ID] = setupCommand{input: *input, operationID: request.OperationID}
	a.work.Add(1)
	go a.runSetup(ctx, request.OperationID, *input)
	return nil
}

func (a *Application) runSetup(ctx context.Context, operationID string, input SetupInput) {
	defer a.work.Done()
	result, err := a.deps.setup(ctx, input, func(progress relaysetup.Progress) {
		a.mu.Lock()
		defer a.mu.Unlock()
		if progress.Stage == "frontend_saved" {
			a.setup.FrontendSaved = true
		}
		if a.setup.State == "running" {
			a.setup.Stage, a.setup.VerificationURL, a.setup.UserCode = progress.Stage, progress.VerificationURL, progress.UserCode
		}
		a.snapshotLocked()
	})
	a.mu.Lock()
	a.setup.FrontendSaved, a.setup.RelaySaved = result.FrontendSaved, result.RelaySaved
	a.setup.RelayURL = result.RelayURL
	// Only the completed workflow receipt can identify an enrollment to clean up.
	// An input URL alone does not mean this intent acquired that relay's approval.
	if result.RelayURL == "" {
		a.setup.Recovery = relaysetup.RecoveryRestart
	}
	if setupError, ok := errors.AsType[*relaysetup.SetupError](err); ok {
		a.setup.Recovery = setupError.Recovery.Action
	}
	cancelled := ctx.Err() != nil || a.closing
	a.interruptSetup()
	a.interruptSetup = nil
	if cancelled {
		a.setup.State = "cancelling"
		a.snapshotLocked()
		a.mu.Unlock()
		a.finishSetupCancellation()
		return
	}
	defer a.mu.Unlock()
	a.setup.VerificationURL, a.setup.UserCode = "", ""
	if err != nil {
		a.setup.State, a.setup.Error = "failed", failure("setup_failed", err)

	} else {
		a.setup.State = "finished"
	}
	a.deps.log("host.setup_completed", a.setup)
	if err == nil {
		op := a.operations[operationID]
		if op != nil && waitingForAdmission(op.phase) && result.RelayURL != "" {
			// A setup choice applies to this retained selection. It does not
			// replace saved defaults or unrelated selections' explicit choices.
			op.relays = []string{result.RelayURL}
		}
		for _, pending := range a.operations {
			if waitingForAdmission(pending.phase) {
				a.admit(pending)
			}
		}
	}
	a.snapshotLocked()
}
