package desktop

import (
	"context"
	"errors"
	"reflect"
	"slices"

	"github.com/windshare/windshare/core/networkendpoint"
	"github.com/windshare/windshare/engine"
	"github.com/windshare/windshare/engine/relayconfig"
	"github.com/windshare/windshare/engine/relaysetup"
)

type SetupInput struct {
	ID             string                      `json:"id"`
	RelayURL       string                      `json:"relay_url"`
	FrontURL       string                      `json:"front_url"`
	AccessKey      string                      `json:"access_key,omitempty"`
	RelayOperation relaysetup.Operation        `json:"relay_operation,omitempty"`
	Preference     *relayconfig.PreferenceEdit `json:"preference,omitempty"`
}

type SetupResult struct {
	FrontendSaved bool
	RelayURL      string
	RelaySaved    bool
	RelayRemoved  bool
}

func (result SetupResult) affectsAdmission(explicitRelays []string) bool {
	// A pinned share uses the shared frontend and credentials for its own routes.
	// Its relay choice is independent of changes to saved defaults.
	if len(explicitRelays) == 0 || result.FrontendSaved {
		return true
	}
	if !result.RelaySaved {
		return false
	}
	saved, err := networkendpoint.NormalizeRelay(result.RelayURL)
	if err != nil {
		return false
	}
	return slices.ContainsFunc(explicitRelays, func(address string) bool {
		current, err := networkendpoint.NormalizeRelay(address)
		return err == nil && current.DialURL == saved.DialURL
	})
}

type SetupView struct {
	State           string                    `json:"state"`
	ID              string                    `json:"id,omitempty"`
	OperationID     string                    `json:"operation_id,omitempty"`
	RelayURL        string                    `json:"relay_url,omitempty"`
	RelayOperation  relaysetup.Operation      `json:"relay_operation,omitempty"`
	Stage           string                    `json:"stage,omitempty"`
	VerificationURL string                    `json:"verification_url,omitempty"`
	ServiceLogin    *relaysetup.ServiceLogin  `json:"service_login,omitempty"`
	UserCode        string                    `json:"user_code,omitempty"`
	FrontendSaved   bool                      `json:"frontend_saved"`
	RelaySaved      bool                      `json:"relay_saved"`
	RelayRemoved    bool                      `json:"relay_removed"`
	Recovery        relaysetup.RecoveryAction `json:"recovery,omitempty"`
	Error           *Failure                  `json:"error,omitempty"`
}

func (view SetupView) busy() bool { return view.State == "running" || view.State == "cancelling" }

func (view *SetupView) clearApproval() {
	view.VerificationURL = ""
	view.UserCode = ""
	view.ServiceLogin = nil
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
		if !reflect.DeepEqual(previous.input, *input) || previous.operationID != request.OperationID {
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
		if a.reconcileAndProjectLocked(op).State == Finished {
			return errors.New("creation operation has finished")
		}
	}
	if err := input.validate(); err != nil {
		return err
	}
	ctx, cancel := context.WithCancel(context.Background())
	a.interruptSetup = cancel
	a.setup = SetupView{
		State: "running", ID: input.ID, OperationID: request.OperationID,
		RelayURL: input.RelayURL, RelayOperation: input.RelayOperation,
	}
	a.setupRequests[input.ID] = setupCommand{input: *input, operationID: request.OperationID}
	a.work.Add(1)
	go a.runSetup(ctx, *input)
	return nil
}

func (input SetupInput) validate() error {
	if input.Preference != nil && (input.RelayURL != "" || input.FrontURL != "") {
		return errors.New("invalid sharing settings request")
	}
	if input.RelayURL == "" {
		if input.RelayOperation != "" || input.AccessKey != "" {
			return errors.New("relay operation and access key require a relay URL")
		}
	} else if input.RelayOperation != relaysetup.AddProfile && input.RelayOperation != relaysetup.AuthorizeDevice {
		return errors.New("invalid relay setup operation")
	}
	if input.Preference == nil && input.RelayURL == "" && input.FrontURL == "" {
		return errors.New("enter a relay or frontend URL")
	}
	if input.FrontURL != "" {
		return relayconfig.ValidateFrontURL(input.FrontURL)
	}
	return nil
}

func setupRecovery(result SetupResult, err error) relaysetup.RecoveryAction {
	if setupError, ok := errors.AsType[*relaysetup.SetupError](err); ok {
		return setupError.Recovery.Action
	}
	// Only the completed workflow receipt can identify an enrollment to clean up.
	// An input URL alone does not mean this intent acquired that relay's approval.
	if result.RelayURL == "" {
		return relaysetup.RecoveryRestart
	}
	return ""
}

func (a *Application) runSetup(ctx context.Context, input SetupInput) {
	defer a.work.Done()
	result, err := a.deps.setup(ctx, input, func(progress relaysetup.Progress) {
		a.mu.Lock()
		defer a.mu.Unlock()
		if progress.Stage == "frontend_saved" {
			a.setup.FrontendSaved = true
		}
		if a.setup.State == "running" {
			a.setup.Stage = progress.Stage
			if progress.VerificationURL != "" {
				a.setup.VerificationURL, a.setup.UserCode, a.setup.ServiceLogin = progress.VerificationURL, progress.UserCode, progress.ServiceLogin
			}
		}
		a.snapshotLocked()
	})
	preferences, preferencesErr := a.deps.load()
	a.mu.Lock()
	if preferencesErr == nil {
		a.preferences = preferences.Preferences(a.deps.defaultFrontend)
	}
	a.setup.FrontendSaved, a.setup.RelaySaved, a.setup.RelayRemoved = result.FrontendSaved, result.RelaySaved, result.RelayRemoved
	a.setup.RelayURL = result.RelayURL
	cancelled := ctx.Err() != nil || a.closing
	a.interruptSetup()
	a.interruptSetup = nil
	a.setup.clearApproval()
	if cancelled {
		cleanup := setupCleanupFor(a.setup.RelayURL, a.setup.RelaySaved, setupRecovery(result, err))
		a.setup.State, a.setup.Recovery = "cancelling", ""
		a.snapshotLocked()
		a.mu.Unlock()
		a.finishSetupCancellation(cleanup)
		return
	}
	defer a.mu.Unlock()
	if err != nil {
		a.setup.State = "failed"
		a.setup.Recovery = setupRecovery(result, err)
		a.setup.Error = failure("setup_failed", err)
	} else {
		a.setup.State, a.setup.Recovery = "finished", ""
	}
	a.logSetupResult("host.setup_completed")
	if err == nil {
		for _, pending := range a.operations {
			if setupResumesPhase(pending.phase) && result.affectsAdmission(pending.explicitRelays) {
				a.admit(pending)
			}
		}
	}
	a.snapshotLocked()
}
