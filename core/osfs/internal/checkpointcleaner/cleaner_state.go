package checkpointcleaner

import (
	"bytes"
	"crypto/sha256"
	"encoding/json"
	"slices"

	"github.com/windshare/windshare/core/osfs/internal/checkpointstore"
	"github.com/windshare/windshare/core/osfs/internal/outputcap"
	"github.com/windshare/windshare/core/osfs/internal/resumestate"
)

type cleanerState struct {
	Schema        uint8  `json:"schema"`
	Ownership     string `json:"ownership"`
	Namespace     string `json:"namespace"`
	BackendID     string `json:"backend"`
	RootIdentity  []byte `json:"rootIdentity"`
	RunGeneration uint64 `json:"runGeneration"`
	Mutations     uint64 `json:"mutations"`
	Complete      bool   `json:"complete"`
	Checksum      []byte `json:"checksum"`
}

func (run *cleanupRun) beginState() (cleanerState, []byte, bool, error) {
	state, encoded, found, err := run.loadState()
	if err != nil {
		return cleanerState{}, nil, false, err
	}
	resumed := found && !state.Complete
	if !found {
		ownership, err := resumestate.NewFileCheckpointOwnership(
			string(run.cleaner.config.BackendID), run.rootBinding.Bytes(),
		)
		if err != nil {
			return cleanerState{}, nil, false, err
		}
		state = cleanerState{
			Schema: 1, Ownership: ownership.Marker, Namespace: ownership.Namespace,
			BackendID: ownership.BackendID, RootIdentity: ownership.RootIdentity.Bytes(),
		}
	}
	if state.RunGeneration == ^uint64(0) {
		return cleanerState{}, nil, false, ErrCheckpointCleanerState
	}
	state.RunGeneration++
	state.Complete = false
	if err := run.persistState(&state, &encoded); err != nil {
		return cleanerState{}, nil, false, err
	}
	return state, encoded, resumed, nil
}

func (run *cleanupRun) loadState() (cleanerState, []byte, bool, error) {
	kind, err := run.namespace.ObserveEntry(FileCheckpointCleanupState)
	if err != nil {
		return cleanerState{}, nil, false, err
	}
	if kind == outputcap.EntryAbsent {
		return cleanerState{}, nil, false, nil
	}
	if kind != outputcap.EntryRegularFile {
		return cleanerState{}, nil, false, ErrCheckpointCleanerState
	}
	encoded, err := checkpointstore.ReadFile(run.namespace, FileCheckpointCleanupState)
	if err != nil {
		return cleanerState{}, nil, false, err
	}
	if len(encoded) > maxCleanerStateBytes {
		return cleanerState{}, nil, false, ErrCheckpointCleanerState
	}
	var state cleanerState
	if err := json.Unmarshal(encoded, &state); err != nil || !run.validState(state) {
		return cleanerState{}, nil, false, ErrCheckpointCleanerState
	}
	return state, encoded, true, nil
}

func (run *cleanupRun) validState(state cleanerState) bool {
	ownership, err := resumestate.NewFileCheckpointOwnership(
		string(run.cleaner.config.BackendID), run.rootBinding.Bytes(),
	)
	if err != nil || state.Schema != 1 || state.Ownership != ownership.Marker ||
		state.Namespace != ownership.Namespace || state.BackendID != ownership.BackendID ||
		!slices.Equal(state.RootIdentity, ownership.RootIdentity.Bytes()) || state.RunGeneration == 0 {
		return false
	}
	return len(state.Checksum) == sha256.Size && bytes.Equal(state.Checksum, stateChecksum(state))
}

func stateChecksum(state cleanerState) []byte {
	copyState := state
	copyState.Checksum = nil
	encoded, _ := json.Marshal(copyState)
	hash := sha256.Sum256(append([]byte("windshare/file-checkpoint-cleaner-state/v2\x00"), encoded...))
	return hash[:]
}

func (run *cleanupRun) persistState(state *cleanerState, previousEncoded *[]byte) error {
	if state == nil || previousEncoded == nil {
		return ErrCheckpointCleanerState
	}
	state.Checksum = stateChecksum(*state)
	next, err := json.Marshal(*state)
	if err != nil {
		return err
	}
	if len(next) > maxCleanerStateBytes {
		return ErrCheckpointCleanerLimit
	}
	if len(*previousEncoded) == 0 {
		err = checkpointstore.InstallCreate(run.namespace, FileCheckpointCleanupState, next)
	} else {
		err = checkpointstore.InstallReplace(run.namespace, FileCheckpointCleanupState, *previousEncoded, next)
	}
	if err == nil {
		*previousEncoded = append((*previousEncoded)[:0], next...)
	}
	return err
}
