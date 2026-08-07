package resumestate

import (
	"encoding/hex"
	"fmt"
	"strings"

	"github.com/windshare/windshare/core/transfer"
)

const (
	ControlDirectoryName     = ".windshare-output"
	SessionsDirectoryName    = "sessions"
	BootstrapCandidatePrefix = ".windshare-output.bootstrap-"
	SessionCandidatePrefix   = ".candidate-"
	FilesDirectoryName       = "files"
	AnchorsDirectoryName     = "anchors"
	StagesDirectoryName      = "stages"
	// CheckpointsDirectoryName is a file-local namespace. It is intentionally a
	// sibling of the legacy v3 state directories so a restart can inspect only
	// authenticated FileCheckpointV1 records without treating a v3 record as
	// resume authority.
	CheckpointsDirectoryName = "checkpoints-v1"
	HeaderRecordName         = "header.state"
	SessionLockName          = "session.lock"
	CoordinatorLockName      = "coordinator.lock"
	ControlRecordName        = "control.state"
	ShardHexCharacters       = 2
	encodedSHA256Characters  = 64
	encodedSessionCharacters = transfer.OutputSessionIdentityBytes * 2
)

type ShardedName struct {
	shard string
	name  string
}

type IntentNamespaceClassification uint8

const (
	IntentNamespaceCanonical IntentNamespaceClassification = iota + 1
	IntentNamespaceDecodableAlias
	IntentNamespaceOpaque
)

type ClassifiedIntentNamespace struct {
	classification IntentNamespaceClassification
	intent         transfer.TransferIntentDigest
}

func (classified ClassifiedIntentNamespace) Classification() IntentNamespaceClassification {
	return classified.classification
}
func (classified ClassifiedIntentNamespace) Intent() transfer.TransferIntentDigest {
	return classified.intent
}

// ClassifyIntentNamespaceName preserves the intent behind a decodable but
// non-canonical spelling. Linux must block that matching intent rather than let
// an uppercase alias coexist with the canonical directory.
func ClassifyIntentNamespaceName(name string) ClassifiedIntentNamespace {
	if intent, err := ParseIntentNamespaceName(name); err == nil {
		return ClassifiedIntentNamespace{classification: IntentNamespaceCanonical, intent: intent}
	}
	if len(name) != encodedSHA256Characters {
		return ClassifiedIntentNamespace{classification: IntentNamespaceOpaque}
	}
	raw, err := hex.DecodeString(name)
	if err != nil {
		return ClassifiedIntentNamespace{classification: IntentNamespaceOpaque}
	}
	intent, err := transfer.TransferIntentDigestFromBytes(raw)
	if err != nil {
		return ClassifiedIntentNamespace{classification: IntentNamespaceOpaque}
	}
	return ClassifiedIntentNamespace{classification: IntentNamespaceDecodableAlias, intent: intent}
}

func (name ShardedName) Shard() string { return name.shard }
func (name ShardedName) Name() string  { return name.name }

func IntentNamespaceName(intent transfer.TransferIntentDigest) string {
	return hex.EncodeToString(intent.Bytes())
}

func SessionDirectoryName(session transfer.OutputSessionID) string {
	return hex.EncodeToString(session.Bytes())
}

func SessionCandidateName(session transfer.OutputSessionID) string {
	return SessionCandidatePrefix + SessionDirectoryName(session)
}

func BootstrapCandidateName(nonce BootstrapNonce) string {
	return BootstrapCandidatePrefix + nonce.String()
}

func FileRecordName(digest LocatorDigest) ShardedName {
	return shardedName(digest.String(), ".state")
}

func FileCheckpointName(recordID FileCheckpointRecordID) ShardedName {
	return shardedName(hex.EncodeToString(recordID[:]), ".checkpoint")
}

func AnchorName(id OutputObjectID) ShardedName { return shardedName(id.String(), ".anchor") }
func StageName(id OutputObjectID) ShardedName  { return shardedName(id.String(), ".stage") }

func shardedName(encoded, suffix string) ShardedName {
	return ShardedName{shard: encoded[:ShardHexCharacters], name: encoded + suffix}
}

// SessionDirectorySegments are relative to the control directory. Returning
// components rather than an OS path keeps callers on handle-relative APIs and
// prevents separator interpretation from becoming part of the state format.
func SessionDirectorySegments(intent transfer.TransferIntentDigest, session transfer.OutputSessionID) []string {
	return []string{SessionsDirectoryName, IntentNamespaceName(intent), SessionDirectoryName(session)}
}

func FileRecordSegments(digest LocatorDigest) []string {
	name := FileRecordName(digest)
	return []string{FilesDirectoryName, name.shard, name.name}
}

func AnchorSegments(id OutputObjectID) []string {
	name := AnchorName(id)
	return []string{AnchorsDirectoryName, name.shard, name.name}
}

func StageSegments(id OutputObjectID) []string {
	name := StageName(id)
	return []string{StagesDirectoryName, name.shard, name.name}
}

func ParseIntentNamespaceName(name string) (transfer.TransferIntentDigest, error) {
	if !validLowerHex(name, encodedSHA256Characters) {
		return transfer.TransferIntentDigest{}, fmt.Errorf("%w: intent namespace name", ErrInvalidState)
	}
	raw, _ := hex.DecodeString(name)
	intent, err := transfer.TransferIntentDigestFromBytes(raw)
	if err != nil {
		return transfer.TransferIntentDigest{}, fmt.Errorf("%w: intent namespace identity", ErrInvalidState)
	}
	return intent, nil
}

func ParseBootstrapCandidateName(name string) (BootstrapNonce, error) {
	if !strings.HasPrefix(name, BootstrapCandidatePrefix) {
		return BootstrapNonce{}, fmt.Errorf("%w: bootstrap candidate name", ErrInvalidState)
	}
	encoded := strings.TrimPrefix(name, BootstrapCandidatePrefix)
	if !validLowerHex(encoded, encodedSHA256Characters) {
		return BootstrapNonce{}, fmt.Errorf("%w: bootstrap candidate nonce", ErrInvalidState)
	}
	raw, _ := hex.DecodeString(encoded)
	return BootstrapNonceFromBytes(raw)
}

func ParseFileRecordName(shard, name string) (LocatorDigest, error) {
	raw, err := parseShardedHex(shard, name, ".state", encodedSHA256Characters)
	if err != nil {
		return LocatorDigest{}, err
	}
	return LocatorDigestFromBytes(raw)
}

func ParseFileCheckpointName(shard, name string) (FileCheckpointRecordID, error) {
	raw, err := parseShardedHex(shard, name, ".checkpoint", encodedSHA256Characters)
	if err != nil {
		return FileCheckpointRecordID{}, err
	}
	return FileCheckpointRecordIDFromBytes(raw)
}

func ParseAnchorName(shard, name string) (OutputObjectID, error) {
	return parseOutputObjectName(shard, name, ".anchor")
}

func ParseStageName(shard, name string) (OutputObjectID, error) {
	return parseOutputObjectName(shard, name, ".stage")
}

func parseOutputObjectName(shard, name, suffix string) (OutputObjectID, error) {
	raw, err := parseShardedHex(shard, name, suffix, encodedSHA256Characters)
	if err != nil {
		return OutputObjectID{}, err
	}
	return OutputObjectIDFromBytes(raw)
}

func ParseSessionDirectoryName(name string) (transfer.OutputSessionID, error) {
	if !validLowerHex(name, encodedSessionCharacters) {
		return transfer.OutputSessionID{}, fmt.Errorf("%w: session directory name", ErrInvalidState)
	}
	raw, _ := hex.DecodeString(name)
	session, err := transfer.OutputSessionIDFromBytes(raw)
	if err != nil {
		return transfer.OutputSessionID{}, fmt.Errorf("%w: session directory identity", ErrInvalidState)
	}
	return session, nil
}

func ParseSessionCandidateName(name string) (transfer.OutputSessionID, error) {
	if !strings.HasPrefix(name, SessionCandidatePrefix) {
		return transfer.OutputSessionID{}, fmt.Errorf("%w: session candidate name", ErrInvalidState)
	}
	return ParseSessionDirectoryName(strings.TrimPrefix(name, SessionCandidatePrefix))
}

func parseShardedHex(shard, name, suffix string, encodedLength int) ([]byte, error) {
	if len(shard) != ShardHexCharacters || !strings.HasSuffix(name, suffix) {
		return nil, fmt.Errorf("%w: malformed sharded name", ErrInvalidState)
	}
	base := strings.TrimSuffix(name, suffix)
	if !validLowerHex(shard, ShardHexCharacters) || !validLowerHex(base, encodedLength) ||
		!strings.HasPrefix(base, shard) {
		return nil, fmt.Errorf("%w: malformed or misplaced sharded name", ErrInvalidState)
	}
	raw, err := hex.DecodeString(base)
	if err != nil {
		return nil, fmt.Errorf("%w: sharded name encoding", ErrInvalidState)
	}
	return raw, nil
}

func validLowerHex(value string, length int) bool {
	if len(value) != length {
		return false
	}
	for _, character := range value {
		if character < '0' || character > '9' {
			if character < 'a' || character > 'f' {
				return false
			}
		}
	}
	return true
}

type RecordKind uint8

const (
	RecordControlDirectoryBinding RecordKind = iota + 1
	RecordGlobalControl
	RecordCoordinatorLock
	RecordSessionsDirectory
)

type CorruptionDisposition uint8

const (
	CorruptionRetainOpaque CorruptionDisposition = iota + 1
	CorruptionQuarantineFile
	CorruptionBlockResumeNamespace
	CorruptionBlockOutputRoot
)

type CorruptionClassification struct {
	disposition CorruptionDisposition
	intent      transfer.TransferIntentDigest
	locator     LocatorDigest
}

func (classification CorruptionClassification) Disposition() CorruptionDisposition {
	return classification.disposition
}
func (classification CorruptionClassification) Intent() transfer.TransferIntentDigest {
	return classification.intent
}
func (classification CorruptionClassification) Locator() LocatorDigest {
	return classification.locator
}

func ClassifyGlobalCorruption(kind RecordKind) (CorruptionClassification, error) {
	switch kind {
	case RecordControlDirectoryBinding, RecordGlobalControl, RecordCoordinatorLock, RecordSessionsDirectory:
		return CorruptionClassification{disposition: CorruptionBlockOutputRoot}, nil
	default:
		return CorruptionClassification{}, fmt.Errorf("%w: global corruption kind", ErrInvalidState)
	}
}

func ClassifyHeaderCorruption(intentDirectory string) CorruptionClassification {
	namespace := ClassifyIntentNamespaceName(intentDirectory)
	if namespace.classification == IntentNamespaceOpaque {
		return CorruptionClassification{disposition: CorruptionRetainOpaque}
	}
	return CorruptionClassification{
		disposition: CorruptionBlockResumeNamespace, intent: namespace.intent,
	}
}

type FileShardEntryClassification uint8

const (
	FileShardEntryRecord FileShardEntryClassification = iota + 1
	FileShardEntryUpdateTemporary
	FileShardEntryMalformedForLocator
	FileShardEntryOpaque
)

type ClassifiedFileShardEntry struct {
	classification FileShardEntryClassification
	locator        LocatorDigest
	nonce          UpdateNonce
}

func (classified ClassifiedFileShardEntry) Classification() FileShardEntryClassification {
	return classified.classification
}
func (classified ClassifiedFileShardEntry) Locator() LocatorDigest { return classified.locator }
func (classified ClassifiedFileShardEntry) Nonce() UpdateNonce     { return classified.nonce }

// ClassifyFileShardEntry is the only parser for entries in a file-state shard.
// Checking update temporaries before the malformed fallback prevents one fixed
// entry from authorizing both cleanup and quarantine under different parsers.
func ClassifyFileShardEntry(shard, name string) ClassifiedFileShardEntry {
	if locator, err := ParseFileRecordName(shard, name); err == nil {
		return ClassifiedFileShardEntry{classification: FileShardEntryRecord, locator: locator}
	}
	if temporary := classifyUpdateTemporaryName(shard, name); temporary.classification != FileShardEntryOpaque {
		return temporary
	}
	if len(name) < encodedSHA256Characters {
		return ClassifiedFileShardEntry{classification: FileShardEntryOpaque}
	}
	raw, err := hex.DecodeString(name[:encodedSHA256Characters])
	if err != nil {
		return ClassifiedFileShardEntry{classification: FileShardEntryOpaque}
	}
	locator, err := LocatorDigestFromBytes(raw)
	if err != nil {
		return ClassifiedFileShardEntry{classification: FileShardEntryOpaque}
	}
	return ClassifiedFileShardEntry{
		classification: FileShardEntryMalformedForLocator, locator: locator,
	}
}

func ClassifyFileCorruption(
	namespace SessionNamespaceAuthority,
	classified ClassifiedFileShardEntry,
) (CorruptionClassification, error) {
	if !namespace.valid() || classified.classification < FileShardEntryRecord ||
		classified.classification > FileShardEntryOpaque {
		return CorruptionClassification{}, fmt.Errorf("%w: file corruption namespace", ErrInvalidState)
	}
	switch classified.classification {
	case FileShardEntryOpaque:
		return CorruptionClassification{
			disposition: CorruptionRetainOpaque, intent: namespace.header.intentDigest,
		}, nil
	case FileShardEntryRecord, FileShardEntryMalformedForLocator:
		return CorruptionClassification{
			disposition: CorruptionQuarantineFile, intent: namespace.header.intentDigest,
			locator: classified.locator,
		}, nil
	case FileShardEntryUpdateTemporary:
		return CorruptionClassification{}, fmt.Errorf("%w: update temporary is not corrupt file state", ErrInvalidState)
	default:
		return CorruptionClassification{}, fmt.Errorf("%w: file shard entry classification", ErrInvalidState)
	}
}
