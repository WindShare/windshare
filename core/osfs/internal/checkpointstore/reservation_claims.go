package checkpointstore

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io/fs"
	"slices"

	"github.com/windshare/windshare/core/osfs/internal/checkpointmodel"
	"github.com/windshare/windshare/core/osfs/internal/destinationauthority"
	"github.com/windshare/windshare/core/osfs/internal/outputcap"
	"github.com/windshare/windshare/core/transfer"
	"github.com/windshare/windshare/core/transfer/receivecontract"
)

type ReservationClaimHandle struct {
	registry *OperationRegistry
	record   checkpointmodel.ReservationClaimRecord
	lock     outputcap.Lock
	closed   bool
}

func (handle *ReservationClaimHandle) Claim() destinationauthority.ReservationClaim {
	if handle == nil || handle.closed || !handle.record.Valid() {
		return destinationauthority.ReservationClaim{}
	}
	return destinationauthority.ReservationClaim{
		Token: destinationauthority.ReservationClaimToken(handle.record.Token()), Generation: handle.record.Generation(),
	}
}

// BeginReservation durably claims the platform-canonical top-level name before
// the public absence probe or directory creation. A collision is decided solely
// by the authenticated private claim table; public names are never used as locks.
func (registry *OperationRegistry) BeginReservation(
	spec destinationauthority.ReservationClaimSpec,
) (destinationauthority.ReservationClaimHandle, destinationauthority.ReservationMetadataClaimOutcome, error) {
	if !registry.valid() {
		return nil, 0, transfer.ErrInvalidOutputBinding
	}
	record, err := checkpointmodel.NewReservationClaimRecord(checkpointmodel.ReservationClaimRecordSpec{
		CanonicalNameKey: spec.CanonicalNameKey, OperationID: spec.OperationID,
		ReservationID: spec.ReservationID, RequestedName: spec.RequestedName,
		ReservedName: spec.ReservedName, EntryKind: spec.EntryKind,
		CollisionIndex: spec.CollisionIndex, Generation: 1, Phase: checkpointmodel.ReservationClaimed,
	})
	if err != nil {
		return nil, 0, transfer.ErrInvalidOutputBinding
	}
	token := record.Token()
	lock, created, err := registry.leases.AcquireLock(claimLockName(token), false)
	if err != nil {
		return nil, destinationauthority.ReservationMetadataClaimIndeterminate,
			repositoryError("acquire reservation claim", errors.Join(err, closeLock(lock)))
	}
	if lock == nil {
		return nil, destinationauthority.ReservationMetadataClaimIndeterminate,
			codedError(ErrorUnsafeInstall, "acquire reservation claim", outputcap.ErrUnsafeNamespace)
	}
	if created {
		if err := registry.leases.Sync(); err != nil {
			return nil, destinationauthority.ReservationMetadataClaimIndeterminate,
				repositoryError("sync reservation claim lock", errors.Join(err, lock.Close()))
		}
	}
	name := reservationClaimName(spec.CanonicalNameKey)
	existingBytes, readErr := ReadFile(registry.claims, name)
	switch {
	case readErr == nil:
		existing, decodeErr := checkpointmodel.DecodeReservationClaimRecord(existingBytes)
		if decodeErr != nil {
			return nil, destinationauthority.ReservationMetadataClaimIndeterminate,
				errors.Join(codedError(ErrorCorruptRecord, "decode reservation claim", decodeErr), lock.Close())
		}
		if !checkpointmodel.SameReservationClaim(existing, record) {
			return nil, destinationauthority.ReservationMetadataClaimCollision, lock.Close()
		}
		return &ReservationClaimHandle{registry: registry, record: existing, lock: lock},
			destinationauthority.ReservationMetadataClaimCommitted, nil
	case !errors.Is(readErr, fs.ErrNotExist):
		return nil, destinationauthority.ReservationMetadataClaimIndeterminate,
			errors.Join(repositoryError("read reservation claim", readErr), lock.Close())
	}
	encoded, encodeErr := checkpointmodel.EncodeReservationClaimRecord(record)
	if encodeErr != nil {
		return nil, destinationauthority.ReservationMetadataClaimIndeterminate,
			errors.Join(codedError(ErrorCorruptRecord, "encode reservation claim", encodeErr), lock.Close())
	}
	if err := InstallCreate(registry.claims, name, encoded); err != nil {
		return nil, destinationauthority.ReservationMetadataClaimIndeterminate,
			errors.Join(repositoryError("install reservation claim", err), lock.Close())
	}
	return &ReservationClaimHandle{registry: registry, record: record, lock: lock},
		destinationauthority.ReservationMetadataClaimCommitted, nil
}

func (handle *ReservationClaimHandle) BindReservation(
	reservation receivecontract.DestinationReservation,
) (destinationauthority.ReservationMetadataClaimOutcome, error) {
	if !handle.valid() || reservation.IsZero() || reservation.ID() != handle.record.ReservationID() ||
		reservation.OperationID() != handle.record.OperationID() ||
		reservation.RequestedName() != handle.record.RequestedName() ||
		reservation.ReservedName() != handle.record.ReservedName() ||
		reservation.EntryKind() != handle.record.EntryKind() ||
		reservation.CollisionIndex() != handle.record.CollisionIndex() {
		return 0, transfer.ErrInvalidOutputBinding
	}
	next, err := checkpointmodel.BindReservationClaim(handle.record, reservation.Digest())
	if err != nil {
		return 0, err
	}
	return handle.replace(next)
}

func (handle *ReservationClaimHandle) BindDirectoryIdentity(
	persistentIdentity []byte,
) (destinationauthority.ReservationMetadataClaimOutcome, error) {
	if !handle.valid() {
		return 0, transfer.ErrInvalidOutputBinding
	}
	next, err := checkpointmodel.BindReservationDirectory(handle.record, persistentIdentity)
	if err != nil {
		return 0, err
	}
	return handle.replace(next)
}

// Rollback is allowed only before a public result can exist. Later phases need
// exact cleanup evidence and therefore remain durable for reconciliation.
func (handle *ReservationClaimHandle) Rollback() (destinationauthority.ReservationMetadataClaimOutcome, error) {
	if !handle.valid() || (handle.record.Phase() != checkpointmodel.ReservationClaimed &&
		handle.record.Phase() != checkpointmodel.ReservationBindingBound) {
		return destinationauthority.ReservationMetadataClaimIndeterminate, transfer.ErrInvalidOutputBinding
	}
	encoded, err := checkpointmodel.EncodeReservationClaimRecord(handle.record)
	if err != nil {
		return destinationauthority.ReservationMetadataClaimIndeterminate, err
	}
	removeErr, closeErr := RemoveExact(handle.registry.claims,
		reservationClaimName(handle.record.CanonicalNameKey()), encoded)
	if removeErr != nil || closeErr != nil {
		return destinationauthority.ReservationMetadataClaimIndeterminate,
			repositoryError("rollback reservation claim", errors.Join(removeErr, closeErr))
	}
	handle.record = checkpointmodel.ReservationClaimRecord{}
	return destinationauthority.ReservationMetadataClaimCommitted, nil
}

func (handle *ReservationClaimHandle) Close() error {
	if handle == nil || handle.closed {
		return nil
	}
	handle.closed = true
	err := closeLock(handle.lock)
	handle.lock = nil
	return repositoryError("close reservation claim", err)
}

func (handle *ReservationClaimHandle) replace(
	next checkpointmodel.ReservationClaimRecord,
) (destinationauthority.ReservationMetadataClaimOutcome, error) {
	previousBytes, previousErr := checkpointmodel.EncodeReservationClaimRecord(handle.record)
	nextBytes, nextErr := checkpointmodel.EncodeReservationClaimRecord(next)
	if previousErr != nil || nextErr != nil {
		return destinationauthority.ReservationMetadataClaimIndeterminate,
			codedError(ErrorCorruptRecord, "encode reservation claim replacement", errors.Join(previousErr, nextErr))
	}
	err := InstallReplace(handle.registry.claims, reservationClaimName(handle.record.CanonicalNameKey()), previousBytes, nextBytes)
	if err != nil {
		return destinationauthority.ReservationMetadataClaimIndeterminate,
			repositoryError("replace reservation claim", err)
	}
	handle.record = next
	return destinationauthority.ReservationMetadataClaimCommitted, nil
}

func (handle *ReservationClaimHandle) valid() bool {
	return handle != nil && !handle.closed && handle.registry != nil && handle.lock != nil && handle.record.Valid()
}

func (registry *OperationRegistry) bindClaimOperation(
	claim destinationauthority.ReservationClaim,
	record checkpointmodel.OrdinaryOperationRecord,
) error {
	if !registry.valid() || !claim.Valid() || !record.Valid() {
		return transfer.ErrInvalidOutputBinding
	}
	token := [sha256.Size]byte(claim.Token)
	lock, _, err := registry.leases.AcquireLock(claimLockName(token), false)
	if err != nil {
		return repositoryError("acquire operation reservation claim", errors.Join(err, closeLock(lock)))
	}
	if lock == nil {
		return codedError(ErrorUnsafeInstall, "acquire operation reservation claim", outputcap.ErrUnsafeNamespace)
	}
	defer lock.Close()
	current, name, err := registry.readClaimByToken(token, claim.Generation)
	if err != nil {
		return err
	}
	if !current.Valid() || current.Generation() != claim.Generation || current.OperationID() != record.OperationID() {
		return checkpointmodel.ErrInvalidReservationClaim
	}
	intent, err := record.VerifyIntent(transfer.DecodeReceiveIntent)
	if err != nil {
		return err
	}
	reservation, direct := intent.MaterializationPlan().DestinationReservation()
	if !direct || reservation.Digest() != current.ReservationDigest() || reservation.ID() != current.ReservationID() {
		return checkpointmodel.ErrInvalidReservationClaim
	}
	bindingDigest, err := checkpointmodel.OrdinaryOperationBindingDigest(record)
	if err != nil {
		return err
	}
	next, err := checkpointmodel.BindReservationOperation(current, bindingDigest)
	if err != nil {
		return err
	}
	if next.Token() != record.ReservationClaim().Token() || next.Generation() != record.ReservationClaim().Generation() {
		return checkpointmodel.ErrInvalidReservationClaim
	}
	previousBytes, _ := checkpointmodel.EncodeReservationClaimRecord(current)
	nextBytes, _ := checkpointmodel.EncodeReservationClaimRecord(next)
	return repositoryError("bind reservation claim to operation", InstallReplace(registry.claims, name, previousBytes, nextBytes))
}

func (registry *OperationRegistry) recoveryProof(
	record checkpointmodel.OrdinaryOperationRecord,
) (ReservationRecoveryProof, error) {
	locator := record.ReservationClaim()
	claimRecord, _, err := registry.readClaimByToken(locator.Token(), locator.Generation())
	if err != nil {
		return ReservationRecoveryProof{}, err
	}
	intent, err := record.VerifyIntent(transfer.DecodeReceiveIntent)
	if err != nil {
		return ReservationRecoveryProof{}, err
	}
	reservation, direct := intent.MaterializationPlan().DestinationReservation()
	bindingDigest, digestErr := checkpointmodel.OrdinaryOperationBindingDigest(record)
	if !direct || digestErr != nil || claimRecord.Phase() != checkpointmodel.ReservationOperationBound ||
		claimRecord.OperationID() != record.OperationID() || claimRecord.ReservationID() != reservation.ID() ||
		claimRecord.ReservationDigest() != reservation.Digest() || claimRecord.RequestedName() != reservation.RequestedName() ||
		claimRecord.ReservedName() != reservation.ReservedName() || claimRecord.EntryKind() != reservation.EntryKind() ||
		claimRecord.CollisionIndex() != reservation.CollisionIndex() || claimRecord.OperationBindingDigest() != bindingDigest {
		return ReservationRecoveryProof{}, checkpointmodel.ErrInvalidReservationClaim
	}
	return ReservationRecoveryProof{
		claim: destinationauthority.ReservationClaim{
			Token: destinationauthority.ReservationClaimToken(locator.Token()), Generation: locator.Generation(),
		},
		persistentIdentity: claimRecord.PersistentIdentity(),
	}, nil
}

func (registry *OperationRegistry) ReleaseReservation(
	claim destinationauthority.ReservationClaim,
	expectedOperation receivecontract.OperationID,
) error {
	if !registry.valid() || !claim.Valid() || expectedOperation.IsZero() {
		return transfer.ErrInvalidOutputBinding
	}
	token := [sha256.Size]byte(claim.Token)
	lock, _, err := registry.leases.AcquireLock(claimLockName(token), false)
	if err != nil {
		return repositoryError("acquire reservation release", errors.Join(err, closeLock(lock)))
	}
	if lock == nil {
		return codedError(ErrorUnsafeInstall, "acquire reservation release", outputcap.ErrUnsafeNamespace)
	}
	defer lock.Close()
	record, name, err := registry.readClaimByToken(token, claim.Generation)
	if err != nil {
		return err
	}
	if record.OperationID() != expectedOperation || record.Phase() != checkpointmodel.ReservationOperationBound {
		return checkpointmodel.ErrInvalidReservationClaim
	}
	encoded, _ := checkpointmodel.EncodeReservationClaimRecord(record)
	removeErr, closeErr := RemoveExact(registry.claims, name, encoded)
	return repositoryError("release reservation claim", errors.Join(removeErr, closeErr))
}

func (registry *OperationRegistry) readClaimByToken(
	token [sha256.Size]byte,
	generation uint64,
) (checkpointmodel.ReservationClaimRecord, string, error) {
	if registry.claims == nil || token == ([sha256.Size]byte{}) || generation == 0 {
		return checkpointmodel.ReservationClaimRecord{}, "", transfer.ErrInvalidOutputBinding
	}
	name := hex.EncodeToString(token[:])
	encoded, readErr := ReadFile(registry.claims, name)
	if readErr != nil {
		return checkpointmodel.ReservationClaimRecord{}, "", repositoryError("read reservation claim token", readErr)
	}
	record, decodeErr := checkpointmodel.ReservationClaimRecordFromCanonicalBytes(encoded, token, generation)
	if decodeErr != nil {
		return checkpointmodel.ReservationClaimRecord{}, "", codedError(ErrorCorruptRecord, "decode reservation claim token", decodeErr)
	}
	return record, name, nil
}

type ReservationClaimPage struct {
	records []checkpointmodel.ReservationClaimRecord
	next    ReservationClaimPageCursor
	unknown bool
}

type ReservationClaimPageCursor struct {
	after string
}

func NewReservationClaimPageCursor(token [sha256.Size]byte) ReservationClaimPageCursor {
	if token == ([sha256.Size]byte{}) {
		return ReservationClaimPageCursor{}
	}
	return ReservationClaimPageCursor{after: hex.EncodeToString(token[:])}
}

func (cursor ReservationClaimPageCursor) IsZero() bool { return cursor.after == "" }

func (page ReservationClaimPage) Records() []checkpointmodel.ReservationClaimRecord {
	return slices.Clone(page.records)
}
func (page ReservationClaimPage) Unknown() bool { return page.unknown }
func (page ReservationClaimPage) Next() ReservationClaimPageCursor {
	if len(page.records) == 0 {
		return ReservationClaimPageCursor{}
	}
	return page.next
}

// PageReservationClaims is an explicit cleanup/inventory path. Active lookup
// never calls it, so fresh admission cost is independent of stale claim count.
func (registry *OperationRegistry) PageReservationClaims(
	cursor ReservationClaimPageCursor,
	maximum int,
) (ReservationClaimPage, error) {
	if !registry.valid() || maximum <= 0 || maximum > MaximumOrdinaryOperationPageSizeV1 {
		return ReservationClaimPage{}, transfer.ErrInvalidOutputBinding
	}
	upperBound := checkpointmodel.MaxCheckpointRecordsPerOperation + 1
	names, err := registry.claims.Names(upperBound)
	if err != nil {
		if errors.Is(err, outputcap.ErrUnsafeNamespace) {
			return ReservationClaimPage{unknown: true}, nil
		}
		return ReservationClaimPage{}, repositoryError("page reservation claims", err)
	}
	slices.Sort(names)
	start := 0
	if cursor.after != "" {
		start, _ = slices.BinarySearch(names, cursor.after)
		for start < len(names) && names[start] <= cursor.after {
			start++
		}
	}
	page := ReservationClaimPage{unknown: len(names)-start > maximum}
	end := min(start+maximum, len(names))
	for _, name := range names[start:end] {
		encoded, readErr := ReadFile(registry.claims, name)
		record, decodeErr := checkpointmodel.DecodeReservationClaimRecord(encoded)
		if readErr != nil || decodeErr != nil || name != reservationClaimName(record.CanonicalNameKey()) {
			page.unknown = true
			continue
		}
		page.records = append(page.records, record)
	}
	if end < len(names) {
		page.next = ReservationClaimPageCursor{after: names[end-1]}
	}
	return page, nil
}

func reservationClaimName(canonicalNameKey string) string {
	token, _ := checkpointmodel.ReservationClaimTokenForCanonicalNameKey(canonicalNameKey)
	return hex.EncodeToString(token[:])
}
