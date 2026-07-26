package resumestate

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"io"
	"strings"
)

const (
	UpdateNonceBytes             = OutputObjectIDBytes
	UpdateTemporarySeparator     = ".state.tmp-"
	recordTemporarySeparator     = ".tmp-"
	HeaderUpdateTemporaryPrefix  = HeaderRecordName + recordTemporarySeparator
	ControlUpdateTemporaryPrefix = ControlRecordName + recordTemporarySeparator
)

type UpdateNonce [UpdateNonceBytes]byte

func NewUpdateNonce() (UpdateNonce, error) { return GenerateUpdateNonce(rand.Reader) }

func GenerateUpdateNonce(random io.Reader) (UpdateNonce, error) {
	var nonce UpdateNonce
	if random == nil {
		return nonce, fmt.Errorf("%w: update entropy source is nil", ErrInvalidState)
	}
	if _, err := io.ReadFull(random, nonce[:]); err != nil {
		return UpdateNonce{}, fmt.Errorf("generate update nonce: %w", err)
	}
	if nonce.IsZero() {
		return UpdateNonce{}, fmt.Errorf("%w: update nonce is zero", ErrInvalidState)
	}
	return nonce, nil
}

func UpdateNonceFromBytes(raw []byte) (UpdateNonce, error) {
	return fixedIDFromBytes[UpdateNonce](raw, UpdateNonceBytes, "update nonce")
}

func (nonce UpdateNonce) Bytes() []byte  { return fixedIDBytes(nonce) }
func (nonce UpdateNonce) String() string { return fixedIDString(nonce) }
func (nonce UpdateNonce) IsZero() bool   { return nonce == UpdateNonce{} }

// RecordUpdateTemporaryName centralizes the grammar shared by initial envelope
// candidates and later atomic replacements. Recovery must never derive cleanup
// authority from a caller-assembled prefix that accepts a second spelling.
func RecordUpdateTemporaryName(target string, nonce UpdateNonce) (string, error) {
	if !validRecordUpdateTarget(target) || nonce.IsZero() {
		return "", fmt.Errorf("%w: state update temporary target", ErrInvalidState)
	}
	return target + recordTemporarySeparator + nonce.String(), nil
}

func RecordUpdateTemporaryPrefix(target string) (string, error) {
	if !validRecordUpdateTarget(target) {
		return "", fmt.Errorf("%w: state update temporary target", ErrInvalidState)
	}
	return target + recordTemporarySeparator, nil
}

func ParseRecordUpdateTemporaryName(target, name string) (UpdateNonce, error) {
	if !validRecordUpdateTarget(target) || !strings.HasPrefix(name, target+recordTemporarySeparator) {
		return UpdateNonce{}, fmt.Errorf("%w: state update temporary name", ErrInvalidState)
	}
	encodedNonce := strings.TrimPrefix(name, target+recordTemporarySeparator)
	if !validLowerHex(encodedNonce, encodedSHA256Characters) {
		return UpdateNonce{}, fmt.Errorf("%w: state update temporary nonce", ErrInvalidState)
	}
	raw, _ := hex.DecodeString(encodedNonce)
	nonce, err := UpdateNonceFromBytes(raw)
	if err != nil {
		return UpdateNonce{}, fmt.Errorf("%w: state update temporary nonce", ErrInvalidState)
	}
	canonical, _ := RecordUpdateTemporaryName(target, nonce)
	if canonical != name {
		return UpdateNonce{}, fmt.Errorf("%w: non-canonical state update temporary", ErrInvalidState)
	}
	return nonce, nil
}

func validRecordUpdateTarget(target string) bool {
	if target == HeaderRecordName || target == ControlRecordName {
		return true
	}
	if len(target) != encodedSHA256Characters+len(".state") || !strings.HasSuffix(target, ".state") {
		return false
	}
	return validLowerHex(strings.TrimSuffix(target, ".state"), encodedSHA256Characters)
}

func UpdateTemporaryName(target LocatorDigest, nonce UpdateNonce) ShardedName {
	record := FileRecordName(target)
	name, _ := RecordUpdateTemporaryName(record.name, nonce)
	return ShardedName{
		shard: record.shard,
		name:  name,
	}
}

// classifyUpdateTemporaryName never treats a temporary as state authority. It
// extracts a target only to choose the smallest safe quarantine scope.
func classifyUpdateTemporaryName(shard, name string) ClassifiedFileShardEntry {
	if len(name) < encodedSHA256Characters {
		return ClassifiedFileShardEntry{classification: FileShardEntryOpaque}
	}
	targetBytes, err := hex.DecodeString(name[:encodedSHA256Characters])
	if err != nil {
		return ClassifiedFileShardEntry{classification: FileShardEntryOpaque}
	}
	target, err := LocatorDigestFromBytes(targetBytes)
	if err != nil {
		return ClassifiedFileShardEntry{classification: FileShardEntryOpaque}
	}
	malformed := ClassifiedFileShardEntry{
		classification: FileShardEntryMalformedForLocator,
		locator:        target,
	}
	record := FileRecordName(target)
	if shard != record.shard {
		return malformed
	}
	nonce, err := ParseRecordUpdateTemporaryName(record.name, name)
	if err != nil {
		return malformed
	}
	return ClassifiedFileShardEntry{
		classification: FileShardEntryUpdateTemporary, locator: target, nonce: nonce,
	}
}

type UpdateTemporaryEntryObservation uint8

const (
	UpdateTemporaryEntryMissing UpdateTemporaryEntryObservation = iota + 1
	UpdateTemporaryEntryRegular
	UpdateTemporaryEntryUnsafe
)

type UpdateTargetObservation uint8

const (
	UpdateTargetMissing UpdateTargetObservation = iota + 1
	UpdateTargetValid
	UpdateTargetInvalid
)

type HeaderUpdateTemporaryClassification uint8

const (
	HeaderUpdateTemporaryCanonical HeaderUpdateTemporaryClassification = iota + 1
	HeaderUpdateTemporaryMalformed
	HeaderUpdateTemporaryUnrelated
)

type ClassifiedHeaderUpdateTemporary struct {
	classification HeaderUpdateTemporaryClassification
	nonce          UpdateNonce
}

func (classified ClassifiedHeaderUpdateTemporary) Classification() HeaderUpdateTemporaryClassification {
	return classified.classification
}

func (classified ClassifiedHeaderUpdateTemporary) Nonce() UpdateNonce { return classified.nonce }

func ClassifyHeaderUpdateTemporaryName(name string) ClassifiedHeaderUpdateTemporary {
	nonce, err := ParseRecordUpdateTemporaryName(HeaderRecordName, name)
	if err == nil {
		return ClassifiedHeaderUpdateTemporary{
			classification: HeaderUpdateTemporaryCanonical,
			nonce:          nonce,
		}
	}
	if strings.HasPrefix(name, HeaderUpdateTemporaryPrefix) {
		return ClassifiedHeaderUpdateTemporary{classification: HeaderUpdateTemporaryMalformed}
	}
	return ClassifiedHeaderUpdateTemporary{classification: HeaderUpdateTemporaryUnrelated}
}

type HeaderUpdateTemporaryAction uint8

const (
	HeaderUpdateTemporaryRemoveAndSyncSession HeaderUpdateTemporaryAction = iota + 1
	HeaderUpdateTemporaryAcceptInstalledHeader
	HeaderUpdateTemporaryBlockResumeNamespace
)

type HeaderUpdateTemporaryDecision struct {
	action    HeaderUpdateTemporaryAction
	temporary string
	namespace SessionNamespaceAuthority
}

func (decision HeaderUpdateTemporaryDecision) Action() HeaderUpdateTemporaryAction {
	return decision.action
}

func (decision HeaderUpdateTemporaryDecision) TemporaryName() string { return decision.temporary }

// ReduceHeaderUpdateTemporary recognizes only the two generation cuts that can
// legitimately retain a temporary: the initial link cut (identical installed
// header) and the pre-replace cut (the one legal next lifecycle transition).
// A temporary is never promoted, but a divergent same-generation envelope is
// evidence that the live owner no longer has singular state authority.
func ReduceHeaderUpdateTemporary(
	namespace SessionNamespaceAuthority,
	classified ClassifiedHeaderUpdateTemporary,
	entry UpdateTemporaryEntryObservation,
	candidate *Header,
) (HeaderUpdateTemporaryDecision, error) {
	if !namespace.valid() || classified.classification < HeaderUpdateTemporaryCanonical ||
		classified.classification > HeaderUpdateTemporaryUnrelated ||
		entry < UpdateTemporaryEntryMissing || entry > UpdateTemporaryEntryUnsafe ||
		(candidate != nil && !candidate.valid()) {
		return HeaderUpdateTemporaryDecision{}, fmt.Errorf("%w: header update temporary observation", ErrInvalidState)
	}
	block := HeaderUpdateTemporaryDecision{
		action: HeaderUpdateTemporaryBlockResumeNamespace, namespace: namespace,
	}
	if classified.classification != HeaderUpdateTemporaryCanonical || classified.nonce.IsZero() {
		return block, nil
	}
	temporary, err := RecordUpdateTemporaryName(HeaderRecordName, classified.nonce)
	if err != nil {
		return HeaderUpdateTemporaryDecision{}, err
	}
	if entry == UpdateTemporaryEntryMissing {
		return HeaderUpdateTemporaryDecision{
			action: HeaderUpdateTemporaryAcceptInstalledHeader, temporary: temporary, namespace: namespace,
		}, nil
	}
	if entry != UpdateTemporaryEntryRegular {
		return block, nil
	}
	if candidate == nil || *candidate == namespace.header {
		return HeaderUpdateTemporaryDecision{
			action: HeaderUpdateTemporaryRemoveAndSyncSession, temporary: temporary, namespace: namespace,
		}, nil
	}
	if sameHeaderIdentity(*candidate, namespace.header) &&
		candidate.stateGeneration < namespace.header.stateGeneration {
		// A later installed generation dominates an older proposal. This cut can
		// arise when the live lock holder settles after another opener has observed
		// (but cannot yet remove) its update temporary.
		return HeaderUpdateTemporaryDecision{
			action: HeaderUpdateTemporaryRemoveAndSyncSession, temporary: temporary, namespace: namespace,
		}, nil
	}
	next, transitionErr := namespace.WithLifecycle(candidate.lifecycle)
	if transitionErr == nil && next.header == *candidate {
		return HeaderUpdateTemporaryDecision{
			action: HeaderUpdateTemporaryRemoveAndSyncSession, temporary: temporary, namespace: namespace,
		}, nil
	}
	return block, nil
}

func sameHeaderIdentity(left, right Header) bool {
	left.lifecycle = right.lifecycle
	left.stateGeneration = right.stateGeneration
	return left == right
}

func (decision HeaderUpdateTemporaryDecision) AuthorizeRemoval(
	namespace SessionNamespaceAuthority,
	name string,
	entry UpdateTemporaryEntryObservation,
) error {
	if decision.action != HeaderUpdateTemporaryRemoveAndSyncSession || !namespace.valid() ||
		decision.namespace != namespace || entry != UpdateTemporaryEntryRegular || decision.temporary != name {
		return fmt.Errorf("%w: header update temporary removal authority", ErrInvalidState)
	}
	classified := ClassifyHeaderUpdateTemporaryName(name)
	if classified.classification != HeaderUpdateTemporaryCanonical {
		return fmt.Errorf("%w: header update temporary removal identity", ErrInvalidState)
	}
	canonical, err := RecordUpdateTemporaryName(HeaderRecordName, classified.nonce)
	if err != nil || canonical != name {
		return fmt.Errorf("%w: header update temporary removal identity", ErrInvalidState)
	}
	return nil
}

type UpdateTemporaryAction uint8

const (
	UpdateTemporaryRemoveAndSyncShard UpdateTemporaryAction = iota + 1
	UpdateTemporaryInstallFileQuarantine
	UpdateTemporaryRetainLocatorQuarantine
	UpdateTemporaryMarkSessionNeedsAttention
	UpdateTemporaryAcceptInstalledTarget
)

type UpdateTemporaryDecision struct {
	action           UpdateTemporaryAction
	target           LocatorDigest
	temporary        ShardedName
	quarantineReason QuarantineReason
	namespace        SessionNamespaceAuthority
}

func (decision UpdateTemporaryDecision) Action() UpdateTemporaryAction { return decision.action }
func (decision UpdateTemporaryDecision) Target() LocatorDigest         { return decision.target }
func (decision UpdateTemporaryDecision) TemporaryName() ShardedName    { return decision.temporary }
func (decision UpdateTemporaryDecision) QuarantineReason() QuarantineReason {
	return decision.quarantineReason
}

func ReduceUpdateTemporary(
	namespace SessionNamespaceAuthority,
	classified ClassifiedFileShardEntry,
	entry UpdateTemporaryEntryObservation,
	target UpdateTargetObservation,
) (UpdateTemporaryDecision, error) {
	if !namespace.valid() || classified.classification < FileShardEntryRecord ||
		classified.classification > FileShardEntryOpaque ||
		entry < UpdateTemporaryEntryMissing || entry > UpdateTemporaryEntryUnsafe ||
		target < UpdateTargetMissing || target > UpdateTargetInvalid {
		return UpdateTemporaryDecision{}, fmt.Errorf("%w: update temporary observation", ErrInvalidState)
	}
	switch classified.classification {
	case FileShardEntryOpaque:
		if entry == UpdateTemporaryEntryMissing || !classified.locator.IsZero() || !classified.nonce.IsZero() {
			return UpdateTemporaryDecision{}, fmt.Errorf("%w: opaque update temporary carries identity", ErrInvalidState)
		}
		return UpdateTemporaryDecision{
			action: UpdateTemporaryMarkSessionNeedsAttention, namespace: namespace,
		}, nil
	case FileShardEntryMalformedForLocator:
		if entry == UpdateTemporaryEntryMissing || classified.locator.IsZero() || !classified.nonce.IsZero() {
			return UpdateTemporaryDecision{}, fmt.Errorf("%w: malformed update temporary identity", ErrInvalidState)
		}
		return updateTemporaryQuarantineDecision(namespace, classified.locator, target), nil
	case FileShardEntryUpdateTemporary:
		if classified.locator.IsZero() || classified.nonce.IsZero() {
			return UpdateTemporaryDecision{}, fmt.Errorf("%w: canonical update temporary identity", ErrInvalidState)
		}
		if entry == UpdateTemporaryEntryMissing && target == UpdateTargetValid {
			return UpdateTemporaryDecision{
				action: UpdateTemporaryAcceptInstalledTarget,
				target: classified.locator, temporary: UpdateTemporaryName(classified.locator, classified.nonce),
				namespace: namespace,
			}, nil
		}
		if entry == UpdateTemporaryEntryRegular && target == UpdateTargetValid {
			return UpdateTemporaryDecision{
				action: UpdateTemporaryRemoveAndSyncShard,
				target: classified.locator, temporary: UpdateTemporaryName(classified.locator, classified.nonce),
				namespace: namespace,
			}, nil
		}
		return updateTemporaryQuarantineDecision(namespace, classified.locator, target), nil
	case FileShardEntryRecord:
		return UpdateTemporaryDecision{}, fmt.Errorf("%w: authoritative file record is not an update temporary", ErrInvalidState)
	default:
		return UpdateTemporaryDecision{}, fmt.Errorf("%w: update temporary classification", ErrInvalidState)
	}
}

// AuthorizeRemoval binds a reducer result to the exact fixed entry observed at
// execution time. A random name is not authority to unlink a replacement that
// appeared after the recovery scan.
func (decision UpdateTemporaryDecision) AuthorizeRemoval(
	installedTarget BoundFileRecord,
	shard string,
	name string,
	entry UpdateTemporaryEntryObservation,
) error {
	if decision.action != UpdateTemporaryRemoveAndSyncShard || !installedTarget.valid() ||
		decision.namespace != installedTarget.session.namespace ||
		decision.target != installedTarget.record.locatorDigest || entry != UpdateTemporaryEntryRegular ||
		decision.temporary.shard != shard || decision.temporary.name != name {
		return fmt.Errorf("%w: update temporary removal authority", ErrInvalidState)
	}
	classified := ClassifyFileShardEntry(shard, name)
	if classified.classification != FileShardEntryUpdateTemporary || classified.locator != decision.target ||
		UpdateTemporaryName(classified.locator, classified.nonce) != decision.temporary {
		return fmt.Errorf("%w: update temporary removal identity", ErrInvalidState)
	}
	return nil
}

func updateTemporaryQuarantineDecision(
	namespace SessionNamespaceAuthority,
	targetDigest LocatorDigest,
	targetObservation UpdateTargetObservation,
) UpdateTemporaryDecision {
	action := UpdateTemporaryRetainLocatorQuarantine
	if targetObservation == UpdateTargetValid {
		action = UpdateTemporaryInstallFileQuarantine
	}
	return UpdateTemporaryDecision{
		action: action, target: targetDigest, quarantineReason: QuarantineUpdateTemporary,
		namespace: namespace,
	}
}

// ApplyUpdateTemporaryQuarantine turns a parseable temporary-name hazard into
// durable file-scoped quarantine. The target check prevents an adapter from
// applying a scan result to a different record in the same shard.
func ApplyUpdateTemporaryQuarantine(
	bound BoundFileRecord,
	decision UpdateTemporaryDecision,
) (BoundFileRecord, error) {
	if !bound.valid() || decision.action != UpdateTemporaryInstallFileQuarantine ||
		!decision.namespace.valid() || decision.namespace != bound.session.namespace ||
		decision.target != bound.record.locatorDigest ||
		decision.quarantineReason != QuarantineUpdateTemporary {
		return BoundFileRecord{}, fmt.Errorf("%w: update temporary quarantine authority", ErrInvalidState)
	}
	if bound.record.phase == FileQuarantined {
		return bound, nil
	}
	return bound.transition(FileTransition{
		Next: FileQuarantined, QuarantineReason: decision.quarantineReason,
	})
}
