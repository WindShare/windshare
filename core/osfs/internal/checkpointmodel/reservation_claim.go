package checkpointmodel

import (
	"bytes"
	"crypto/sha256"
	"encoding/binary"
	"errors"
	"fmt"
	"slices"
	"unicode/utf8"

	"github.com/windshare/windshare/core/transfer/receivecontract"
)

const (
	ReservationClaimRecordVersionV2       = uint8(2)
	ReservationClaimRecordDomainV2        = "windshare/reservation-claim/v2"
	ReservationClaimTokenDomainV2         = "windshare/reservation-claim-token/v2"
	MaximumReservationClaimRecordBytesV2  = 16 * 1024
	MaximumReservationNameBytesV2         = 4 * 1024
	MaximumCanonicalNameKeyBytesV2        = 4 * 1024
	MaximumPersistentIdentityClaimBytesV2 = 4 * 1024
)

var ErrInvalidReservationClaim = errors.New("reservation metadata claim is invalid")

type ReservationClaimPhase uint8

const (
	ReservationClaimed ReservationClaimPhase = iota + 1
	ReservationBindingBound
	ReservationDirectoryBound
	ReservationOperationBound
)

func (phase ReservationClaimPhase) Valid() bool {
	return phase >= ReservationClaimed && phase <= ReservationOperationBound
}

type ReservationClaimRecordSpec struct {
	CanonicalNameKey       string
	OperationID            receivecontract.OperationID
	ReservationID          receivecontract.DestinationReservationID
	RequestedName          string
	LogicalReservedName    string
	PhysicalName           string
	EntryKind              receivecontract.ContainerEntryKind
	CollisionIndex         uint32
	Generation             uint64
	Phase                  ReservationClaimPhase
	ReservationDigest      receivecontract.BindingDigest
	PersistentIdentity     []byte
	OperationBindingDigest [sha256.Size]byte
}

// ReservationClaimRecord is root-private name ownership, not public path
// authority. The canonical key is used only to serialize claims that the native
// filesystem considers equal; mutations still require retained destination
// handles.
type ReservationClaimRecord struct {
	canonicalNameKey       string
	token                  [sha256.Size]byte
	operationID            receivecontract.OperationID
	reservationID          receivecontract.DestinationReservationID
	requestedName          string
	logicalReservedName    string
	physicalName           string
	entryKind              receivecontract.ContainerEntryKind
	collisionIndex         uint32
	generation             uint64
	phase                  ReservationClaimPhase
	reservationDigest      receivecontract.BindingDigest
	persistentIdentity     []byte
	operationBindingDigest [sha256.Size]byte
}

func NewReservationClaimRecord(spec ReservationClaimRecordSpec) (ReservationClaimRecord, error) {
	if !validReservationClaimText(spec.CanonicalNameKey, MaximumCanonicalNameKeyBytesV2) ||
		!validReservationClaimText(spec.RequestedName, MaximumReservationNameBytesV2) ||
		!validReservationClaimText(spec.LogicalReservedName, MaximumReservationNameBytesV2) ||
		!validReservationClaimText(spec.PhysicalName, MaximumReservationNameBytesV2) ||
		spec.LogicalReservedName != spec.PhysicalName ||
		spec.OperationID.IsZero() || spec.ReservationID.IsZero() || spec.Generation == 0 ||
		(spec.EntryKind != receivecontract.ContainerEntrySingleFile &&
			spec.EntryKind != receivecontract.ContainerEntryResultRoot) || !spec.Phase.Valid() ||
		len(spec.PersistentIdentity) > MaximumPersistentIdentityClaimBytesV2 {
		return ReservationClaimRecord{}, ErrInvalidReservationClaim
	}
	token, _ := ReservationClaimTokenForCanonicalNameKey(spec.CanonicalNameKey)
	record := ReservationClaimRecord{
		canonicalNameKey: spec.CanonicalNameKey, token: token, operationID: spec.OperationID,
		reservationID: spec.ReservationID, requestedName: spec.RequestedName,
		logicalReservedName: spec.LogicalReservedName, physicalName: spec.PhysicalName,
		entryKind: spec.EntryKind, collisionIndex: spec.CollisionIndex, generation: spec.Generation,
		phase: spec.Phase, reservationDigest: spec.ReservationDigest,
		persistentIdentity: slices.Clone(spec.PersistentIdentity), operationBindingDigest: spec.OperationBindingDigest,
	}
	if !record.Valid() {
		return ReservationClaimRecord{}, ErrInvalidReservationClaim
	}
	return record, nil
}

func (record ReservationClaimRecord) CanonicalNameKey() string { return record.canonicalNameKey }
func (record ReservationClaimRecord) Token() [sha256.Size]byte { return record.token }
func (record ReservationClaimRecord) OperationID() receivecontract.OperationID {
	return record.operationID
}
func (record ReservationClaimRecord) ReservationID() receivecontract.DestinationReservationID {
	return record.reservationID
}
func (record ReservationClaimRecord) RequestedName() string { return record.requestedName }
func (record ReservationClaimRecord) LogicalReservedName() string {
	return record.logicalReservedName
}
func (record ReservationClaimRecord) PhysicalName() string { return record.physicalName }
func (record ReservationClaimRecord) EntryKind() receivecontract.ContainerEntryKind {
	return record.entryKind
}
func (record ReservationClaimRecord) CollisionIndex() uint32 { return record.collisionIndex }
func (record ReservationClaimRecord) Generation() uint64     { return record.generation }
func (record ReservationClaimRecord) Phase() ReservationClaimPhase {
	return record.phase
}
func (record ReservationClaimRecord) ReservationDigest() receivecontract.BindingDigest {
	return record.reservationDigest
}
func (record ReservationClaimRecord) PersistentIdentity() []byte {
	return slices.Clone(record.persistentIdentity)
}
func (record ReservationClaimRecord) OperationBindingDigest() [sha256.Size]byte {
	return record.operationBindingDigest
}

func (record ReservationClaimRecord) Valid() bool {
	if !validReservationClaimText(record.canonicalNameKey, MaximumCanonicalNameKeyBytesV2) ||
		!validReservationClaimText(record.requestedName, MaximumReservationNameBytesV2) ||
		!validReservationClaimText(record.logicalReservedName, MaximumReservationNameBytesV2) ||
		!validReservationClaimText(record.physicalName, MaximumReservationNameBytesV2) ||
		record.logicalReservedName != record.physicalName ||
		record.operationID.IsZero() || record.reservationID.IsZero() || record.generation == 0 ||
		(record.entryKind != receivecontract.ContainerEntrySingleFile &&
			record.entryKind != receivecontract.ContainerEntryResultRoot) || !record.phase.Valid() ||
		len(record.persistentIdentity) > MaximumPersistentIdentityClaimBytesV2 ||
		record.token != mustReservationClaimToken(record.canonicalNameKey) {
		return false
	}
	digestBound := !record.reservationDigest.IsZero()
	identityBound := len(record.persistentIdentity) > 0
	operationBound := record.operationBindingDigest != ([sha256.Size]byte{})
	switch record.phase {
	case ReservationClaimed:
		return !digestBound && !identityBound && !operationBound
	case ReservationBindingBound:
		return digestBound && !identityBound && !operationBound
	case ReservationDirectoryBound:
		return record.entryKind == receivecontract.ContainerEntryResultRoot && digestBound && identityBound && !operationBound
	case ReservationOperationBound:
		return digestBound && operationBound &&
			(record.entryKind == receivecontract.ContainerEntrySingleFile && !identityBound ||
				record.entryKind == receivecontract.ContainerEntryResultRoot && identityBound)
	default:
		return false
	}
}

func BindReservationClaim(
	previous ReservationClaimRecord,
	digest receivecontract.BindingDigest,
) (ReservationClaimRecord, error) {
	if !previous.Valid() || previous.phase != ReservationClaimed || digest.IsZero() {
		return ReservationClaimRecord{}, ErrInvalidReservationClaim
	}
	next := previous
	if !advanceReservationClaim(&next) {
		return ReservationClaimRecord{}, ErrInvalidReservationClaim
	}
	next.phase, next.reservationDigest = ReservationBindingBound, digest
	return next, nil
}

func BindReservationDirectory(
	previous ReservationClaimRecord,
	persistentIdentity []byte,
) (ReservationClaimRecord, error) {
	if !previous.Valid() || previous.phase != ReservationBindingBound ||
		previous.entryKind != receivecontract.ContainerEntryResultRoot ||
		len(persistentIdentity) == 0 || len(persistentIdentity) > MaximumPersistentIdentityClaimBytesV2 {
		return ReservationClaimRecord{}, ErrInvalidReservationClaim
	}
	next := previous
	if !advanceReservationClaim(&next) {
		return ReservationClaimRecord{}, ErrInvalidReservationClaim
	}
	next.phase = ReservationDirectoryBound
	next.persistentIdentity = slices.Clone(persistentIdentity)
	return next, nil
}

func BindReservationOperation(
	previous ReservationClaimRecord,
	recordDigest [sha256.Size]byte,
) (ReservationClaimRecord, error) {
	if !previous.Valid() || recordDigest == ([sha256.Size]byte{}) ||
		(previous.entryKind == receivecontract.ContainerEntrySingleFile && previous.phase != ReservationBindingBound) ||
		(previous.entryKind == receivecontract.ContainerEntryResultRoot && previous.phase != ReservationDirectoryBound) {
		return ReservationClaimRecord{}, ErrInvalidReservationClaim
	}
	next := previous
	if !advanceReservationClaim(&next) {
		return ReservationClaimRecord{}, ErrInvalidReservationClaim
	}
	next.phase, next.operationBindingDigest = ReservationOperationBound, recordDigest
	return next, nil
}

func SameReservationClaim(left, right ReservationClaimRecord) bool {
	return left.Valid() && right.Valid() && left.token == right.token &&
		left.canonicalNameKey == right.canonicalNameKey && left.operationID == right.operationID &&
		left.reservationID == right.reservationID && left.requestedName == right.requestedName &&
		left.logicalReservedName == right.logicalReservedName && left.physicalName == right.physicalName &&
		left.entryKind == right.entryKind &&
		left.collisionIndex == right.collisionIndex
}

func ReservationClaimRecordFromCanonicalBytes(
	encoded []byte,
	token [sha256.Size]byte,
	generation uint64,
) (ReservationClaimRecord, error) {
	record, err := DecodeReservationClaimRecord(encoded)
	if err != nil || record.token != token || record.generation != generation {
		return ReservationClaimRecord{}, ErrInvalidReservationClaim
	}
	return record, nil
}

func EncodeReservationClaimRecord(record ReservationClaimRecord) ([]byte, error) {
	if !record.Valid() {
		return nil, ErrInvalidReservationClaim
	}
	var encoded bytes.Buffer
	_, _ = encoded.WriteString(ReservationClaimRecordDomainV2)
	_ = encoded.WriteByte(0)
	_ = encoded.WriteByte(ReservationClaimRecordVersionV2)
	writeOrdinaryFrame(&encoded, []byte(record.canonicalNameKey))
	writeOrdinaryFrame(&encoded, record.token[:])
	writeOrdinaryFrame(&encoded, record.operationID.Bytes())
	writeOrdinaryFrame(&encoded, record.reservationID.Bytes())
	writeOrdinaryFrame(&encoded, []byte(record.requestedName))
	writeOrdinaryFrame(&encoded, []byte(record.logicalReservedName))
	writeOrdinaryFrame(&encoded, []byte(record.physicalName))
	_ = encoded.WriteByte(byte(record.entryKind))
	writeReservationClaimUint32(&encoded, record.collisionIndex)
	writeOrdinaryUint64(&encoded, record.generation)
	_ = encoded.WriteByte(byte(record.phase))
	reservationDigest := record.reservationDigest.Bytes()
	if record.reservationDigest.IsZero() {
		reservationDigest = nil
	}
	writeReservationClaimOptionalFrame(&encoded, reservationDigest)
	writeReservationClaimOptionalFrame(&encoded, record.persistentIdentity)
	writeReservationClaimOptionalFrame(&encoded, record.operationBindingDigest[:])
	if encoded.Len() > MaximumReservationClaimRecordBytesV2 {
		return nil, ErrInvalidReservationClaim
	}
	return encoded.Bytes(), nil
}

func DecodeReservationClaimRecord(encoded []byte) (ReservationClaimRecord, error) {
	if len(encoded) == 0 || len(encoded) > MaximumReservationClaimRecordBytesV2 {
		return ReservationClaimRecord{}, ErrInvalidReservationClaim
	}
	prefix := append(append([]byte(nil), ReservationClaimRecordDomainV2...), 0, ReservationClaimRecordVersionV2)
	if !bytes.HasPrefix(encoded, prefix) {
		return ReservationClaimRecord{}, ErrInvalidReservationClaim
	}
	cursor := reservationClaimCursor{encoded: encoded, offset: len(prefix)}
	key := cursor.text(MaximumCanonicalNameKeyBytesV2)
	tokenRaw := cursor.frame(sha256.Size)
	operationRaw := cursor.frame(receivecontract.StableIdentityBytes)
	reservationRaw := cursor.frame(receivecontract.StableIdentityBytes)
	requested := cursor.text(MaximumReservationNameBytesV2)
	logicalReserved := cursor.text(MaximumReservationNameBytesV2)
	physical := cursor.text(MaximumReservationNameBytesV2)
	entryKind := cursor.byte()
	collisionIndex := cursor.uint32()
	generation := cursor.uint64()
	phase := cursor.byte()
	digestRaw := cursor.optionalFrame(sha256.Size)
	identityRaw := cursor.optionalFrame(MaximumPersistentIdentityClaimBytesV2)
	operationDigestRaw := cursor.optionalFrame(sha256.Size)
	if cursor.err != nil || cursor.offset != len(encoded) {
		return ReservationClaimRecord{}, ErrInvalidReservationClaim
	}
	operation, operationErr := receivecontract.OperationIDFromBytes(operationRaw)
	reservation, reservationErr := receivecontract.DestinationReservationIDFromBytes(reservationRaw)
	var digest receivecontract.BindingDigest
	var digestErr error
	if len(digestRaw) > 0 {
		digest, digestErr = receivecontract.BindingDigestFromBytes(digestRaw)
	}
	var token, operationDigest [sha256.Size]byte
	copy(token[:], tokenRaw)
	copy(operationDigest[:], operationDigestRaw)
	record, recordErr := NewReservationClaimRecord(ReservationClaimRecordSpec{
		CanonicalNameKey: key, OperationID: operation, ReservationID: reservation,
		RequestedName: requested, LogicalReservedName: logicalReserved, PhysicalName: physical,
		EntryKind:      receivecontract.ContainerEntryKind(entryKind),
		CollisionIndex: collisionIndex, Generation: generation, Phase: ReservationClaimPhase(phase),
		ReservationDigest: digest, PersistentIdentity: identityRaw, OperationBindingDigest: operationDigest,
	})
	canonical, canonicalErr := EncodeReservationClaimRecord(record)
	if errors.Join(operationErr, reservationErr, digestErr, recordErr, canonicalErr) != nil ||
		record.token != token || !bytes.Equal(canonical, encoded) {
		return ReservationClaimRecord{}, ErrInvalidReservationClaim
	}
	return record, nil
}

func ReservationClaimTokenForCanonicalNameKey(canonicalNameKey string) ([sha256.Size]byte, error) {
	if !validReservationClaimText(canonicalNameKey, MaximumCanonicalNameKeyBytesV2) {
		return [sha256.Size]byte{}, ErrInvalidReservationClaim
	}
	hash := sha256.New()
	_, _ = hash.Write([]byte(ReservationClaimTokenDomainV2))
	_, _ = hash.Write([]byte{0, ReservationClaimRecordVersionV2})
	writeOrdinaryFrame(hash, []byte(canonicalNameKey))
	var token [sha256.Size]byte
	copy(token[:], hash.Sum(nil))
	return token, nil
}

func mustReservationClaimToken(canonicalNameKey string) [sha256.Size]byte {
	token, _ := ReservationClaimTokenForCanonicalNameKey(canonicalNameKey)
	return token
}

func validReservationClaimText(value string, maximum int) bool {
	return value != "" && len(value) <= maximum && utf8.ValidString(value)
}

func advanceReservationClaim(record *ReservationClaimRecord) bool {
	if record.generation == ^uint64(0) {
		return false
	}
	record.generation++
	return true
}

func writeReservationClaimUint32(writer interface{ Write([]byte) (int, error) }, value uint32) {
	var encoded [4]byte
	binary.BigEndian.PutUint32(encoded[:], value)
	_, _ = writer.Write(encoded[:])
}

func writeReservationClaimOptionalFrame(writer interface{ Write([]byte) (int, error) }, value []byte) {
	var length [8]byte
	if allZero(value) {
		value = nil
	}
	binary.BigEndian.PutUint64(length[:], uint64(len(value)))
	_, _ = writer.Write(length[:])
	_, _ = writer.Write(value)
}

type reservationClaimCursor struct {
	encoded []byte
	offset  int
	err     error
}

func (cursor *reservationClaimCursor) take(count int) []byte {
	if cursor.err != nil {
		return nil
	}
	if count < 0 || count > len(cursor.encoded)-cursor.offset {
		cursor.err = ErrInvalidReservationClaim
		return nil
	}
	value := cursor.encoded[cursor.offset : cursor.offset+count]
	cursor.offset += count
	return value
}

func (cursor *reservationClaimCursor) byte() byte {
	value := cursor.take(1)
	if cursor.err != nil {
		return 0
	}
	return value[0]
}

func (cursor *reservationClaimCursor) uint32() uint32 {
	value := cursor.take(4)
	if cursor.err != nil {
		return 0
	}
	return binary.BigEndian.Uint32(value)
}

func (cursor *reservationClaimCursor) uint64() uint64 {
	value := cursor.take(8)
	if cursor.err != nil {
		return 0
	}
	return binary.BigEndian.Uint64(value)
}

func (cursor *reservationClaimCursor) frame(maximum int) []byte {
	value := cursor.optionalFrame(maximum)
	if len(value) == 0 && cursor.err == nil {
		cursor.err = ErrInvalidReservationClaim
	}
	return value
}

func (cursor *reservationClaimCursor) optionalFrame(maximum int) []byte {
	length := cursor.uint64()
	if cursor.err != nil || length > uint64(maximum) || length > uint64(len(cursor.encoded)-cursor.offset) {
		cursor.err = fmt.Errorf("%w: framed field", ErrInvalidReservationClaim)
		return nil
	}
	return cursor.take(int(length))
}

func (cursor *reservationClaimCursor) text(maximum int) string {
	value := cursor.frame(maximum)
	if cursor.err != nil || !utf8.Valid(value) {
		cursor.err = ErrInvalidReservationClaim
		return ""
	}
	return string(value)
}
