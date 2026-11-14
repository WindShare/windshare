package networkdirectory

import (
	"errors"
	"fmt"
	"time"

	"github.com/windshare/windshare/core/nodedirectory"
)

var ErrUnavailable = errors.New("built-in network unavailable")

type BlockingReason string

const (
	Unprovisioned        BlockingReason = "unprovisioned"
	InvalidConfiguration BlockingReason = "invalid_network_configuration"
	TrustStateUnreadable BlockingReason = "trust_state_unreadable"
	TrustStateInvalid    BlockingReason = "trust_state_invalid"
	TrustStateUnwritable BlockingReason = "trust_state_unwritable"
	SnapshotUnavailable  BlockingReason = "directory_unavailable"
)

// UnavailableError carries the owner's recovery decision, independently of the
// diagnostic cause. RetryAt is the earliest permitted refresh, not a promise of
// connectivity or an instruction for applications to start another poller.
type UnavailableError struct {
	Reason  BlockingReason
	RetryAt time.Time
	Cause   error
}

func (err *UnavailableError) Error() string {
	if err.Cause != nil {
		return fmt.Sprintf("%s: %s: %v", ErrUnavailable, err.Reason, err.Cause)
	}
	return fmt.Sprintf("%s: %s", ErrUnavailable, err.Reason)
}
func (err *UnavailableError) Unwrap() error        { return err.Cause }
func (err *UnavailableError) Is(target error) bool { return target == ErrUnavailable }

func blocked(reason BlockingReason, cause error) *UnavailableError {
	var previous *UnavailableError
	if errors.As(cause, &previous) {
		return previous
	}
	return &UnavailableError{Reason: reason, Cause: cause}
}

// Unavailability ignores maintenance failures while a trusted snapshot is usable.
func (directory *Directory) Unavailability() *UnavailableError {
	directory.mu.Lock()
	defer directory.mu.Unlock()
	if directory.current.Usable(directory.config.Now(), nodedirectory.DefaultGrace) {
		return nil
	}
	if len(directory.config.Roots) == 0 {
		return blocked(Unprovisioned, nil)
	}
	return directory.unavailableLocked()
}

func (directory *Directory) unavailableLocked() *UnavailableError {
	failure := *blocked(SnapshotUnavailable, directory.lastError)
	if directory.failures > 0 {
		failure.RetryAt = directory.nextRefresh
	}
	return &failure
}

// RetryAt exposes only failure backoff. Successful periodic maintenance must
// never postpone an explicit retry or unknown-alias refresh.
func (directory *Directory) RetryAt() time.Time {
	directory.mu.Lock()
	defer directory.mu.Unlock()
	if directory.failures > 0 {
		return directory.nextRefresh
	}
	return time.Time{}
}
