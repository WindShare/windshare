package desktop

import (
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"time"

	"github.com/windshare/windshare/core/serviceauthority"
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
	ResetLocal          string            `json:"reset_local,omitempty"`
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

type officialAuthorizationGrant struct {
	ready             bool
	account           string
	credential        string
	ticketExpiresUnix int64
}

func officialGrantFromSnapshot(status account.Snapshot) officialAuthorizationGrant {
	if status.State != account.Ready {
		return officialAuthorizationGrant{}
	}
	var expires int64
	if !status.TicketExpires.IsZero() {
		expires = status.TicketExpires.UnixNano()
	}
	return officialAuthorizationGrant{
		ready:             true,
		account:           status.Account,
		credential:        status.Credential,
		ticketExpiresUnix: expires,
	}
}

func (a *Application) currentOfficialGrantLocked() officialAuthorizationGrant {
	if a.accountView == nil {
		return officialAuthorizationGrant{}
	}
	return officialGrantFromSnapshot(a.accountView.Account)
}

func (a *Application) recordOfficialAuthorizationFailureLocked(op *operation, view Share) {
	if op.officialFailureRecorded || view.State != Finished || view.Authorization == nil ||
		view.Authorization.Kind != engine.ShareOfficialAuthorization {
		return
	}
	op.officialFailureGrant = a.currentOfficialGrantLocked()
	op.officialFailureRecorded = true
}

func (a *Application) officialFailureGrantLocked(op *operation) officialAuthorizationGrant {
	if !op.officialFailureRecorded {
		op.officialFailureGrant = a.currentOfficialGrantLocked()
		op.officialFailureRecorded = true
	}
	return op.officialFailureGrant
}

func refreshesOfficialAuthorization(action string) bool {
	switch action {
	case "login", "register", "verify", "refresh":
		return true
	default:
		return false
	}
}

func (a *Application) resumeOfficialAuthorizationLocked(explicitSuccess bool) {
	if a.closing {
		return
	}
	grant := a.currentOfficialGrantLocked()
	if !grant.ready {
		return
	}
	trigger := "account_updated"
	if explicitSuccess {
		trigger = "account_command"
	}
	for _, op := range a.operations {
		waiting, ok := op.phase.(awaitingAuthorization)
		if !ok || waiting.authority.Kind != engine.ShareOfficialAuthorization {
			continue
		}
		// Avoid re-admitting the same locally cached ticket that just failed on a relay
		// unless the user explicitly completed a fresh authorization action.
		if !explicitSuccess && waiting.rejectedGrant == grant {
			continue
		}
		a.deps.log("host.share_official_authorization_resumed", map[string]string{
			"operation_id": op.id,
			"trigger":      trigger,
		})
		a.admit(op)
	}
}

func showsAllowanceReset(status account.Snapshot) bool {
	if status.State != account.Ready || status.ResetAt.IsZero() {
		return false
	}
	return status.Allowance == serviceauthority.AllowanceAvailable ||
		status.Allowance == serviceauthority.AllowanceSpent
}

func (a *Application) syncAccountLocked() <-chan struct{} {
	status, changed := a.deps.accounts.Snapshot()
	view := *a.accountView
	view.Account = status
	view.ResetLocal = ""
	if showsAllowanceReset(status) {
		view.ResetLocal = status.ResetAt.Local().Format("2006-01-02 15:04 MST")
	}
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
	a.accountReloads = make(chan string, 1)
	a.syncAccountLocked()
	a.readers.Go(func() {
		for {
			a.mu.Lock()
			changed := a.syncAccountLocked()
			a.resumeOfficialAuthorizationLocked(false)
			a.snapshotLocked()
			a.mu.Unlock()
			select {
			case <-a.accountContext.Done():
				return
			case <-changed:
			case requestID := <-a.accountReloads:
				err := a.deps.accounts.ReloadSelection(a.accountContext)
				a.deps.log("host.account_selection_reload_completed", map[string]string{
					"request_id": requestID, "error": errorText(err),
				})
			}
		}
	})
}

func (a *Application) requestAccountReload(requestID string) error {
	if a.closing {
		return engine.ErrClosed
	}
	if a.deps.accounts == nil {
		return account.ErrNotConfigured
	}
	// The pending read covers any further openings before it starts.
	select {
	case a.accountReloads <- requestID:
	default:
	}
	return nil
}

func (subscription *subscription) hideAccountView() {
	if subscription.releaseAccountView != nil {
		subscription.releaseAccountView()
		subscription.releaseAccountView = nil
	}
}

func (a *Application) setAccountView(subscription *subscription, request Request) Message {
	a.mu.Lock()
	defer a.mu.Unlock()
	var err error
	switch {
	case request.AccountViewVisible == nil:
		err = errors.New("account_view_visible is required")
	case !*request.AccountViewVisible:
		subscription.hideAccountView()
	case a.closing:
		err = engine.ErrClosed
	case a.deps.accounts == nil:
		err = account.ErrNotConfigured
	case subscription.releaseAccountView == nil:
		subscription.releaseAccountView = a.deps.accounts.ObserveAccount()
	}
	reply := Message{Version: ProtocolVersion, ID: request.ID}
	if err != nil {
		reply.Error = failure("request_rejected", err)
	}
	a.logRequestResult(request, reply.Error)
	return reply
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
		fields := map[string]string{"operation_id": request.OperationID, "action": input.Action, "state": result.State}
		if result.Error != nil {
			fields["error_code"] = result.Error.Code
		}
		a.deps.log("host.account_completed", fields)
		a.resumeOfficialAuthorizationLocked(err == nil && refreshesOfficialAuthorization(input.Action))
		a.snapshotLocked()
	})
	return nil
}
func (a *Application) runAccountCommand(input AccountInput) error {
	service, ctx := a.deps.accounts, a.accountContext
	switch input.Action {
	case "register":
		return service.Register(ctx)
	case "refresh":
		_, err := service.RefreshOnce(ctx)
		return err
	case "login":
		return service.Login(ctx, input.Username, input.Password)
	case "password":
		return service.SetPassword(ctx, input.AccountID, input.Username, input.Password)
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
		return service.RevokeCredential(ctx, input.AccountID, [16]byte(raw))
	default:
		return errors.New("unknown account action")
	}
}
