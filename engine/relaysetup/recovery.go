package relaysetup

import "context"

// RecoveryAction describes what the last observed transaction permits, rather
// than equating every persisted checkpoint with a resumable installation.
type RecoveryAction string

const (
	RecoveryUnknown     RecoveryAction = "unknown"
	RecoveryRestart     RecoveryAction = "restart"
	RecoveryContinue    RecoveryAction = "continue"
	RecoveryCancel      RecoveryAction = "cancel"
	RecoveryRetryCancel RecoveryAction = "retry_cancel"
	RecoveryBlocked     RecoveryAction = "blocked"
)

type Recovery struct {
	Action                RecoveryAction
	RelayURL, OperationID string
}

// SetupError carries the workflow's last transaction outcome to its caller.
// Reading storage again to explain an interruption could block shutdown and
// describe a different operation started after this one finished.
type SetupError struct {
	Recovery Recovery
	cause    error
}

func (failure *SetupError) Error() string { return failure.cause.Error() }
func (failure *SetupError) Unwrap() error { return failure.cause }

// recoveryStore observes existing transactions only; it performs no extra I/O.
// Each Run owns one instance, so concurrent cancellation cannot overwrite the
// failed run's receipt before its caller consumes it.
type recoveryStore struct {
	Store
	request  Request
	recovery Recovery
}

func (store *recoveryStore) Load() (Snapshot, error) {
	saved, err := store.Store.Load()
	if err != nil {
		store.recovery.Action = RecoveryUnknown
	} else {
		store.observe(saved)
	}
	return saved, err
}

func (store *recoveryStore) Update(ctx context.Context, change func(*Snapshot) error) (Snapshot, error) {
	rejected := false
	saved, err := store.Store.Update(ctx, func(current *Snapshot) error {
		store.observe(*current)
		err := change(current)
		rejected = err != nil
		return err
	})
	switch {
	case err == nil:
		store.observe(saved)
	case !rejected:
		// A storage error can occur after the durable commit point. Neither the
		// old checkpoint nor the proposed replacement is then safe to promise.
		store.recovery.Action = RecoveryUnknown
	}
	return saved, err
}

func (store *recoveryStore) observe(saved Snapshot) {
	pending := saved.Pending
	if pending == nil {
		store.recovery = Recovery{Action: RecoveryRestart, RelayURL: store.request.URL}
		return
	}
	store.recovery = Recovery{RelayURL: pending.RelayURL, OperationID: pending.OperationID}
	switch {
	case !sameRelay(pending.RelayURL, store.request.URL) || pending.Operation != store.request.Operation:
		store.recovery.Action = RecoveryBlocked
	case pending.phase.stage() == (cancelling{}).stage():
		store.recovery.Action = RecoveryRetryCancel
	case pending.SettingsRevision != saved.SettingsRevision():
		store.recovery.Action = RecoveryCancel
	default:
		store.recovery.Action = RecoveryContinue
	}
}
