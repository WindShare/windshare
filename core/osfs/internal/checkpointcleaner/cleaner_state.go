package checkpointcleaner

import (
	"bytes"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"io/fs"

	"github.com/windshare/windshare/core/osfs/internal/checkpointstore"
	"github.com/windshare/windshare/core/osfs/internal/legacyresume"
	"github.com/windshare/windshare/core/osfs/internal/outputcap"
)

const cleanerStateSchema = uint8(1)

type cleanerState struct {
	Schema        uint8  `json:"schema"`
	BackendID     string `json:"backend"`
	Certification string `json:"certification"`
	RootIdentity  []byte `json:"rootIdentity"`
	Durability    uint8  `json:"durability"`
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
		state = cleanerState{
			Schema: cleanerStateSchema, BackendID: string(run.cleaner.config.BackendID),
			Certification: run.certification, RootIdentity: append([]byte(nil), run.rootBinding...),
			Durability: uint8(run.durability),
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
	if run.namespace == nil {
		return cleanerState{}, nil, false, nil
	}
	kind, exact, err := run.namespace.ClassifyExactEntry(FileCheckpointCleanupState)
	if err != nil {
		return cleanerState{}, nil, false, err
	}
	if kind == outputcap.EntryAbsent {
		return cleanerState{}, nil, false, nil
	}
	if !exact || kind != outputcap.EntryRegularFile {
		return cleanerState{}, nil, false, ErrCheckpointCleanerState
	}
	encoded, err := checkpointstore.ReadFile(run.namespace, FileCheckpointCleanupState)
	if errors.Is(err, fs.ErrNotExist) {
		return cleanerState{}, nil, false, ErrCheckpointCleanerState
	}
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
	canonical, err := json.Marshal(state)
	if err != nil || !bytes.Equal(canonical, encoded) {
		return cleanerState{}, nil, false, ErrCheckpointCleanerState
	}
	return state, encoded, true, nil
}

func (run *cleanupRun) validState(state cleanerState) bool {
	expected := legacyresume.ExpectedOwnership{
		Backend: run.cleaner.config.BackendID, RootIdentity: run.rootBinding,
		Certification: run.certification, Durability: run.durability,
	}
	if legacyresume.ValidateExpectedOwnership(expected) != nil ||
		state.Schema != cleanerStateSchema || state.BackendID != string(expected.Backend) ||
		state.Certification != expected.Certification ||
		!bytes.Equal(state.RootIdentity, expected.RootIdentity) ||
		state.Durability != uint8(expected.Durability) || state.RunGeneration == 0 {
		return false
	}
	return len(state.Checksum) == sha256.Size && bytes.Equal(state.Checksum, stateChecksum(state))
}

func stateChecksum(state cleanerState) []byte {
	copyState := state
	copyState.Checksum = nil
	encoded, _ := json.Marshal(copyState)
	hash := sha256.Sum256(append([]byte("windshare/legacy-resume-cleaner-state/v1\x00"), encoded...))
	return hash[:]
}

func (run *cleanupRun) persistState(state *cleanerState, previousEncoded *[]byte) error {
	if state == nil || previousEncoded == nil || run.namespace == nil || run.cleanupLock == nil {
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
