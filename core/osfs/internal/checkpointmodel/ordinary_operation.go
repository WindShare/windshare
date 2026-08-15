package checkpointmodel

import (
	"bytes"
	"crypto/sha256"
	"encoding/binary"
	"errors"
	"fmt"
	"slices"

	"github.com/windshare/windshare/core/transfer"
	"github.com/windshare/windshare/core/transfer/ordinaryoutput"
	"github.com/windshare/windshare/core/transfer/receivecontract"
)

const (
	ActiveOperationKeyV1       = uint8(1)
	ActiveOperationKeyDomainV1 = "windshare/active-operation-key/v1"

	OrdinaryOperationRecordVersionV1      = uint8(1)
	OrdinaryOperationRecordDomainV1       = "windshare/ordinary-operation/v1"
	OrdinaryOperationBindingDomainV1      = "windshare/ordinary-operation-binding/v1"
	MaximumOrdinaryOperationRecordBytesV1 = 2 * 1024 * 1024
	MaximumOrdinaryReceiveIntentBytesV1   = 1024 * 1024

	OrdinaryAdmissionCandidateVersionV1 = uint8(1)
	OrdinaryAdmissionCandidateDomainV1  = "windshare/ordinary-admission-candidate/v1"
)

var ErrInvalidOrdinaryOperation = errors.New("ordinary operation record is invalid")

type ActiveOperationKey [sha256.Size]byte

func NewActiveOperationKey(
	selection transfer.SelectionSpecDigest,
	destinationAuthorityID receivecontract.AuthorityRef,
	policyVersion uint8,
) (ActiveOperationKey, error) {
	if selection.IsZero() || destinationAuthorityID.IsZero() || policyVersion == 0 {
		return ActiveOperationKey{}, ErrInvalidOrdinaryOperation
	}
	hash := sha256.New()
	_, _ = hash.Write([]byte(ActiveOperationKeyDomainV1))
	_, _ = hash.Write([]byte{0, ActiveOperationKeyV1})
	writeOrdinaryFrame(hash, selection.Bytes())
	writeOrdinaryFrame(hash, destinationAuthorityID.Bytes())
	writeOrdinaryFrame(hash, []byte{policyVersion})
	var key ActiveOperationKey
	copy(key[:], hash.Sum(nil))
	return key, nil
}

func NewActiveOperationKeyV1(
	selection transfer.SelectionSpecDigest,
	destinationAuthorityID receivecontract.AuthorityRef,
) (ActiveOperationKey, error) {
	return NewActiveOperationKey(selection, destinationAuthorityID, ordinaryoutput.OrdinaryOutputPolicyVersion)
}

func ActiveOperationKeyFromBytes(raw []byte) (ActiveOperationKey, error) {
	if len(raw) != sha256.Size {
		return ActiveOperationKey{}, ErrInvalidOrdinaryOperation
	}
	var key ActiveOperationKey
	copy(key[:], raw)
	if key.IsZero() {
		return ActiveOperationKey{}, ErrInvalidOrdinaryOperation
	}
	return key, nil
}

func (key ActiveOperationKey) Bytes() []byte { return slices.Clone(key[:]) }
func (key ActiveOperationKey) IsZero() bool  { return key == ActiveOperationKey{} }

// ReservationClaimLocator is the exact private-record coordinate needed to
// recover a frozen destination reservation. The generation names the
// operation-bound claim, not the earlier authority handle generation.
type ReservationClaimLocator struct {
	token      [sha256.Size]byte
	generation uint64
}

func NewReservationClaimLocator(token [sha256.Size]byte, generation uint64) (ReservationClaimLocator, error) {
	if token == ([sha256.Size]byte{}) || generation == 0 {
		return ReservationClaimLocator{}, ErrInvalidOrdinaryOperation
	}
	return ReservationClaimLocator{token: token, generation: generation}, nil
}

func (locator ReservationClaimLocator) Token() [sha256.Size]byte { return locator.token }
func (locator ReservationClaimLocator) Generation() uint64       { return locator.generation }
func (locator ReservationClaimLocator) Valid() bool {
	return locator.token != ([sha256.Size]byte{}) && locator.generation > 0
}

type OrdinaryOperationRecordSpec struct {
	ActiveKey           ActiveOperationKey
	Intent              transfer.ReceiveIntent
	ReservationClaim    ReservationClaimLocator
	LifecycleGeneration uint64
	Lifecycle           OrdinaryOperationLifecycle
	Lease               OrdinaryLeaseState
	ClosedReason        OrdinaryClosedReason
}

// NextOrdinaryOperationRecordSpec retains the immutable operation binding while
// advancing only registry-owned state. Keeping this constructor in the model
// prevents persistence code from reconstructing a record from decoded intent
// bytes and accidentally changing its authority coordinates.
type NextOrdinaryOperationRecordSpec struct {
	Lifecycle    OrdinaryOperationLifecycle
	Lease        OrdinaryLeaseState
	ClosedReason OrdinaryClosedReason
}

// OrdinaryOperationRecord is intentionally operation-sized. Per-file paths,
// revisions, ranges, phases, and checkpoint references remain solely in
// FileCheckpointV2 and can therefore never make this active index grow.
type OrdinaryOperationRecord struct {
	activeKey           ActiveOperationKey
	operationID         receivecontract.OperationID
	intentBytes         []byte
	intentDigest        transfer.ReceiveIntentDigest
	reservationClaim    ReservationClaimLocator
	lifecycleGeneration uint64
	lifecycle           OrdinaryOperationLifecycle
	lease               OrdinaryLeaseState
	closedReason        OrdinaryClosedReason
}

func NewOrdinaryOperationRecord(spec OrdinaryOperationRecordSpec) (OrdinaryOperationRecord, error) {
	if spec.ActiveKey.IsZero() || spec.Intent.IsZero() || spec.LifecycleGeneration == 0 ||
		len(spec.Intent.CanonicalBytes()) > MaximumOrdinaryReceiveIntentBytesV1 {
		return OrdinaryOperationRecord{}, ErrInvalidOrdinaryOperation
	}
	record := OrdinaryOperationRecord{
		activeKey: spec.ActiveKey, operationID: spec.Intent.OperationID(),
		intentBytes: spec.Intent.CanonicalBytes(), intentDigest: spec.Intent.Digest(),
		reservationClaim:    spec.ReservationClaim,
		lifecycleGeneration: spec.LifecycleGeneration, lifecycle: spec.Lifecycle,
		lease: spec.Lease, closedReason: spec.ClosedReason,
	}
	if !record.Valid() {
		return OrdinaryOperationRecord{}, ErrInvalidOrdinaryOperation
	}
	return record, nil
}

func (record OrdinaryOperationRecord) ActiveOperationKey() ActiveOperationKey {
	return record.activeKey
}
func (record OrdinaryOperationRecord) OperationID() receivecontract.OperationID {
	return record.operationID
}
func (record OrdinaryOperationRecord) ReceiveIntentDigest() transfer.ReceiveIntentDigest {
	return record.intentDigest
}
func (record OrdinaryOperationRecord) ReservationClaim() ReservationClaimLocator {
	return record.reservationClaim
}
func (record OrdinaryOperationRecord) IntentBytes() []byte                   { return slices.Clone(record.intentBytes) }
func (record OrdinaryOperationRecord) LifecycleGeneration() uint64           { return record.lifecycleGeneration }
func (record OrdinaryOperationRecord) Lifecycle() OrdinaryOperationLifecycle { return record.lifecycle }
func (record OrdinaryOperationRecord) Lease() OrdinaryLeaseState             { return record.lease }
func (record OrdinaryOperationRecord) ClosedReason() OrdinaryClosedReason    { return record.closedReason }

func NextOrdinaryOperationRecord(
	previous OrdinaryOperationRecord,
	spec NextOrdinaryOperationRecordSpec,
) (OrdinaryOperationRecord, error) {
	if !previous.Valid() || previous.lifecycleGeneration == ^uint64(0) {
		return OrdinaryOperationRecord{}, ErrInvalidOrdinaryOperation
	}
	next := previous
	next.lifecycleGeneration++
	next.lifecycle = spec.Lifecycle
	next.lease = spec.Lease
	next.closedReason = spec.ClosedReason
	if !next.Valid() {
		return OrdinaryOperationRecord{}, ErrInvalidOrdinaryOperation
	}
	return next, nil
}

func SameOrdinaryOperation(left, right OrdinaryOperationRecord) bool {
	if !left.Valid() || !right.Valid() {
		return false
	}
	return left.activeKey == right.activeKey && left.operationID == right.operationID &&
		left.intentDigest == right.intentDigest && left.reservationClaim == right.reservationClaim &&
		bytes.Equal(left.intentBytes, right.intentBytes)
}

type OrdinaryAdmissionCandidateState uint8

const (
	OrdinaryAdmissionPreparing OrdinaryAdmissionCandidateState = iota + 1
	OrdinaryAdmissionNeedsAttention
)

type OrdinaryAdmissionCandidate struct {
	activeKey        ActiveOperationKey
	operationID      receivecontract.OperationID
	reservationClaim ReservationClaimLocator
	generation       uint64
	state            OrdinaryAdmissionCandidateState
}

func NewOrdinaryAdmissionCandidate(
	activeKey ActiveOperationKey,
	operationID receivecontract.OperationID,
) (OrdinaryAdmissionCandidate, error) {
	if activeKey.IsZero() || operationID.IsZero() {
		return OrdinaryAdmissionCandidate{}, ErrInvalidOrdinaryOperation
	}
	return OrdinaryAdmissionCandidate{
		activeKey: activeKey, operationID: operationID, generation: 1, state: OrdinaryAdmissionPreparing,
	}, nil
}

func (candidate OrdinaryAdmissionCandidate) ActiveOperationKey() ActiveOperationKey {
	return candidate.activeKey
}
func (candidate OrdinaryAdmissionCandidate) OperationID() receivecontract.OperationID {
	return candidate.operationID
}
func (candidate OrdinaryAdmissionCandidate) Generation() uint64 { return candidate.generation }
func (candidate OrdinaryAdmissionCandidate) State() OrdinaryAdmissionCandidateState {
	return candidate.state
}
func (candidate OrdinaryAdmissionCandidate) ReservationClaim() ReservationClaimLocator {
	return candidate.reservationClaim
}
func (candidate OrdinaryAdmissionCandidate) Valid() bool {
	return !candidate.activeKey.IsZero() && !candidate.operationID.IsZero() && candidate.generation > 0 &&
		(candidate.state == OrdinaryAdmissionPreparing || candidate.state == OrdinaryAdmissionNeedsAttention)
}

// BindOrdinaryAdmissionReservation adds the exact cleanup locator while the
// active-key admission lock is retained. The initial marker still precedes name
// selection; this transition makes every later public mutation exactly
// reconcilable without paging the global claim table.
func BindOrdinaryAdmissionReservation(
	previous OrdinaryAdmissionCandidate,
	claim ReservationClaimLocator,
) (OrdinaryAdmissionCandidate, error) {
	if !previous.Valid() || previous.reservationClaim.Valid() || !claim.Valid() || previous.generation == ^uint64(0) {
		return OrdinaryAdmissionCandidate{}, ErrInvalidOrdinaryOperation
	}
	next := previous
	next.generation++
	next.reservationClaim = claim
	return next, nil
}

func RequireOrdinaryAdmissionAttention(
	previous OrdinaryAdmissionCandidate,
) (OrdinaryAdmissionCandidate, error) {
	if !previous.Valid() || previous.state != OrdinaryAdmissionPreparing || previous.generation == ^uint64(0) {
		return OrdinaryAdmissionCandidate{}, ErrInvalidOrdinaryOperation
	}
	next := previous
	next.generation++
	next.state = OrdinaryAdmissionNeedsAttention
	return next, nil
}

func EncodeOrdinaryAdmissionCandidate(candidate OrdinaryAdmissionCandidate) ([]byte, error) {
	if !candidate.Valid() {
		return nil, ErrInvalidOrdinaryOperation
	}
	var encoded bytes.Buffer
	_, _ = encoded.WriteString(OrdinaryAdmissionCandidateDomainV1)
	_ = encoded.WriteByte(0)
	_ = encoded.WriteByte(OrdinaryAdmissionCandidateVersionV1)
	writeOrdinaryFrame(&encoded, candidate.activeKey.Bytes())
	writeOrdinaryFrame(&encoded, candidate.operationID.Bytes())
	writeOrdinaryOptionalFrame(&encoded, candidate.reservationClaim.token[:])
	writeOrdinaryUint64(&encoded, candidate.reservationClaim.generation)
	writeOrdinaryUint64(&encoded, candidate.generation)
	_ = encoded.WriteByte(byte(candidate.state))
	return encoded.Bytes(), nil
}

func DecodeOrdinaryAdmissionCandidate(encoded []byte) (OrdinaryAdmissionCandidate, error) {
	prefix := append(append([]byte(nil), OrdinaryAdmissionCandidateDomainV1...), 0, OrdinaryAdmissionCandidateVersionV1)
	if len(encoded) == 0 || !bytes.HasPrefix(encoded, prefix) {
		return OrdinaryAdmissionCandidate{}, ErrInvalidOrdinaryOperation
	}
	cursor := ordinaryCursor{encoded: encoded, offset: len(prefix)}
	keyRaw, keyErr := cursor.frame(sha256.Size)
	operationRaw, operationErr := cursor.frame(receivecontract.StableIdentityBytes)
	claimTokenRaw, claimTokenErr := cursor.optionalFrame(sha256.Size)
	claimGeneration, claimGenerationErr := cursor.uint64()
	generation, generationErr := cursor.uint64()
	state, stateErr := cursor.byte()
	key, parseKeyErr := ActiveOperationKeyFromBytes(keyRaw)
	operation, parseOperationErr := receivecontract.OperationIDFromBytes(operationRaw)
	var claimToken [sha256.Size]byte
	copy(claimToken[:], claimTokenRaw)
	var claim ReservationClaimLocator
	var parseClaimErr error
	if len(claimTokenRaw) > 0 || claimGeneration > 0 {
		claim, parseClaimErr = NewReservationClaimLocator(claimToken, claimGeneration)
	}
	candidate := OrdinaryAdmissionCandidate{
		activeKey: key, operationID: operation, reservationClaim: claim,
		generation: generation, state: OrdinaryAdmissionCandidateState(state),
	}
	canonical, canonicalErr := EncodeOrdinaryAdmissionCandidate(candidate)
	if errors.Join(keyErr, operationErr, claimTokenErr, claimGenerationErr, generationErr, stateErr,
		parseKeyErr, parseOperationErr, parseClaimErr, canonicalErr) != nil ||
		cursor.offset != len(encoded) || !bytes.Equal(canonical, encoded) {
		return OrdinaryAdmissionCandidate{}, ErrInvalidOrdinaryOperation
	}
	return candidate, nil
}

func (record OrdinaryOperationRecord) Valid() bool {
	if record.activeKey.IsZero() || record.operationID.IsZero() || record.intentDigest.IsZero() ||
		!record.reservationClaim.Valid() || record.lifecycleGeneration == 0 || !record.lifecycle.Valid() || !record.lease.Valid() ||
		len(record.intentBytes) == 0 || len(record.intentBytes) > MaximumOrdinaryReceiveIntentBytesV1 ||
		transfer.ReceiveIntentDigest(sha256.Sum256(record.intentBytes)) != record.intentDigest {
		return false
	}
	if record.lifecycle == OrdinaryOperationNeedsAttention {
		return record.closedReason.IsAttentionReason()
	}
	if record.lifecycle == OrdinaryOperationCleanupPending {
		return record.closedReason.IsCleanupReason()
	}
	return record.closedReason == OrdinaryReasonNone
}

type OrdinaryReceiveIntentDecoder func([]byte) (transfer.ReceiveIntent, error)

func (record OrdinaryOperationRecord) VerifyIntent(
	decode OrdinaryReceiveIntentDecoder,
) (transfer.ReceiveIntent, error) {
	if !record.Valid() || decode == nil {
		return transfer.ReceiveIntent{}, ErrInvalidOrdinaryOperation
	}
	intent, err := decode(record.IntentBytes())
	if err != nil || intent.IsZero() || intent.OperationID() != record.operationID ||
		intent.Digest() != record.intentDigest || !bytes.Equal(intent.CanonicalBytes(), record.intentBytes) {
		return transfer.ReceiveIntent{}, errors.Join(ErrInvalidOrdinaryOperation, err)
	}
	return intent, nil
}

func EncodeOrdinaryOperationRecord(record OrdinaryOperationRecord) ([]byte, error) {
	if !record.Valid() {
		return nil, ErrInvalidOrdinaryOperation
	}
	var encoded bytes.Buffer
	_, _ = encoded.WriteString(OrdinaryOperationRecordDomainV1)
	_ = encoded.WriteByte(0)
	_ = encoded.WriteByte(OrdinaryOperationRecordVersionV1)
	writeOrdinaryFrame(&encoded, record.activeKey.Bytes())
	writeOrdinaryFrame(&encoded, record.operationID.Bytes())
	writeOrdinaryFrame(&encoded, record.intentBytes)
	writeOrdinaryFrame(&encoded, record.intentDigest.Bytes())
	writeOrdinaryFrame(&encoded, record.reservationClaim.token[:])
	writeOrdinaryUint64(&encoded, record.reservationClaim.generation)
	writeOrdinaryUint64(&encoded, record.lifecycleGeneration)
	_ = encoded.WriteByte(byte(record.lifecycle))
	_ = encoded.WriteByte(byte(record.lease))
	_ = encoded.WriteByte(byte(record.closedReason))
	if encoded.Len() > MaximumOrdinaryOperationRecordBytesV1 {
		return nil, ErrInvalidOrdinaryOperation
	}
	return encoded.Bytes(), nil
}

func DecodeOrdinaryOperationRecord(encoded []byte) (OrdinaryOperationRecord, error) {
	if len(encoded) == 0 || len(encoded) > MaximumOrdinaryOperationRecordBytesV1 {
		return OrdinaryOperationRecord{}, ErrInvalidOrdinaryOperation
	}
	prefix := append(append([]byte(nil), OrdinaryOperationRecordDomainV1...), 0, OrdinaryOperationRecordVersionV1)
	if !bytes.HasPrefix(encoded, prefix) {
		return OrdinaryOperationRecord{}, ErrInvalidOrdinaryOperation
	}
	cursor := ordinaryCursor{encoded: encoded, offset: len(prefix)}
	activeRaw, activeErr := cursor.frame(sha256.Size)
	operationRaw, operationErr := cursor.frame(receivecontract.StableIdentityBytes)
	intentRaw, intentErr := cursor.frame(MaximumOrdinaryReceiveIntentBytesV1)
	digestRaw, digestErr := cursor.frame(transfer.ReceiveIntentDigestBytes)
	claimTokenRaw, claimTokenErr := cursor.frame(sha256.Size)
	claimGeneration, claimGenerationErr := cursor.uint64()
	generation, generationErr := cursor.uint64()
	lifecycle, lifecycleErr := cursor.byte()
	lease, leaseErr := cursor.byte()
	reason, reasonErr := cursor.byte()
	if errors.Join(activeErr, operationErr, intentErr, digestErr, claimTokenErr, claimGenerationErr,
		generationErr, lifecycleErr, leaseErr, reasonErr) != nil ||
		cursor.offset != len(encoded) {
		return OrdinaryOperationRecord{}, ErrInvalidOrdinaryOperation
	}
	active, activeErr := ActiveOperationKeyFromBytes(activeRaw)
	operation, operationErr := receivecontract.OperationIDFromBytes(operationRaw)
	digest, digestErr := transfer.ReceiveIntentDigestFromBytes(digestRaw)
	var claimToken [sha256.Size]byte
	copy(claimToken[:], claimTokenRaw)
	claimLocator, claimErr := NewReservationClaimLocator(claimToken, claimGeneration)
	record := OrdinaryOperationRecord{
		activeKey: active, operationID: operation, intentBytes: slices.Clone(intentRaw), intentDigest: digest,
		reservationClaim:    claimLocator,
		lifecycleGeneration: generation, lifecycle: OrdinaryOperationLifecycle(lifecycle),
		lease: OrdinaryLeaseState(lease), closedReason: OrdinaryClosedReason(reason),
	}
	canonical, canonicalErr := EncodeOrdinaryOperationRecord(record)
	if errors.Join(activeErr, operationErr, digestErr, claimErr, canonicalErr) != nil || !bytes.Equal(canonical, encoded) {
		return OrdinaryOperationRecord{}, ErrInvalidOrdinaryOperation
	}
	return record, nil
}

// OrdinaryOperationBindingDigest authenticates the immutable operation,
// destination-reservation, and claim coordinates. Lifecycle and lease changes
// intentionally do not alter it, so the active index and reverse claim link do
// not require unsafe multi-file rewrites on every state transition.
func OrdinaryOperationBindingDigest(record OrdinaryOperationRecord) ([sha256.Size]byte, error) {
	if !record.Valid() {
		return [sha256.Size]byte{}, ErrInvalidOrdinaryOperation
	}
	hash := sha256.New()
	_, _ = hash.Write([]byte(OrdinaryOperationBindingDomainV1))
	_, _ = hash.Write([]byte{0, OrdinaryOperationRecordVersionV1})
	writeOrdinaryFrame(hash, record.activeKey.Bytes())
	writeOrdinaryFrame(hash, record.operationID.Bytes())
	writeOrdinaryFrame(hash, record.intentBytes)
	writeOrdinaryFrame(hash, record.intentDigest.Bytes())
	writeOrdinaryFrame(hash, record.reservationClaim.token[:])
	writeOrdinaryUint64(hash, record.reservationClaim.generation)
	var digest [sha256.Size]byte
	copy(digest[:], hash.Sum(nil))
	return digest, nil
}

type ordinaryCursor struct {
	encoded []byte
	offset  int
}

func (cursor *ordinaryCursor) take(count int) ([]byte, error) {
	if count < 0 || cursor.offset < 0 || count > len(cursor.encoded)-cursor.offset {
		return nil, ErrInvalidOrdinaryOperation
	}
	value := cursor.encoded[cursor.offset : cursor.offset+count]
	cursor.offset += count
	return value, nil
}

func (cursor *ordinaryCursor) byte() (byte, error) {
	value, err := cursor.take(1)
	if err != nil {
		return 0, err
	}
	return value[0], nil
}

func (cursor *ordinaryCursor) uint64() (uint64, error) {
	value, err := cursor.take(8)
	if err != nil {
		return 0, err
	}
	return binary.BigEndian.Uint64(value), nil
}

func (cursor *ordinaryCursor) frame(maximum int) ([]byte, error) {
	length, err := cursor.uint64()
	if err != nil || length == 0 || length > uint64(maximum) || length > uint64(len(cursor.encoded)-cursor.offset) {
		return nil, fmt.Errorf("%w: framed field", ErrInvalidOrdinaryOperation)
	}
	return cursor.take(int(length))
}

func (cursor *ordinaryCursor) optionalFrame(maximum int) ([]byte, error) {
	length, err := cursor.uint64()
	if err != nil || length > uint64(maximum) || length > uint64(len(cursor.encoded)-cursor.offset) {
		return nil, fmt.Errorf("%w: optional framed field", ErrInvalidOrdinaryOperation)
	}
	if length == 0 {
		return nil, nil
	}
	return cursor.take(int(length))
}

func writeOrdinaryFrame(writer interface{ Write([]byte) (int, error) }, value []byte) {
	var length [8]byte
	binary.BigEndian.PutUint64(length[:], uint64(len(value)))
	_, _ = writer.Write(length[:])
	_, _ = writer.Write(value)
}

func writeOrdinaryUint64(writer interface{ Write([]byte) (int, error) }, value uint64) {
	var encoded [8]byte
	binary.BigEndian.PutUint64(encoded[:], value)
	_, _ = writer.Write(encoded[:])
}

func writeOrdinaryOptionalFrame(writer interface{ Write([]byte) (int, error) }, value []byte) {
	if allZero(value) {
		value = nil
	}
	var length [8]byte
	binary.BigEndian.PutUint64(length[:], uint64(len(value)))
	_, _ = writer.Write(length[:])
	_, _ = writer.Write(value)
}
