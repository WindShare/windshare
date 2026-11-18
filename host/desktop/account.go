package desktop

import (
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"

	"github.com/windshare/windshare/engine"
	"github.com/windshare/windshare/engine/account"
)

type AccountInput struct {
	Action     string `json:"action"`
	Username   string `json:"username,omitempty"`
	Password   string `json:"password,omitempty"`
	Email      string `json:"email,omitempty"`
	Code       string `json:"code,omitempty"`
	Credential string `json:"credential,omitempty"`
}
type AccountView struct {
	ResetLocal string           `json:"reset_local"`
	Account    account.Snapshot `json:"status"`
	Busy       bool             `json:"busy"`
	Error      *Failure         `json:"error,omitempty"`
}

func (a *Application) watchAccount() {
	if a.deps.accounts == nil {
		return
	}
	initial, _ := a.deps.accounts.Snapshot()
	a.accountView = &AccountView{Account: initial, ResetLocal: initial.ResetAt.Local().Format("Jan 2 15:04 MST")}
	a.readers.Go(func() {
		for {
			status, changed := a.deps.accounts.Snapshot()
			a.mu.Lock()
			view := *a.accountView
			view.Account = status
			view.ResetLocal = status.ResetAt.Local().Format("Jan 2 15:04 MST")
			a.accountView = &view
			a.snapshotLocked()
			a.mu.Unlock()
			select {
			case <-a.accountContext.Done():
				return
			case <-changed:
			}
		}
	})
}
func (a *Application) accountCommand(request Request) error {
	if a.closing {
		return engine.ErrClosed
	}
	if a.deps.accounts == nil {
		return account.ErrNotConfigured
	}
	if request.Account == nil || !validOperation(request.ID) {
		return errors.New("account command is required")
	}
	encoded, err := json.Marshal(request.Account)
	if err != nil || len(encoded) > 2048 {
		return errors.New("invalid account command")
	}
	digest := sha256.Sum256(encoded)
	clear(encoded)
	if previous, exists := a.accountRequests[request.ID]; exists {
		if previous != digest {
			return errors.New("account request ID reused")
		}
		return nil
	}
	if a.accountView.Busy {
		return errors.New("an account operation is already running")
	}
	if len(a.accountRequests) >= maximumOperations {
		return errors.New("account operation limit reached")
	}
	a.accountRequests[request.ID] = digest
	view := *a.accountView
	view.Busy, view.Error = true, nil
	a.accountView = &view
	input := *request.Account
	a.deps.log("host.account_operation", map[string]string{"request_id": request.ID, "action": input.Action})
	a.work.Go(func() {
		err := a.runAccountCommand(input)
		a.mu.Lock()
		defer a.mu.Unlock()
		view := *a.accountView
		view.Busy, view.Error = false, nil
		if err != nil {
			view.Error = failure("account_operation_failed", err)
		}
		a.accountView = &view
		a.snapshotLocked()
	})
	return nil
}
func (a *Application) runAccountCommand(input AccountInput) error {
	service, ctx := a.deps.accounts, a.accountContext
	switch input.Action {
	case "retry":
		service.Retry()
		return nil
	case "login":
		return service.Login(ctx, input.Username, input.Password)
	case "password":
		return service.SetPassword(ctx, input.Username, input.Password)
	case "email":
		return service.RequestEmail(ctx, input.Email)
	case "verify":
		return service.VerifyEmail(ctx, input.Code)
	case "revoke":
		raw, err := base64.RawURLEncoding.Strict().DecodeString(input.Credential)
		if err != nil || len(raw) != 16 {
			return errors.New("invalid credential ID")
		}
		return service.RevokeCredential(ctx, [16]byte(raw))
	default:
		return errors.New("unknown account action")
	}
}
