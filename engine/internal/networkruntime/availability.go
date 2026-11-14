package networkruntime

import (
	"errors"

	"github.com/windshare/windshare/engine/networkdirectory"
)

var ErrUnavailable = networkdirectory.ErrUnavailable

const (
	NoRelayCandidates networkdirectory.BlockingReason = "no_relay_candidates"
	AliasUnavailable  networkdirectory.BlockingReason = "alias_unavailable"
)

// Unavailability reads the current default-network decision without fetching.
// Applications can reject a retry during cooldown without claiming admission
// started; all timing and coalescing still belong to the directory.
func (r *Runtime) Unavailability() *networkdirectory.UnavailableError {
	if r.directory == nil {
		return unavailable(r.directoryError, networkdirectory.Unprovisioned)
	}
	if failure := r.directory.Unavailability(); failure != nil {
		return failure
	}
	plan, err := r.buildPlan(nil, false, r.generation())
	if err != nil || len(plan.Network) == 0 {
		failure := unavailable(err, NoRelayCandidates)
		failure.RetryAt = r.directory.RetryAt()
		return failure
	}
	return nil
}

func unavailable(cause error, reason networkdirectory.BlockingReason) *networkdirectory.UnavailableError {
	var failure *networkdirectory.UnavailableError
	if errors.As(cause, &failure) {
		return failure
	}
	return &networkdirectory.UnavailableError{Reason: reason, Cause: cause}
}

func (r *Runtime) noCandidates(reason networkdirectory.BlockingReason) error {
	if failure := r.Unavailability(); failure != nil {
		return failure
	}
	failure := unavailable(nil, reason)
	if r.directory != nil {
		failure.RetryAt = r.directory.RetryAt()
	}
	return failure
}
