package outputruntime

import (
	"crypto/sha256"
	"sync"

	"github.com/windshare/windshare/core/osfs/internal/outputcap"
	"github.com/windshare/windshare/core/osfs/internal/resumestate"
	"github.com/windshare/windshare/core/transfer"
)

type Session struct {
	owner             *Authority
	platform          outputcap.Platform
	sessionDir        outputcap.Directory
	anchorsDir        outputcap.Directory
	stagesDir         outputcap.Directory
	checkpointsDir    outputcap.Directory
	sessionLock       outputcap.Lock
	checkpointRuntime resumestate.CheckpointRuntimeBinding
	// Lifecycle replacement mutates state; identity getters use these immutable
	// bindings so observability cannot race an authority transition.
	sessionID       transfer.OutputSessionID
	selection       transfer.OutputSelection
	intentDigest    transfer.TransferIntentDigest
	ancestry        outputAncestrySnapshot
	capabilities    transfer.OutputCapabilities
	selectedFiles   map[string]transfer.OutputSelectionFile
	selectedDirs    map[string]transfer.OutputSelectionDirectory
	admittedDirs    map[string]transfer.DirectoryAdmission
	admissionSecret [sha256.Size]byte
	// Incremental discovery has no complete selection at OpenOutput time. Its
	// in-memory authority image stays frozen while this live view is replaced
	// atomically after each committed admission.
	incrementalSelection        transfer.OutputSelection
	incrementalAdmission        bool
	incrementalIntentDigest     transfer.TransferIntentDigest
	incrementalFiles            map[string]resumestate.LiveFileSelection
	incrementalCheckpoints      map[resumestate.LiveFileKey]resumestate.FileCheckpointV1
	incrementalCheckpointByPath map[string]resumestate.FileCheckpointV1
	objectClaims                map[resumestate.OutputObjectID]resumestate.LocatorDigest

	operationGate sync.RWMutex
	stateInstall  sync.RWMutex
	mu            sync.Mutex
	poisonOnce    sync.Once
	beginWG       sync.WaitGroup
	beginning     map[resumestate.LocatorDigest]struct{}
	active        map[resumestate.LocatorDigest]*FileTransaction
	attention     []ResumeAttention
	settling      bool
	poisoned      bool
	exposed       bool
	closed        bool
}

func closeOutputDirectory(directory outputcap.Directory) error {
	if directory == nil {
		return nil
	}
	return directory.Close()
}

func closeOutputLock(lock outputcap.Lock) error {
	if lock == nil {
		return nil
	}
	return lock.Close()
}
