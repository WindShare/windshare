package resumestate

import (
	"fmt"
	"math"

	"github.com/windshare/windshare/core/catalog"
	"github.com/windshare/windshare/core/transfer"
)

type SessionLifecycle uint8

const (
	SessionActive               SessionLifecycle = 1
	SessionPausing              SessionLifecycle = 2
	SessionPaused               SessionLifecycle = 3
	SessionPausedNeedsAttention SessionLifecycle = 4
	SessionCompleting           SessionLifecycle = 5
	SessionDiscarding           SessionLifecycle = 6
)

func (lifecycle SessionLifecycle) Valid() bool {
	return lifecycle >= SessionActive && lifecycle <= SessionDiscarding
}

func (lifecycle SessionLifecycle) String() string {
	switch lifecycle {
	case SessionActive:
		return "active"
	case SessionPausing:
		return "pausing"
	case SessionPaused:
		return "paused"
	case SessionPausedNeedsAttention:
		return "paused-needs-attention"
	case SessionCompleting:
		return "completing"
	case SessionDiscarding:
		return "discarding"
	default:
		return "invalid"
	}
}

func CanTransitionSession(from, to SessionLifecycle) bool {
	switch from {
	case SessionActive:
		return to == SessionPausing || to == SessionCompleting || to == SessionDiscarding
	case SessionPausing:
		return to == SessionPaused || to == SessionPausedNeedsAttention
	case SessionPaused:
		return to == SessionActive || to == SessionDiscarding
	case SessionPausedNeedsAttention:
		return to == SessionActive || to == SessionDiscarding
	case SessionCompleting:
		return to == SessionPausedNeedsAttention
	default:
		return false
	}
}

type HeaderSpec struct {
	Backend        transfer.OutputBackendID
	SessionID      transfer.OutputSessionID
	Selection      transfer.OutputSelection
	OutputRoot     OutputRootBinding
	OutputAncestry OutputAncestryBinding
}

type Header struct {
	backend                transfer.OutputBackendID
	sessionID              transfer.OutputSessionID
	shareInstance          catalog.ShareInstance
	syntheticRoot          catalog.DirectoryID
	resumeIntent           transfer.ResumeIntent
	selectionIdentity      transfer.SelectionIdentity
	selectedDirectoryCount uint32
	selectedFileCount      uint32
	outputRoot             OutputRootBinding
	outputAncestry         OutputAncestryBinding
	lifecycle              SessionLifecycle
	stateGeneration        uint64
}

func NewHeader(spec HeaderSpec) (Header, error) {
	directories := spec.Selection.Directories()
	files := spec.Selection.Files()
	if len(files) > MaxFilesPerSession || len(directories)+len(files) > MaxSelectedEntriesPerSession {
		return Header{}, fmt.Errorf("%w: selected plan exceeds session bound", ErrInvalidState)
	}
	return newHeaderFromClaims(headerClaims{
		backend: spec.Backend, sessionID: spec.SessionID,
		shareInstance: spec.Selection.ShareInstance(), syntheticRoot: spec.Selection.SyntheticRoot(),
		resumeIntent: spec.Selection.ResumeIntent(), selectionIdentity: spec.Selection.Identity(),
		selectedDirectoryCount: uint32(len(directories)), selectedFileCount: uint32(len(files)),
		outputRoot: spec.OutputRoot, outputAncestry: spec.OutputAncestry,
		lifecycle: SessionActive, stateGeneration: 1,
	})
}

type headerClaims struct {
	backend                transfer.OutputBackendID
	sessionID              transfer.OutputSessionID
	shareInstance          catalog.ShareInstance
	syntheticRoot          catalog.DirectoryID
	resumeIntent           transfer.ResumeIntent
	selectionIdentity      transfer.SelectionIdentity
	selectedDirectoryCount uint32
	selectedFileCount      uint32
	outputRoot             OutputRootBinding
	outputAncestry         OutputAncestryBinding
	lifecycle              SessionLifecycle
	stateGeneration        uint64
}

func newHeaderFromClaims(claims headerClaims) (Header, error) {
	_, backendErr := transfer.NewOutputBackendID(string(claims.backend))
	selectedCount := uint64(claims.selectedDirectoryCount) + uint64(claims.selectedFileCount)
	if backendErr != nil || claims.sessionID.IsZero() || claims.shareInstance.IsZero() || claims.syntheticRoot.IsZero() ||
		claims.resumeIntent.IsZero() || claims.selectionIdentity.IsZero() || !claims.outputRoot.valid() ||
		!claims.outputAncestry.valid() ||
		!claims.lifecycle.Valid() || claims.stateGeneration == 0 ||
		uint64(claims.selectedFileCount) > MaxFilesPerSession || selectedCount > MaxSelectedEntriesPerSession {
		return Header{}, fmt.Errorf("%w: session header", ErrInvalidState)
	}
	return Header(claims), nil
}

func (header Header) Backend() transfer.OutputBackendID    { return header.backend }
func (header Header) SessionID() transfer.OutputSessionID  { return header.sessionID }
func (header Header) ShareInstance() catalog.ShareInstance { return header.shareInstance }
func (header Header) SyntheticRoot() catalog.DirectoryID   { return header.syntheticRoot }
func (header Header) ResumeIntent() transfer.ResumeIntent  { return header.resumeIntent }
func (header Header) SelectionIdentity() transfer.SelectionIdentity {
	return header.selectionIdentity
}
func (header Header) SelectedDirectoryCount() uint32         { return header.selectedDirectoryCount }
func (header Header) SelectedFileCount() uint32              { return header.selectedFileCount }
func (header Header) OutputRoot() OutputRootBinding          { return header.outputRoot }
func (header Header) OutputAncestry() OutputAncestryBinding  { return header.outputAncestry }
func (header Header) ResumeNamespace() transfer.ResumeIntent { return header.resumeIntent }
func (header Header) Lifecycle() SessionLifecycle            { return header.lifecycle }
func (header Header) StateGeneration() uint64                { return header.stateGeneration }

func (header Header) withLifecycle(next SessionLifecycle) (Header, error) {
	if !header.valid() || !CanTransitionSession(header.lifecycle, next) || header.stateGeneration == math.MaxUint64 {
		return Header{}, fmt.Errorf("%w: session %s -> %s", ErrInvalidTransition, header.lifecycle, next)
	}
	header.lifecycle = next
	header.stateGeneration++
	return header, nil
}

func (header Header) valid() bool {
	validated, err := newHeaderFromClaims(headerClaims(header))
	return err == nil && validated == header
}
