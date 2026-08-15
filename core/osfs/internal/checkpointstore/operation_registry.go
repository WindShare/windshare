package checkpointstore

import (
	"crypto/sha256"
	"errors"
	"io/fs"
	"slices"

	"github.com/windshare/windshare/core/osfs/internal/checkpointmodel"
	"github.com/windshare/windshare/core/osfs/internal/destinationauthority"
	"github.com/windshare/windshare/core/osfs/internal/outputcap"
	"github.com/windshare/windshare/core/transfer"
	"github.com/windshare/windshare/core/transfer/receivecontract"
)

const (
	OrdinaryRegistryDirectoryV1 = "ordinary-v1"
	ordinaryOperationsDirectory = "operations"
	ordinaryActiveDirectory     = "active"
	ordinaryLeasesDirectory     = "leases"
	ordinaryClaimsDirectory     = "claims"
	ordinaryCandidatesDirectory = "candidates"

	ordinaryOperationRecordFile    = "record"
	ordinaryFileStateDirectory     = "files"
	ordinaryAdmissionCandidateFile = "candidate"
	ordinaryOperationLockSuffix    = ".operation.lock"
	ordinaryActiveLockSuffix       = ".active.lock"
	ordinaryClaimLockSuffix        = ".claim.lock"

	MaximumOrdinaryOperationPageSizeV1 = 256
	MaximumOrdinaryOperationRecordsV1  = 65_536
	MaximumActiveMatchesV1             = 2

	singleFileCandidateClaimAdvanceV1 = uint64(2)
	resultRootCandidateClaimAdvanceV1 = uint64(3)
)

var ordinaryRegistryEntries = map[string]outputcap.EntryKind{
	ordinaryOperationsDirectory: outputcap.EntryDirectory,
	ordinaryActiveDirectory:     outputcap.EntryDirectory,
	ordinaryLeasesDirectory:     outputcap.EntryDirectory,
	ordinaryClaimsDirectory:     outputcap.EntryDirectory,
	ordinaryCandidatesDirectory: outputcap.EntryDirectory,
}

var ordinaryOperationEntries = map[string]outputcap.EntryKind{
	ordinaryOperationRecordFile: outputcap.EntryRegularFile,
	ordinaryFileStateDirectory:  outputcap.EntryDirectory,
}

var ordinaryAdmissionCandidateEntries = map[string]outputcap.EntryKind{
	ordinaryAdmissionCandidateFile: outputcap.EntryRegularFile,
}

type ActiveLookupState uint8

const (
	ActiveLookupNone ActiveLookupState = iota + 1
	ActiveLookupReopenable
	ActiveLookupAlreadyRunning
	ActiveLookupNeedsAttention
	ActiveLookupAmbiguous
)

func (state ActiveLookupState) Valid() bool {
	return state >= ActiveLookupNone && state <= ActiveLookupAmbiguous
}

type ActiveLookup struct {
	state         ActiveLookupState
	record        checkpointmodel.OrdinaryOperationRecord
	recoveryProof ReservationRecoveryProof
	lease         *OperationRegistryLease
}

func (lookup ActiveLookup) State() ActiveLookupState { return lookup.state }
func (lookup ActiveLookup) Record() checkpointmodel.OrdinaryOperationRecord {
	return lookup.record
}
func (lookup ActiveLookup) RecoveryProof() ReservationRecoveryProof {
	return lookup.recoveryProof.Clone()
}

// ReservationRecoveryProof is the compact, exact-open proof consumed by
// destination recovery. Canonical reservation bytes remain in ReceiveIntent;
// this value contributes only authenticated private metadata and exact native
// identity evidence.
type ReservationRecoveryProof struct {
	claim              destinationauthority.ReservationClaim
	persistentIdentity []byte
}

func (proof ReservationRecoveryProof) Claim() destinationauthority.ReservationClaim {
	return proof.claim
}
func (proof ReservationRecoveryProof) PersistentIdentity() []byte {
	return slices.Clone(proof.persistentIdentity)
}
func (proof ReservationRecoveryProof) Valid() bool { return proof.claim.Valid() }
func (proof ReservationRecoveryProof) Clone() ReservationRecoveryProof {
	proof.persistentIdentity = slices.Clone(proof.persistentIdentity)
	return proof
}

// TakeLease transfers the exact operation lock only for a reopenable match.
// Returning a pointer keeps ownership explicit: closing a copied value could
// otherwise release a lease still used by the runtime.
func (lookup *ActiveLookup) TakeLease() *OperationRegistryLease {
	if lookup == nil || lookup.state != ActiveLookupReopenable {
		return nil
	}
	lease := lookup.lease
	lookup.lease = nil
	return lease
}

type OperationRegistry struct {
	root       outputcap.Directory
	operations outputcap.Directory
	active     outputcap.Directory
	leases     outputcap.Directory
	claims     outputcap.Directory
	candidates outputcap.Directory
}

// OpenOperationRegistry creates or authenticates only ordinary-v1 below the
// already-authenticated destination control directory. It never probes the
// public output tree and never reads the legacy checkpoint lookup namespace.
func OpenOperationRegistry(control outputcap.Directory) (registry OperationRegistry, resultErr error) {
	if control == nil {
		return OperationRegistry{}, transfer.ErrInvalidOutputBinding
	}
	root, err := openOrCreateDirectory(control, OrdinaryRegistryDirectoryV1)
	if err != nil {
		return OperationRegistry{}, repositoryError("open ordinary operation registry", err)
	}
	defer func() {
		if resultErr != nil {
			resultErr = errors.Join(resultErr, root.Close())
		}
	}()
	if err := validateAllowedEntries(root, ordinaryRegistryEntries); err != nil {
		return OperationRegistry{}, repositoryError("authenticate ordinary operation registry", err)
	}
	operations, err := openOrCreateDirectory(root, ordinaryOperationsDirectory)
	if err != nil {
		return OperationRegistry{}, repositoryError("open ordinary operation records", err)
	}
	active, err := openOrCreateDirectory(root, ordinaryActiveDirectory)
	if err != nil {
		return OperationRegistry{}, repositoryError("open ordinary active index", errors.Join(err, operations.Close()))
	}
	leases, err := openOrCreateDirectory(root, ordinaryLeasesDirectory)
	if err != nil {
		return OperationRegistry{}, repositoryError("open ordinary operation leases", errors.Join(err, active.Close(), operations.Close()))
	}
	claims, err := openOrCreateDirectory(root, ordinaryClaimsDirectory)
	if err != nil {
		return OperationRegistry{}, repositoryError("open ordinary reservation claims", errors.Join(err, leases.Close(), active.Close(), operations.Close()))
	}
	candidates, err := openOrCreateDirectory(root, ordinaryCandidatesDirectory)
	if err != nil {
		return OperationRegistry{}, repositoryError("open ordinary admission candidates", errors.Join(err, claims.Close(), leases.Close(), active.Close(), operations.Close()))
	}
	return OperationRegistry{root: root, operations: operations, active: active, leases: leases, claims: claims, candidates: candidates}, nil
}

func (registry *OperationRegistry) Close() error {
	if registry == nil {
		return nil
	}
	err := errors.Join(closeDirectory(registry.candidates), closeDirectory(registry.claims), closeDirectory(registry.leases),
		closeDirectory(registry.active), closeDirectory(registry.operations), closeDirectory(registry.root))
	*registry = OperationRegistry{}
	return repositoryError("close ordinary operation registry", err)
}

// ActiveAdmission holds the key lock across an active miss, shape resolution,
// reservation claiming, and active-last publication. That ordering prevents two
// processes from independently treating the same key as absent.
type ActiveAdmission struct {
	registry  *OperationRegistry
	key       checkpointmodel.ActiveOperationKey
	lock      outputcap.Lock
	candidate checkpointmodel.OrdinaryAdmissionCandidate
}

func (admission *ActiveAdmission) Close() error {
	if admission == nil {
		return nil
	}
	err := closeLock(admission.lock)
	*admission = ActiveAdmission{}
	return repositoryError("release active admission", err)
}

func (registry *OperationRegistry) BeginActive(
	key checkpointmodel.ActiveOperationKey,
) (ActiveAdmission, ActiveLookup, error) {
	if !registry.valid() || key.IsZero() {
		return ActiveAdmission{}, ActiveLookup{}, transfer.ErrInvalidOutputBinding
	}
	lock, created, err := registry.leases.AcquireLock(activeLockName(key), false)
	if err != nil {
		return ActiveAdmission{}, ActiveLookup{}, repositoryError("acquire active admission", errors.Join(err, closeLock(lock)))
	}
	if lock == nil {
		return ActiveAdmission{}, ActiveLookup{}, codedError(ErrorUnsafeInstall, "acquire active admission", outputcap.ErrUnsafeNamespace)
	}
	if created {
		if err := registry.leases.Sync(); err != nil {
			return ActiveAdmission{}, ActiveLookup{}, repositoryError("sync active admission", errors.Join(err, lock.Close()))
		}
	}
	admission := ActiveAdmission{registry: registry, key: key, lock: lock}
	lookup, err := registry.lookupActiveLocked(key)
	if err != nil {
		return ActiveAdmission{}, ActiveLookup{}, errors.Join(err, admission.Close())
	}
	if lookup.State() != ActiveLookupNone {
		return ActiveAdmission{}, lookup, admission.Close()
	}
	return admission, lookup, nil
}

// PrepareCandidate publishes the key-addressable pre-index marker before any
// public reservation mutation. A retry can therefore never mistake an
// indeterminate pre-index cut for a clean active miss.
func (admission *ActiveAdmission) PrepareCandidate(operation receivecontract.OperationID) error {
	if admission == nil || admission.registry == nil || admission.lock == nil || !admission.candidate.OperationID().IsZero() {
		return transfer.ErrInvalidOutputBinding
	}
	candidate, err := checkpointmodel.NewOrdinaryAdmissionCandidate(admission.key, operation)
	if err != nil {
		return err
	}
	directory, err := openOrCreateDirectory(admission.registry.candidates, activeKeyName(admission.key))
	if err != nil {
		return repositoryError("open ordinary admission candidate", err)
	}
	encoded, encodeErr := checkpointmodel.EncodeOrdinaryAdmissionCandidate(candidate)
	installErr := InstallCreate(directory, ordinaryAdmissionCandidateFile, encoded)
	if encodeErr != nil || installErr != nil {
		return errors.Join(codedError(ErrorCorruptRecord, "install ordinary admission candidate", encodeErr), repositoryError("install ordinary admission candidate", installErr), directory.Close())
	}
	if err := directory.Close(); err != nil {
		return repositoryError("close ordinary admission candidate", err)
	}
	admission.candidate = candidate
	return nil
}

// BeginReservation makes the held admission the consumer-facing metadata
// claimer. It links the key-addressable candidate to the exact global claim
// before returning the handle to destination authority, so BindReservation and
// every later public mutation are always reachable after a crash.
func (admission *ActiveAdmission) BeginReservation(
	spec destinationauthority.ReservationClaimSpec,
) (destinationauthority.ReservationClaimHandle, destinationauthority.ReservationMetadataClaimOutcome, error) {
	if admission == nil || admission.registry == nil || admission.lock == nil ||
		!admission.candidate.Valid() || admission.candidate.OperationID() != spec.OperationID ||
		admission.candidate.ReservationClaim().Valid() {
		return nil, 0, transfer.ErrInvalidOutputBinding
	}
	handle, outcome, err := admission.registry.BeginReservation(spec)
	if err != nil || outcome != destinationauthority.ReservationMetadataClaimCommitted || handle == nil {
		return handle, outcome, err
	}
	if bindErr := admission.BindCandidateReservation(handle.Claim()); bindErr != nil {
		return nil, destinationauthority.ReservationMetadataClaimIndeterminate,
			errors.Join(bindErr, handle.Close())
	}
	return handle, outcome, nil
}

// BindCandidateReservation records the exact claim coordinate before the
// authority performs the first public mutation. The authority-owned handle may
// subsequently advance its claim generation; the candidate retains the
// deterministic token so reconciliation never pages the global claim table.
func (admission *ActiveAdmission) BindCandidateReservation(
	claim destinationauthority.ReservationClaim,
) error {
	if admission == nil || admission.registry == nil || admission.lock == nil ||
		!admission.candidate.Valid() || !claim.Valid() || admission.candidate.ReservationClaim().Valid() {
		return transfer.ErrInvalidOutputBinding
	}
	locator, err := checkpointmodel.NewReservationClaimLocator([sha256.Size]byte(claim.Token), claim.Generation)
	if err != nil {
		return err
	}
	next, err := checkpointmodel.BindOrdinaryAdmissionReservation(admission.candidate, locator)
	if err != nil {
		return err
	}
	directory, err := openExistingDirectory(admission.registry.candidates, activeKeyName(admission.key))
	if err != nil {
		return repositoryError("open ordinary candidate reservation binding", err)
	}
	previousBytes, _ := checkpointmodel.EncodeOrdinaryAdmissionCandidate(admission.candidate)
	nextBytes, _ := checkpointmodel.EncodeOrdinaryAdmissionCandidate(next)
	replaceErr := InstallReplace(directory, ordinaryAdmissionCandidateFile, previousBytes, nextBytes)
	if replaceErr == nil {
		admission.candidate = next
	}
	return repositoryError("bind ordinary candidate reservation", errors.Join(replaceErr, directory.Close()))
}

func (admission *ActiveAdmission) RequireAttention() error {
	if admission == nil || !admission.candidate.Valid() || admission.registry == nil || admission.lock == nil {
		return transfer.ErrInvalidOutputBinding
	}
	next, err := checkpointmodel.RequireOrdinaryAdmissionAttention(admission.candidate)
	if err != nil {
		return err
	}
	directory, err := openExistingDirectory(admission.registry.candidates, activeKeyName(admission.key))
	if err != nil {
		return repositoryError("open ordinary admission attention", err)
	}
	previousBytes, _ := checkpointmodel.EncodeOrdinaryAdmissionCandidate(admission.candidate)
	nextBytes, _ := checkpointmodel.EncodeOrdinaryAdmissionCandidate(next)
	replaceErr := InstallReplace(directory, ordinaryAdmissionCandidateFile, previousBytes, nextBytes)
	if replaceErr == nil {
		admission.candidate = next
	}
	return repositoryError("persist ordinary admission attention", errors.Join(replaceErr, directory.Close()))
}

func (admission *ActiveAdmission) RollbackCandidate() error {
	if admission == nil || !admission.candidate.Valid() || admission.registry == nil || admission.lock == nil ||
		admission.candidate.State() != checkpointmodel.OrdinaryAdmissionPreparing {
		return transfer.ErrInvalidOutputBinding
	}
	err := admission.registry.removeAdmissionCandidate(admission.candidate)
	if err == nil {
		admission.candidate = checkpointmodel.OrdinaryAdmissionCandidate{}
	}
	return err
}

// LookupActive is observation-only. New-operation admission must use
// BeginActive so the miss remains serialized until active-last publication.
func (registry *OperationRegistry) LookupActive(
	key checkpointmodel.ActiveOperationKey,
) (ActiveLookup, error) {
	if !registry.valid() || key.IsZero() {
		return ActiveLookup{}, transfer.ErrInvalidOutputBinding
	}
	return registry.lookupActiveLocked(key)
}

func (registry *OperationRegistry) lookupActiveLocked(
	key checkpointmodel.ActiveOperationKey,
) (ActiveLookup, error) {
	indexDirectory, err := openExistingDirectory(registry.active, activeKeyName(key))
	if errors.Is(err, fs.ErrNotExist) {
		return registry.lookupAdmissionCandidate(key)
	}
	if err != nil {
		return ActiveLookup{}, repositoryError("open ordinary active index", err)
	}
	defer indexDirectory.Close()
	names, err := indexDirectory.Names(MaximumActiveMatchesV1 + 1)
	if err != nil {
		return ActiveLookup{state: ActiveLookupAmbiguous}, nil
	}
	if len(names) != 1 {
		if len(names) == 0 {
			return registry.lookupAdmissionCandidate(key)
		}
		return ActiveLookup{state: ActiveLookupAmbiguous}, nil
	}
	operation, parseErr := parseOperationNamespaceName(names[0])
	indexDigest, readErr := ReadFile(indexDirectory, names[0])
	if parseErr != nil || readErr != nil || len(indexDigest) != sha256.Size {
		return ActiveLookup{state: ActiveLookupAmbiguous}, nil
	}
	record, encoded, recordErr := registry.readOperation(operation)
	if recordErr != nil || record.ActiveOperationKey() != key || !record.Lifecycle().ParticipatesInActiveLookup() ||
		!validActiveIndexDigest(indexDigest, record, encoded) {
		return ActiveLookup{state: ActiveLookupAmbiguous}, nil
	}
	if record.Lifecycle() == checkpointmodel.OrdinaryOperationNeedsAttention {
		proof, proofErr := registry.recoveryProof(record)
		if proofErr != nil {
			return ActiveLookup{state: ActiveLookupAmbiguous, record: record}, nil
		}
		return ActiveLookup{state: ActiveLookupNeedsAttention, record: record, recoveryProof: proof}, nil
	}
	lease, leaseErr := registry.acquireOperationLease(operation)
	if leaseErr != nil {
		if errors.Is(leaseErr, outputcap.ErrNamespaceLockBusy) {
			return ActiveLookup{state: ActiveLookupAlreadyRunning, record: record}, nil
		}
		return ActiveLookup{state: ActiveLookupAmbiguous, record: record}, nil
	}
	current, _, readErr := registry.readOperation(operation)
	if readErr != nil || !sameOrdinaryRecord(current, record) {
		_ = lease.Close()
		return ActiveLookup{state: ActiveLookupAmbiguous, record: record}, nil
	}
	proof, proofErr := registry.recoveryProof(current)
	if proofErr != nil {
		_ = lease.Close()
		return ActiveLookup{state: ActiveLookupAmbiguous, record: record}, nil
	}
	held, transitionErr := checkpointmodel.NextOrdinaryOperationRecord(current, checkpointmodel.NextOrdinaryOperationRecordSpec{
		Lifecycle: current.Lifecycle(), Lease: checkpointmodel.OrdinaryLeaseHeld, ClosedReason: current.ClosedReason(),
	})
	if transitionErr != nil {
		_ = lease.Close()
		return ActiveLookup{state: ActiveLookupAmbiguous, record: record}, nil
	}
	if err := registry.replaceOperation(current, held); err != nil {
		_ = lease.Close()
		return ActiveLookup{state: ActiveLookupNeedsAttention, record: record, recoveryProof: proof}, nil
	}
	lease.record = held
	return ActiveLookup{state: ActiveLookupReopenable, record: held, recoveryProof: proof, lease: lease}, nil
}

func (admission *ActiveAdmission) Create(
	record checkpointmodel.OrdinaryOperationRecord,
	claim destinationauthority.ReservationClaim,
) (*OperationRegistryLease, error) {
	if admission == nil || admission.registry == nil || admission.lock == nil ||
		!record.Valid() || record.ActiveOperationKey() != admission.key ||
		record.LifecycleGeneration() != 1 || record.Lifecycle() != checkpointmodel.OrdinaryOperationActive ||
		record.Lease() != checkpointmodel.OrdinaryLeaseHeld || record.ClosedReason() != checkpointmodel.OrdinaryReasonNone ||
		!claim.Valid() {
		return nil, transfer.ErrInvalidOutputBinding
	}
	claimToken := [sha256.Size]byte(claim.Token)
	if record.ReservationClaim().Token() != claimToken ||
		record.ReservationClaim().Generation() != claim.Generation+1 {
		return nil, transfer.ErrInvalidOutputBinding
	}
	registry := admission.registry
	if !admission.candidate.Valid() || admission.candidate.OperationID() != record.OperationID() ||
		admission.candidate.State() != checkpointmodel.OrdinaryAdmissionPreparing ||
		!admission.candidate.ReservationClaim().Valid() ||
		admission.candidate.ReservationClaim().Token() != [sha256.Size]byte(claim.Token) {
		return nil, transfer.ErrInvalidOutputBinding
	}
	lease, err := registry.acquireOperationLease(record.OperationID())
	if err != nil {
		return nil, err
	}
	encoded, err := checkpointmodel.EncodeOrdinaryOperationRecord(record)
	if err != nil {
		return nil, errors.Join(codedError(ErrorCorruptRecord, "encode ordinary operation", err), lease.Close())
	}
	operationDirectory, err := openOrCreateDirectory(registry.operations, operationNamespaceName(record.OperationID()))
	if err != nil {
		return nil, errors.Join(repositoryError("open ordinary operation record", err), lease.Close())
	}
	if err := validateAllowedEntries(operationDirectory, ordinaryOperationEntries); err != nil {
		return nil, errors.Join(repositoryError("authenticate ordinary operation record", err), operationDirectory.Close(), lease.Close())
	}
	if err := InstallCreate(operationDirectory, ordinaryOperationRecordFile, encoded); err != nil {
		return nil, errors.Join(repositoryError("install ordinary operation record", err), operationDirectory.Close(), lease.Close())
	}
	if err := registry.bindClaimOperation(claim, record); err != nil {
		return nil, errors.Join(err, operationDirectory.Close(), lease.Close())
	}
	indexDirectory, err := openOrCreateDirectory(registry.active, activeKeyName(record.ActiveOperationKey()))
	if err != nil {
		return nil, errors.Join(repositoryError("open ordinary active index", err), operationDirectory.Close(), lease.Close())
	}
	// The operation row and reservation claim are durable before this final
	// publication cut. A crash earlier leaves only bounded cleanup candidates.
	bindingDigest, bindingErr := checkpointmodel.OrdinaryOperationBindingDigest(record)
	indexErr := errors.Join(bindingErr,
		InstallCreate(indexDirectory, operationNamespaceName(record.OperationID()), bindingDigest[:]))
	closeErr := errors.Join(indexDirectory.Close(), operationDirectory.Close())
	if indexErr != nil || closeErr != nil {
		return nil, errors.Join(repositoryError("publish ordinary active index", indexErr), closeErr, lease.Close())
	}
	if err := registry.removeAdmissionCandidate(admission.candidate); err != nil {
		return nil, errors.Join(err, lease.Close())
	}
	admission.candidate = checkpointmodel.OrdinaryAdmissionCandidate{}
	lease.record = record
	if err := admission.Close(); err != nil {
		return nil, errors.Join(err, lease.Close())
	}
	return lease, nil
}
