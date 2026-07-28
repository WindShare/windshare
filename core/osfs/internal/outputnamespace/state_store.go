// Package outputnamespace owns durable private namespace transitions for resumable output.
package outputnamespace

import (
	"bytes"
	"errors"
	"fmt"
	"io"

	"github.com/windshare/windshare/core/osfs/internal/outputcap"
	"github.com/windshare/windshare/core/osfs/internal/resumestate"
)

const (
	// AllocationAttempts bounds private-name collision retries and recovery scans.
	AllocationAttempts = 16
	// FileShardInspectionLimit detects one entry beyond the supported session bound.
	FileShardInspectionLimit = resumestate.MaxFileStateEntriesPerSession + 1
)

// StateInstallStage identifies the fixed-name mutation whose reported failure
// was settled by reopening the exact intended image.
type StateInstallStage uint8

const (
	StateInstallCreate StateInstallStage = iota + 1
	StateInstallReplace
)

// StateInstallObserver receives only unusual adopted cuts. Normal successful
// installs stay silent so the state store does not expose its internal workflow.
type StateInstallObserver interface {
	ObserveStateInstall(StateInstallCut)
}

// StateInstallObserverFunc adapts a function to StateInstallObserver.
type StateInstallObserverFunc func(StateInstallCut)

func (observe StateInstallObserverFunc) ObserveStateInstall(cut StateInstallCut) {
	if observe != nil {
		observe(cut)
	}
}

// StoreConfig supplies the nondeterministic and observability ports used by a Store.
type StoreConfig struct {
	Random   io.Reader
	Observer StateInstallObserver
}

// Store owns fixed-name record installation and restart recovery.
type Store struct {
	random   io.Reader
	observer StateInstallObserver
}

// NewStore constructs a state store without granting it any filesystem authority.
func NewStore(config StoreConfig) Store {
	return Store{random: config.Random, observer: config.Observer}
}

// StateInstallCut describes a reported-failure cut proven adopted by exact reopen.
type StateInstallCut struct {
	stage                     StateInstallStage
	targetName                string
	encoded                   []byte
	mutationReportedFailure   bool
	parentSyncReportedFailure bool
}

func (cut StateInstallCut) Stage() StateInstallStage        { return cut.stage }
func (cut StateInstallCut) TargetName() string              { return cut.targetName }
func (cut StateInstallCut) Encoded() []byte                 { return bytes.Clone(cut.encoded) }
func (cut StateInstallCut) MutationReportedFailure() bool   { return cut.mutationReportedFailure }
func (cut StateInstallCut) ParentSyncReportedFailure() bool { return cut.parentSyncReportedFailure }

// RecordImage binds encoded state to the generation it is expected to carry.
type RecordImage struct {
	encoded    []byte
	generation uint64
}

// NewRecordImage copies an encoded image so validation cannot race caller mutation.
func NewRecordImage(encoded []byte, generation uint64) RecordImage {
	return RecordImage{encoded: bytes.Clone(encoded), generation: generation}
}

type CreateOutcome uint8

const (
	// NotInstalled means the fixed target did not adopt the requested image.
	// The caller retains no authority derived from this creation attempt.
	CreateNotInstalled CreateOutcome = iota + 1
	// Adopted means the fixed target was reopened and byte-verified as the
	// requested image. Cleanup errors do not change that durable authority.
	CreateAdopted
	// Uncertain means mutation was attempted but the fixed target could not be
	// classified as either absent or the requested image.
	CreateUncertain
)

type ReplaceOutcome uint8

const (
	// Unchanged is returned only when the fixed target has been reopened and
	// byte-verified as the caller's current generation after any attempted replace.
	ReplaceUnchanged ReplaceOutcome = iota + 1
	// Adopted means the fixed target has been reopened and byte-verified as the
	// next generation. The caller must advance its in-memory authority.
	ReplaceAdopted
	// Uncertain permanently invalidates the current owner. Only a fresh namespace
	// reopen may decide which generation is authoritative.
	ReplaceUncertain
)

func (store Store) CreateRecord(
	directory outputcap.Directory,
	name string,
	encoded []byte,
	limit int,
) (CreateOutcome, error) {
	if err := validateEncodedState(encoded, limit); err != nil {
		return CreateNotInstalled, err
	}
	for range AllocationAttempts {
		temporaryName, err := store.temporaryName(name)
		if err != nil {
			return CreateNotInstalled, err
		}
		temporary, err := directory.CreateFile(temporaryName, true, int64(len(encoded)))
		if errors.Is(err, outputcap.ErrNamespaceCollision) {
			if closeErr := closeFile(temporary); closeErr != nil {
				return CreateNotInstalled, errors.Join(err, closeErr)
			}
			continue
		}
		if err != nil {
			return CreateNotInstalled, errors.Join(err, closeFile(temporary))
		}
		attempt, installErr := createStateRecordWithTemporary(
			directory, name, temporaryName, temporary, encoded, limit,
		)
		outcome, cut := attempt.outcome, attempt.cut
		if outcome == CreateAdopted && store.observer != nil &&
			(cut.mutationReportedFailure || cut.parentSyncReportedFailure) {
			cut.stage = StateInstallCreate
			cut.targetName = name
			cut.encoded = bytes.Clone(encoded)
			store.observer.ObserveStateInstall(cut)
		}
		var cleanupErr error
		if outcome != CreateUncertain {
			cleanupErr = removeExactStateTemporary(directory, temporaryName, temporary)
		}
		return outcome, errors.Join(installErr, cleanupErr, temporary.Close())
	}
	return CreateNotInstalled, fmt.Errorf("%w: allocate state creation temporary", outputcap.ErrUnsafeNamespace)
}
