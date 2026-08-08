package checkpointstore

import (
	"encoding/hex"
	"errors"
	"io/fs"

	"github.com/windshare/windshare/core/osfs/internal/checkpointmodel"
	"github.com/windshare/windshare/core/osfs/internal/outputcap"
	"github.com/windshare/windshare/core/transfer"
)

const (
	ControlDirectory       = ".windshare-output"
	CheckpointDirectory    = "checkpoints-v1"
	LeasesDirectory        = "leases"
	AnchorsDirectory       = "anchors"
	StagesDirectory        = "stages"
	LegacyCleanupStateFile = "cleanup.state"
	LegacyCleanupLockFile  = "cleanup.lock"

	intentLockSuffix = ".runtime.lock"
)

var checkpointRootEntries = map[string]outputcap.EntryKind{
	OwnershipFile:          outputcap.EntryRegularFile,
	LeasesDirectory:        outputcap.EntryDirectory,
	IntentsDirectory:       outputcap.EntryDirectory,
	LegacyCleanupStateFile: outputcap.EntryRegularFile,
	LegacyCleanupLockFile:  outputcap.EntryRegularFile,
}

var intentEntries = map[string]outputcap.EntryKind{
	RecordsDirectory: outputcap.EntryDirectory,
	AnchorsDirectory: outputcap.EntryDirectory,
	StagesDirectory:  outputcap.EntryDirectory,
}

// CertifiedConfig carries the already-proved native root binding. The
// repository never derives ownership from a pathname or current path existence.
type CertifiedConfig struct {
	Root      outputcap.Directory
	Ownership checkpointmodel.Ownership
}

// Namespace is the fixed current-state scaffold. It contains no intent lease;
// listing this value is therefore read-only with respect to every intent.
type Namespace struct {
	checkpointRoot outputcap.Directory
	intents        outputcap.Directory
	leases         outputcap.Directory
	ownership      checkpointmodel.Ownership
}

// AdoptPinnedNamespace transfers already-pinned fixed-layout directories into
// the one namespace/lease implementation. Resume inspection establishes the
// native pins; this constructor deliberately grants no path or name selection.
func AdoptPinnedNamespace(
	checkpointRoot outputcap.Directory,
	intents outputcap.Directory,
	leases outputcap.Directory,
	ownership checkpointmodel.Ownership,
) (Namespace, error) {
	if checkpointRoot == nil || intents == nil || leases == nil || !ownership.Valid() {
		return Namespace{}, transfer.ErrInvalidOutputBinding
	}
	return Namespace{
		checkpointRoot: checkpointRoot,
		intents:        intents,
		leases:         leases,
		ownership:      ownership,
	}, nil
}

// CheckpointRootEntryKind exposes the fixed allowlist without exporting its
// mutable map. Native inspectors can classify state while checkpointstore
// remains the sole layout authority.
func CheckpointRootEntryKind(name string) (outputcap.EntryKind, bool) {
	kind, known := checkpointRootEntries[name]
	return kind, known
}

func CheckpointRootEntryLimit() int { return len(checkpointRootEntries) }

func IntentEntryKind(name string) (outputcap.EntryKind, bool) {
	kind, known := intentEntries[name]
	return kind, known
}

func IntentEntryLimit() int { return len(intentEntries) }

// Initialize certifies ownership and installs only the fixed root scaffold. It
// deliberately creates no intent child, so every intent mutation remains behind
// AcquireIntent's exclusive lease.
func Initialize(config CertifiedConfig) (result Namespace, resultErr error) {
	if err := validateCertifiedConfig(config); err != nil {
		return Namespace{}, err
	}
	control, err := openOrCreateDirectory(config.Root, ControlDirectory)
	if err != nil {
		return Namespace{}, repositoryError("initialize control directory", err)
	}
	checkpointRoot, err := openOrCreateDirectory(control, CheckpointDirectory)
	if err != nil {
		return Namespace{}, repositoryError("initialize checkpoint directory", errors.Join(err, control.Close()))
	}
	if err := control.Close(); err != nil {
		return Namespace{}, repositoryError("close initialized control directory", errors.Join(err, checkpointRoot.Close()))
	}
	defer func() {
		if resultErr != nil {
			resultErr = errors.Join(resultErr, checkpointRoot.Close())
		}
	}()
	if err := ensureOwnership(checkpointRoot, config.Ownership, true); err != nil {
		return Namespace{}, repositoryError("certify checkpoint ownership", err)
	}
	encoded, err := checkpointmodel.EncodeOwnership(config.Ownership)
	if err != nil {
		return Namespace{}, codedError(ErrorOwnershipMismatch, "encode checkpoint ownership", err)
	}
	if err := reconcileExactCandidates(checkpointRoot, OwnershipFile, encoded); err != nil {
		return Namespace{}, repositoryError("reconcile checkpoint ownership", err)
	}
	if err := validateAllowedEntries(checkpointRoot, checkpointRootEntries); err != nil {
		return Namespace{}, repositoryError("validate checkpoint layout", err)
	}
	leases, err := openOrCreateDirectory(checkpointRoot, LeasesDirectory)
	if err != nil {
		return Namespace{}, repositoryError("initialize checkpoint leases", err)
	}
	intents, err := openOrCreateDirectory(checkpointRoot, IntentsDirectory)
	if err != nil {
		return Namespace{}, repositoryError("initialize checkpoint intents", errors.Join(err, leases.Close()))
	}
	return Namespace{
		checkpointRoot: checkpointRoot,
		intents:        intents,
		leases:         leases,
		ownership:      config.Ownership,
	}, nil
}

// OpenNamespace reopens a previously initialized scaffold without reconciling or
// creating anything. Read-only resume inventory code uses this path.
func OpenNamespace(config CertifiedConfig) (result Namespace, resultErr error) {
	if err := validateCertifiedConfig(config); err != nil {
		return Namespace{}, err
	}
	control, err := openExistingDirectory(config.Root, ControlDirectory)
	if err != nil {
		return Namespace{}, repositoryError("open control directory", err)
	}
	checkpointRoot, err := openExistingDirectory(control, CheckpointDirectory)
	if err != nil {
		return Namespace{}, repositoryError("open checkpoint directory", errors.Join(err, control.Close()))
	}
	if err := control.Close(); err != nil {
		return Namespace{}, repositoryError("close control directory", errors.Join(err, checkpointRoot.Close()))
	}
	defer func() {
		if resultErr != nil {
			resultErr = errors.Join(resultErr, checkpointRoot.Close())
		}
	}()
	status, err := inspectOwnership(checkpointRoot, config.Ownership)
	if err != nil {
		return Namespace{}, repositoryError("verify checkpoint ownership", err)
	}
	if status != OwnershipMatched {
		return Namespace{}, codedError(ErrorOwnershipMismatch, "verify checkpoint ownership", checkpointmodel.ErrInvalidOwnership)
	}
	if err := validateAllowedEntries(checkpointRoot, checkpointRootEntries); err != nil {
		return Namespace{}, repositoryError("validate checkpoint layout", err)
	}
	leases, err := openExistingDirectory(checkpointRoot, LeasesDirectory)
	if err != nil {
		return Namespace{}, repositoryError("open checkpoint leases", err)
	}
	intents, err := openExistingDirectory(checkpointRoot, IntentsDirectory)
	if err != nil {
		return Namespace{}, repositoryError("open checkpoint intents", errors.Join(err, leases.Close()))
	}
	return Namespace{
		checkpointRoot: checkpointRoot,
		intents:        intents,
		leases:         leases,
		ownership:      config.Ownership,
	}, nil
}

func (namespace *Namespace) Close() error {
	if namespace == nil {
		return nil
	}
	err := errors.Join(
		closeDirectory(namespace.intents),
		closeDirectory(namespace.leases),
		closeDirectory(namespace.checkpointRoot),
	)
	*namespace = Namespace{}
	return repositoryError("close checkpoint namespace", err)
}

// IntentLease owns the only authority allowed to inspect or mutate one intent
// subtree. The duplicated intents handle is retained so its lifetime cannot be
// shortened by closing the parent Namespace.
type IntentLease struct {
	intent  transfer.TransferIntentDigest
	binding checkpointmodel.Binding
	intents outputcap.Directory
	lock    outputcap.Lock
}

func (lease IntentLease) Binding() checkpointmodel.Binding { return lease.binding }

func (namespace *Namespace) AcquireIntent(intent transfer.TransferIntentDigest) (IntentLease, error) {
	if namespace == nil || namespace.leases == nil || namespace.intents == nil || intent.IsZero() {
		return IntentLease{}, transfer.ErrInvalidOutputBinding
	}
	binding, err := checkpointmodel.NewBinding(namespace.ownership, intent)
	if err != nil {
		return IntentLease{}, transfer.ErrInvalidOutputBinding
	}
	lock, created, err := namespace.leases.AcquireLock(intentLeaseName(intent), false)
	if err != nil {
		return IntentLease{}, repositoryError("acquire intent lease", errors.Join(err, closeLock(lock)))
	}
	if lock == nil {
		return IntentLease{}, codedError(ErrorUnsafeInstall, "acquire intent lease", outputcap.ErrUnsafeNamespace)
	}
	if created {
		if err := namespace.leases.Sync(); err != nil {
			return IntentLease{}, repositoryError("sync intent lease", errors.Join(err, lock.Close()))
		}
	}
	intents, err := namespace.intents.Duplicate()
	if err != nil {
		return IntentLease{}, repositoryError("retain intent namespace", errors.Join(err, lock.Close()))
	}
	if intents == nil {
		return IntentLease{}, codedError(ErrorUnsafeInstall, "retain intent namespace",
			errors.Join(outputcap.ErrUnsafeNamespace, lock.Close()))
	}
	return IntentLease{intent: intent, binding: binding, intents: intents, lock: lock}, nil
}

func (lease *IntentLease) Close() error {
	if lease == nil {
		return nil
	}
	// The lock is released last so no second owner can observe half-closed
	// repository capabilities from the first owner.
	err := errors.Join(closeDirectory(lease.intents), closeLock(lease.lock))
	*lease = IntentLease{}
	return repositoryError("release intent lease", err)
}

func (lease *IntentLease) OpenOrCreateRepository() (Repository, error) {
	return lease.openRepository(true)
}

func (lease *IntentLease) OpenExistingRepository() (Repository, error) {
	return lease.openRepository(false)
}

func (lease *IntentLease) openRepository(create bool) (result Repository, resultErr error) {
	if lease == nil || lease.intents == nil || lease.lock == nil || lease.intent.IsZero() {
		return Repository{}, transfer.ErrInvalidOutputBinding
	}
	intentName := intentNamespaceName(lease.intent)
	var intent outputcap.Directory
	var err error
	if create {
		intent, err = openOrCreateDirectory(lease.intents, intentName)
	} else {
		intent, err = openExistingDirectory(lease.intents, intentName)
	}
	if err != nil {
		return Repository{}, repositoryError("open leased intent", err)
	}
	defer func() {
		if resultErr != nil {
			resultErr = errors.Join(resultErr, intent.Close())
		}
	}()
	if err := validateAllowedEntries(intent, intentEntries); err != nil {
		return Repository{}, repositoryError("validate intent layout", err)
	}
	open := openExistingDirectory
	if create {
		open = openOrCreateDirectory
	}
	records, err := open(intent, RecordsDirectory)
	if err != nil {
		return Repository{}, repositoryError("open checkpoint records", err)
	}
	anchors, err := open(intent, AnchorsDirectory)
	if err != nil {
		return Repository{}, repositoryError("open checkpoint anchors", errors.Join(err, records.Close()))
	}
	stages, err := open(intent, StagesDirectory)
	if err != nil {
		return Repository{}, repositoryError("open checkpoint stages", errors.Join(err, anchors.Close(), records.Close()))
	}
	return Repository{
		intent: intent, records: records, anchors: anchors, stages: stages, binding: lease.binding,
	}, nil
}

func validateCertifiedConfig(config CertifiedConfig) error {
	if config.Root == nil || !config.Ownership.Valid() {
		return transfer.ErrInvalidOutputBinding
	}
	return nil
}

func intentNamespaceName(intent transfer.TransferIntentDigest) string {
	return hex.EncodeToString(intent.Bytes())
}

func intentLeaseName(intent transfer.TransferIntentDigest) string {
	return intentNamespaceName(intent) + intentLockSuffix
}

func openExistingDirectory(parent outputcap.Directory, name string) (outputcap.Directory, error) {
	if parent == nil || name == "" {
		return nil, transfer.ErrInvalidOutputBinding
	}
	kind, exact, err := parent.ClassifyExactEntry(name)
	if err != nil {
		return nil, err
	}
	if kind == outputcap.EntryAbsent {
		return nil, fs.ErrNotExist
	}
	if !exact || kind != outputcap.EntryDirectory {
		return nil, outputcap.ErrUnsafeNamespace
	}
	opened, err := parent.OpenDirectory(name, true)
	if err != nil {
		return nil, errors.Join(err, closeDirectory(opened))
	}
	if opened == nil {
		return nil, outputcap.ErrUnsafeNamespace
	}
	return opened, nil
}

func validateAllowedEntries(directory outputcap.Directory, allowed map[string]outputcap.EntryKind) error {
	if directory == nil {
		return transfer.ErrInvalidOutputBinding
	}
	names, err := directory.Names(len(allowed) + 1)
	if err != nil {
		return err
	}
	if len(names) > len(allowed) {
		return outputcap.ErrUnsafeNamespace
	}
	for _, name := range names {
		expected, known := allowed[name]
		if !known {
			return outputcap.ErrUnsafeNamespace
		}
		kind, exact, err := directory.ClassifyExactEntry(name)
		if err != nil {
			return err
		}
		if !exact || kind != expected {
			return outputcap.ErrUnsafeNamespace
		}
	}
	return nil
}
