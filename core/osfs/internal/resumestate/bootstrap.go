package resumestate

import "fmt"

// BootstrapIntent identifies the only candidate directory an operation may
// create or remove. The random suffix prevents cleanup from acquiring authority
// over a candidate built by another process.
type BootstrapIntent struct {
	control Control
	nonce   BootstrapNonce
}

func NewBootstrapIntent(control Control, nonce BootstrapNonce) (BootstrapIntent, error) {
	if !control.valid() || nonce.IsZero() {
		return BootstrapIntent{}, fmt.Errorf("%w: control bootstrap intent", ErrInvalidState)
	}
	return BootstrapIntent{control: control, nonce: nonce}, nil
}

func (intent BootstrapIntent) Control() Control      { return intent.control }
func (intent BootstrapIntent) Nonce() BootstrapNonce { return intent.nonce }
func (intent BootstrapIntent) CandidateName() string { return BootstrapCandidateName(intent.nonce) }
func (intent BootstrapIntent) valid() bool           { return intent.control.valid() && !intent.nonce.IsZero() }

type InstalledControlObservation uint8

const (
	InstalledControlMissing InstalledControlObservation = iota + 1
	InstalledControlMatches
	InstalledControlDiffers
	InstalledControlUnsafe
)

type BootstrapCandidateObservation uint8

const (
	BootstrapCandidateMissing BootstrapCandidateObservation = iota + 1
	BootstrapCandidateEmpty
	BootstrapCandidateValidPartial
	BootstrapCandidateComplete
	BootstrapCandidateUnsafe
)

type BootstrapParentObservation uint8

const (
	BootstrapParentNotObserved BootstrapParentObservation = iota
	BootstrapParentSyncRequired
	BootstrapParentSynced
)

type BootstrapObservation struct {
	Installed       InstalledControlObservation
	InstalledParent BootstrapParentObservation
	Candidate       BootstrapCandidateObservation
}

type BootstrapAction uint8

const (
	BootstrapCreateCandidate BootstrapAction = iota + 1
	BootstrapContinueCandidate
	BootstrapInstallCandidateNoReplace
	BootstrapSyncOutputRoot
	BootstrapRemoveOwnedCandidate
	BootstrapUseInstalledControl
	BootstrapBlockOutputRoot
)

type BootstrapSettlement uint8

const (
	BootstrapContinuing BootstrapSettlement = iota + 1
	BootstrapReady
	BootstrapNeedsAttention
)

type BootstrapDecision struct {
	Action     BootstrapAction
	Settlement BootstrapSettlement
}

// ReduceBootstrap returns one namespace mutation. Executors sync and reobserve
// after each action, so every candidate construction and install cut is
// independently recoverable.
func ReduceBootstrap(intent BootstrapIntent, observation BootstrapObservation) (BootstrapDecision, error) {
	if !intent.valid() || !observation.valid() {
		return BootstrapDecision{}, fmt.Errorf("%w: bootstrap observation", ErrInvalidState)
	}
	if observation.Installed == InstalledControlDiffers || observation.Installed == InstalledControlUnsafe ||
		observation.Candidate == BootstrapCandidateUnsafe {
		return BootstrapDecision{Action: BootstrapBlockOutputRoot, Settlement: BootstrapNeedsAttention}, nil
	}
	if observation.Installed == InstalledControlMatches {
		if observation.InstalledParent != BootstrapParentSynced {
			return BootstrapDecision{Action: BootstrapSyncOutputRoot, Settlement: BootstrapContinuing}, nil
		}
		if observation.Candidate != BootstrapCandidateMissing {
			return BootstrapDecision{Action: BootstrapRemoveOwnedCandidate, Settlement: BootstrapContinuing}, nil
		}
		return BootstrapDecision{Action: BootstrapUseInstalledControl, Settlement: BootstrapReady}, nil
	}
	switch observation.Candidate {
	case BootstrapCandidateMissing:
		return BootstrapDecision{Action: BootstrapCreateCandidate, Settlement: BootstrapContinuing}, nil
	case BootstrapCandidateEmpty:
		// Exclusive creation can crash before the first envelope is installed. An
		// exact empty fixed directory contains no identity claim to guess about and
		// is safe to remove; a concurrent builder that loses the name must observe
		// that loss rather than install through a stale handle.
		return BootstrapDecision{Action: BootstrapRemoveOwnedCandidate, Settlement: BootstrapContinuing}, nil
	case BootstrapCandidateValidPartial:
		return BootstrapDecision{Action: BootstrapContinueCandidate, Settlement: BootstrapContinuing}, nil
	case BootstrapCandidateComplete:
		return BootstrapDecision{Action: BootstrapInstallCandidateNoReplace, Settlement: BootstrapContinuing}, nil
	default:
		return BootstrapDecision{}, fmt.Errorf("%w: bootstrap candidate observation", ErrInvalidState)
	}
}

func (observation BootstrapObservation) valid() bool {
	if observation.Installed < InstalledControlMissing || observation.Installed > InstalledControlUnsafe ||
		observation.Candidate < BootstrapCandidateMissing || observation.Candidate > BootstrapCandidateUnsafe {
		return false
	}
	if observation.Installed == InstalledControlMatches {
		return observation.InstalledParent == BootstrapParentSyncRequired ||
			observation.InstalledParent == BootstrapParentSynced
	}
	return observation.InstalledParent == BootstrapParentNotObserved
}
