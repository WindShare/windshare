package desktop

import (
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"time"

	"github.com/windshare/windshare/engine"
	"github.com/windshare/windshare/engine/account"
)

type AccountInput struct {
	Action     string `json:"action"`
	AccountID  string `json:"account_id,omitempty"`
	Nickname   string `json:"nickname,omitempty"`
	Challenge  string `json:"challenge,omitempty"`
	Username   string `json:"username,omitempty"`
	Password   string `json:"password,omitempty"`
	Email      string `json:"email,omitempty"`
	Code       string `json:"code,omitempty"`
	Credential string `json:"credential,omitempty"`
}
type AccountView struct {
	ResetLocal          string            `json:"reset_local"`
	NicknameChangeLocal string            `json:"nickname_change_local,omitempty"`
	Account             account.Snapshot  `json:"status"`
	Operation           *AccountOperation `json:"operation,omitempty"`
}

type AccountOperation struct {
	ID    string   `json:"id"`
	State string   `json:"state"`
	Error *Failure `json:"error,omitempty"`
}

func (view *AccountView) busy() bool {
	return view != nil && view.Operation != nil && view.Operation.State == "running"
}

func (a *Application) syncAccountLocked() <-chan struct{} {
	status, changed := a.deps.accounts.Snapshot()
	view := *a.accountView
	view.Account = status
	view.ResetLocal = status.ResetAt.Local().Format("2006-01-02 15:04 MST")
	view.NicknameChangeLocal = ""
	if at := status.Profile.Nickname.NextChangeAt; at > 0 {
		view.NicknameChangeLocal = time.Unix(at, 0).Local().Format("2006-01-02 15:04:05 MST")
	}
	a.accountView = &view
	return changed
}

func (a *Application) watchAccount() {
	if a.deps.accounts == nil {
		return
	}
	a.accountView = &AccountView{}
	a.syncAccountLocked()
	a.readers.Go(func() {
		for {
			a.mu.Lock()
			changed := a.syncAccountLocked()
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
	if request.Account == nil || !validOperation(request.OperationID) {
		return errors.New("account command is required")
	}
	encoded, err := json.Marshal(request.Account)
	if err != nil || len(encoded) > 2048 {
		return errors.New("invalid account command")
	}
	digest := sha256.Sum256(encoded)
	clear(encoded)
	if previous, exists := a.accountRequests[request.OperationID]; exists {
		if previous != digest {
			return errors.New("account operation ID reused")
		}
		return nil
	}
	if a.accountView.busy() {
		return errors.New("an account operation is already running")
	}
	if len(a.accountRequests) >= maximumOperations {
		return errors.New("account operation limit reached")
	}
	a.accountRequests[request.OperationID] = digest
	view := *a.accountView
	view.Operation = &AccountOperation{ID: request.OperationID, State: "running"}
	a.accountView = &view
	input := *request.Account
	a.deps.log("host.account_operation", map[string]string{"request_id": request.ID, "operation_id": request.OperationID, "action": input.Action})
	a.work.Go(func() {
		err := a.runAccountCommand(input)
		a.mu.Lock()
		defer a.mu.Unlock()
		// Publish the identity and its command receipt together even if the observer coalesces updates.
		a.syncAccountLocked()
		result := &AccountOperation{ID: request.OperationID, State: "succeeded"}
		if err != nil {
			result.State, result.Error = "failed", failure(account.CommandErrorCode(err), err)
		}
		a.accountView.Operation = result
		a.deps.log("host.account_completed", map[string]string{"operation_id": request.OperationID, "action": input.Action, "state": result.State})
		a.snapshotLocked()
	})
	return nil
}
func (a *Application) runAccountCommand(input AccountInput) error {
	service, ctx := a.deps.accounts, a.accountContext
	switch input.Action {
	case "register":
		return service.Register(ctx)
	case "retry":
		service.Retry()
		return nil
	case "login":
		return service.Login(ctx, input.Username, input.Password)
	case "password":
		return service.SetPassword(ctx, input.Username, input.Password)
	case "nickname":
		return service.SetNickname(ctx, input.AccountID, input.Nickname)
	case "request_profile_email":
		return service.RequestProfileEmail(ctx, input.AccountID, input.Email)
	case "confirm_profile_email":
		return service.ConfirmProfileEmail(ctx, input.AccountID, input.Challenge, input.Code)
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
