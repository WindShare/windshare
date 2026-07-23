package catalog

import (
	"crypto/subtle"
	"errors"
	"fmt"
	"strings"
	"unicode"
	"unicode/utf8"

	"golang.org/x/text/cases"
	"golang.org/x/text/unicode/norm"
)

const IdentityBytes = 16

var ErrIdentityLength = errors.New("catalog identity must be exactly 16 bytes")

type (
	ShareInstance       [IdentityBytes]byte
	NodeID              [IdentityBytes]byte
	DirectoryID         [IdentityBytes]byte
	FileID              [IdentityBytes]byte
	DirectoryGeneration [IdentityBytes]byte
	ScanAttemptID       [IdentityBytes]byte
)

func identityFromBytes[T ~[IdentityBytes]byte](raw []byte) (T, error) {
	var value T
	if len(raw) != IdentityBytes {
		return value, fmt.Errorf("%w: got %d", ErrIdentityLength, len(raw))
	}
	copy(value[:], raw)
	return value, nil
}

func ShareInstanceFromBytes(raw []byte) (ShareInstance, error) {
	return identityFromBytes[ShareInstance](raw)
}

func NodeIDFromBytes(raw []byte) (NodeID, error) { return identityFromBytes[NodeID](raw) }

func DirectoryIDFromBytes(raw []byte) (DirectoryID, error) {
	return identityFromBytes[DirectoryID](raw)
}

func FileIDFromBytes(raw []byte) (FileID, error) { return identityFromBytes[FileID](raw) }

func DirectoryGenerationFromBytes(raw []byte) (DirectoryGeneration, error) {
	return identityFromBytes[DirectoryGeneration](raw)
}

func ScanAttemptIDFromBytes(raw []byte) (ScanAttemptID, error) {
	return identityFromBytes[ScanAttemptID](raw)
}

func identityBytes[T ~[IdentityBytes]byte](value T) []byte {
	result := make([]byte, IdentityBytes)
	copy(result, value[:])
	return result
}

func identityZero[T ~[IdentityBytes]byte](value T) bool { return value == T{} }

func identityEqual[T ~[IdentityBytes]byte](left, right T) bool {
	return subtle.ConstantTimeCompare(left[:], right[:]) == 1
}

func (id ShareInstance) Bytes() []byte       { return identityBytes(id) }
func (id NodeID) Bytes() []byte              { return identityBytes(id) }
func (id DirectoryID) Bytes() []byte         { return identityBytes(id) }
func (id FileID) Bytes() []byte              { return identityBytes(id) }
func (id DirectoryGeneration) Bytes() []byte { return identityBytes(id) }
func (id ScanAttemptID) Bytes() []byte       { return identityBytes(id) }

func (id ShareInstance) IsZero() bool       { return identityZero(id) }
func (id NodeID) IsZero() bool              { return identityZero(id) }
func (id DirectoryID) IsZero() bool         { return identityZero(id) }
func (id FileID) IsZero() bool              { return identityZero(id) }
func (id DirectoryGeneration) IsZero() bool { return identityZero(id) }
func (id ScanAttemptID) IsZero() bool       { return identityZero(id) }

func (id ShareInstance) Equal(other ShareInstance) bool             { return identityEqual(id, other) }
func (id NodeID) Equal(other NodeID) bool                           { return identityEqual(id, other) }
func (id DirectoryID) Equal(other DirectoryID) bool                 { return identityEqual(id, other) }
func (id FileID) Equal(other FileID) bool                           { return identityEqual(id, other) }
func (id DirectoryGeneration) Equal(other DirectoryGeneration) bool { return identityEqual(id, other) }
func (id ScanAttemptID) Equal(other ScanAttemptID) bool             { return identityEqual(id, other) }

func (id DirectoryID) NodeID() NodeID { return NodeID(id) }
func (id FileID) NodeID() NodeID      { return NodeID(id) }

const (
	MinChunkSize     = 1 << 10
	DefaultChunkSize = 1 << 20
	MaxChunkSize     = 4 << 20
	MaxSafeInteger   = 1<<53 - 1
	MaxFileSize      = MaxSafeInteger
)

type Capability uint64

const (
	CapabilityCatalog Capability = 1 << iota
	CapabilityRanges
	CapabilityAuthenticatedModifiedTime
	CapabilityShareInstanceRefreshReserved
	CapabilityOfflineObjectReserved
	CapabilityMultiRelayDeliveryReserved
)

const (
	WireVersionV2            = 2
	SuiteV2                  = 2
	SenderPublicKeySize      = 32
	MaxDescriptorObjectBytes = 16 << 10
	PathPolicyV1             = "windshare/path/v1-unicode-15.0.0"
	activeCapabilities       = CapabilityCatalog | CapabilityRanges | CapabilityAuthenticatedModifiedTime
)

type DescriptorSpec struct {
	WireVersion      uint32
	Suite            uint32
	ShareInstance    ShareInstance
	SyntheticRoot    DirectoryID
	RootCommit       CommittedRoot
	ChunkSize        uint32
	Capabilities     Capability
	SenderPublicKey  []byte
	CreatedAtSeconds uint64
	PathPolicy       string
}

type ShareDescriptor struct {
	wireVersion      uint32
	suite            uint32
	shareInstance    ShareInstance
	syntheticRoot    DirectoryID
	chunkSize        uint32
	capabilities     Capability
	senderPublicKey  []byte
	createdAtSeconds uint64
	pathPolicy       string
}

type ReceivedDescriptorSpec struct {
	WireVersion      uint32
	Suite            uint32
	ShareInstance    ShareInstance
	SyntheticRoot    DirectoryID
	ChunkSize        uint32
	Capabilities     Capability
	SenderPublicKey  []byte
	CreatedAtSeconds uint64
	PathPolicy       string
}

func NewShareDescriptor(spec DescriptorSpec) (ShareDescriptor, error) {
	committedShare, committedDirectory, _, err := spec.RootCommit.binding()
	if err != nil {
		return ShareDescriptor{}, fmt.Errorf("catalog descriptor requires an atomic synthetic-root commit: %w", err)
	}
	if spec.WireVersion != WireVersionV2 || spec.Suite != SuiteV2 || spec.ShareInstance != committedShare || spec.SyntheticRoot != committedDirectory {
		return ShareDescriptor{}, errors.New("catalog descriptor requires the v2 suite and identities authorized by its committed root")
	}
	return newDescriptorValue(ReceivedDescriptorSpec{
		WireVersion: spec.WireVersion, Suite: spec.Suite, ShareInstance: spec.ShareInstance,
		SyntheticRoot: spec.SyntheticRoot, ChunkSize: spec.ChunkSize, Capabilities: spec.Capabilities,
		SenderPublicKey: spec.SenderPublicKey, CreatedAtSeconds: spec.CreatedAtSeconds, PathPolicy: spec.PathPolicy,
	})
}

// NewReceivedShareDescriptor validates an authenticated descriptor without
// manufacturing the sender-local CommittedRoot capability required to register.
func NewReceivedShareDescriptor(spec ReceivedDescriptorSpec) (ShareDescriptor, error) {
	if spec.WireVersion != WireVersionV2 || spec.Suite != SuiteV2 || spec.ShareInstance.IsZero() || spec.SyntheticRoot.IsZero() {
		return ShareDescriptor{}, errors.New("catalog received descriptor has invalid v2 identities")
	}
	return newDescriptorValue(spec)
}

func newDescriptorValue(spec ReceivedDescriptorSpec) (ShareDescriptor, error) {
	if spec.ChunkSize < MinChunkSize || spec.ChunkSize > MaxChunkSize || spec.ChunkSize&(spec.ChunkSize-1) != 0 {
		return ShareDescriptor{}, fmt.Errorf("catalog chunk size %d is outside the supported power-of-two range", spec.ChunkSize)
	}
	if len(spec.SenderPublicKey) != SenderPublicKeySize {
		return ShareDescriptor{}, fmt.Errorf("catalog descriptor sender public key must be %d bytes", SenderPublicKeySize)
	}
	if spec.Capabilities&^activeCapabilities != 0 {
		return ShareDescriptor{}, errors.New("catalog descriptor contains an unknown or not-yet-enabled capability bit")
	}
	if spec.CreatedAtSeconds > MaxSafeInteger {
		return ShareDescriptor{}, errors.New("catalog descriptor creation time exceeds the safe integer range")
	}
	if spec.PathPolicy != PathPolicyV1 {
		return ShareDescriptor{}, errors.New("catalog descriptor uses an unsupported path policy")
	}
	return ShareDescriptor{
		wireVersion: spec.WireVersion, suite: spec.Suite, shareInstance: spec.ShareInstance,
		syntheticRoot: spec.SyntheticRoot, chunkSize: spec.ChunkSize, capabilities: spec.Capabilities,
		senderPublicKey: append([]byte(nil), spec.SenderPublicKey...), createdAtSeconds: spec.CreatedAtSeconds,
		pathPolicy: spec.PathPolicy,
	}, nil
}

func (d ShareDescriptor) WireVersion() uint32          { return d.wireVersion }
func (d ShareDescriptor) Suite() uint32                { return d.suite }
func (d ShareDescriptor) ShareInstance() ShareInstance { return d.shareInstance }
func (d ShareDescriptor) SyntheticRoot() DirectoryID   { return d.syntheticRoot }
func (d ShareDescriptor) ChunkSize() uint32            { return d.chunkSize }
func (d ShareDescriptor) Capabilities() Capability     { return d.capabilities }
func (d ShareDescriptor) SenderPublicKey() []byte      { return append([]byte(nil), d.senderPublicKey...) }
func (d ShareDescriptor) CreatedAtSeconds() uint64     { return d.createdAtSeconds }
func (d ShareDescriptor) PathPolicy() string           { return d.pathPolicy }
func (d ShareDescriptor) BlockCountFieldPresent() bool { return false }

type TimePrecision uint8

const (
	TimePrecisionUnknown TimePrecision = iota
	TimePrecisionSeconds
	TimePrecisionMilliseconds
	TimePrecisionNanoseconds
)

type ModifiedTime struct {
	seconds     int64
	nanoseconds uint32
	precision   TimePrecision
	present     bool
}

func NewModifiedTime(seconds int64, nanoseconds uint32, precision TimePrecision) (ModifiedTime, error) {
	if precision < TimePrecisionSeconds || precision > TimePrecisionNanoseconds {
		return ModifiedTime{}, errors.New("catalog modified time has unknown precision")
	}
	if seconds < -MaxSafeInteger || seconds > MaxSafeInteger || nanoseconds >= 1_000_000_000 {
		return ModifiedTime{}, errors.New("catalog modified time is outside its portable range")
	}
	switch precision {
	case TimePrecisionSeconds:
		if nanoseconds != 0 {
			return ModifiedTime{}, errors.New("catalog second-precision modified time contains fractional seconds")
		}
	case TimePrecisionMilliseconds:
		if nanoseconds%1_000_000 != 0 {
			return ModifiedTime{}, errors.New("catalog millisecond-precision modified time contains sub-millisecond data")
		}
	}
	return ModifiedTime{seconds: seconds, nanoseconds: nanoseconds, precision: precision, present: true}, nil
}

func (m ModifiedTime) Present() bool            { return m.present }
func (m ModifiedTime) Seconds() int64           { return m.seconds }
func (m ModifiedTime) Nanoseconds() uint32      { return m.nanoseconds }
func (m ModifiedTime) Precision() TimePrecision { return m.precision }

type opaqueValue struct{ value string }

const (
	MaxSourceIdentityBytes   = 4 << 10
	MaxVersionCandidateBytes = 4 << 10
)

func newOpaqueValue(kind string, raw []byte, limit int) (opaqueValue, error) {
	if len(raw) == 0 || len(raw) > limit {
		return opaqueValue{}, fmt.Errorf("catalog %s length must be in 1..%d bytes", kind, limit)
	}
	return opaqueValue{value: string(append([]byte(nil), raw...))}, nil
}

func (v opaqueValue) bytes() []byte { return []byte(v.value) }
func (v opaqueValue) isZero() bool  { return v.value == "" }

const (
	MaxRootSlots              = 4_096
	MaxSelectedRootNamesBytes = 1 << 20
)

type RootSlot uint16

type Locator struct {
	rootSlot     RootSlot
	relativePath string
	valid        bool
}
type SourceIdentity struct{ opaqueValue }
type VersionCandidate struct{ opaqueValue }

func NewLocator(rootSlot RootSlot, relativePath string) (Locator, error) {
	if uint64(rootSlot) >= MaxRootSlots {
		return Locator{}, errors.New("catalog locator root slot exceeds the selected-root limit")
	}
	if err := validateSourceLocator(relativePath); err != nil {
		return Locator{}, err
	}
	return Locator{rootSlot: rootSlot, relativePath: relativePath, valid: true}, nil
}

func NewSourceIdentity(raw []byte) (SourceIdentity, error) {
	value, err := newOpaqueValue("source identity", raw, MaxSourceIdentityBytes)
	return SourceIdentity{value}, err
}

func NewVersionCandidate(raw []byte) (VersionCandidate, error) {
	value, err := newOpaqueValue("version candidate", raw, MaxVersionCandidateBytes)
	return VersionCandidate{value}, err
}

func (v SourceIdentity) Bytes() []byte   { return v.bytes() }
func (v VersionCandidate) Bytes() []byte { return v.bytes() }
func (v Locator) RootSlot() RootSlot     { return v.rootSlot }
func (v Locator) RelativePath() string   { return v.relativePath }
func (v Locator) IsZero() bool           { return !v.valid }
func (v SourceIdentity) IsZero() bool    { return v.isZero() }
func (v VersionCandidate) IsZero() bool  { return v.isZero() }

type NodeKind uint8

const (
	NodeKindUnknown NodeKind = iota
	NodeKindDirectory
	NodeKindFile
)

type Entry struct {
	kind         NodeKind
	nodeID       NodeID
	directoryID  DirectoryID
	fileID       FileID
	name         string
	expectedSize uint64
	modified     ModifiedTime
}

func NewFileEntry(id FileID, name string, expectedSize uint64, modified ModifiedTime) (Entry, error) {
	if id.IsZero() || expectedSize > MaxFileSize {
		return Entry{}, errors.New("catalog file entry has invalid identity or size")
	}
	canonical, err := CanonicalName(name)
	if err != nil {
		return Entry{}, err
	}
	return Entry{kind: NodeKindFile, nodeID: id.NodeID(), fileID: id, name: canonical, expectedSize: expectedSize, modified: modified}, nil
}

func NewDirectoryEntry(id DirectoryID, name string, modified ModifiedTime) (Entry, error) {
	if id.IsZero() {
		return Entry{}, errors.New("catalog directory entry has a zero identity")
	}
	canonical, err := CanonicalName(name)
	if err != nil {
		return Entry{}, err
	}
	return Entry{kind: NodeKindDirectory, nodeID: id.NodeID(), directoryID: id, name: canonical, modified: modified}, nil
}

func (e Entry) Kind() NodeKind             { return e.kind }
func (e Entry) NodeID() NodeID             { return e.nodeID }
func (e Entry) Name() string               { return e.name }
func (e Entry) ExpectedSize() uint64       { return e.expectedSize }
func (e Entry) ModifiedTime() ModifiedTime { return e.modified }
func (e Entry) FileID() (FileID, bool)     { return e.fileID, e.kind == NodeKindFile }
func (e Entry) DirectoryID() (DirectoryID, bool) {
	return e.directoryID, e.kind == NodeKindDirectory
}

func (e Entry) valid() bool {
	if e.nodeID.IsZero() || e.name == "" {
		return false
	}
	switch e.kind {
	case NodeKindFile:
		return !e.fileID.IsZero() && e.nodeID == e.fileID.NodeID() && e.expectedSize <= MaxFileSize
	case NodeKindDirectory:
		return !e.directoryID.IsZero() && e.nodeID == e.directoryID.NodeID() && e.expectedSize == 0
	default:
		return false
	}
}

type NodeRecord struct {
	kind             NodeKind
	nodeID           NodeID
	directoryID      DirectoryID
	fileID           FileID
	parent           DirectoryID
	name             string
	locator          Locator
	sourceIdentity   SourceIdentity
	versionCandidate VersionCandidate
	expectedSize     uint64
	modified         ModifiedTime
	syntheticRoot    bool
}

const CatalogNodeMemoryOverhead = uint64(256)

func NewSyntheticRootNodeRecord(id DirectoryID) (NodeRecord, error) {
	if id.IsZero() {
		return NodeRecord{}, errors.New("catalog synthetic root has a zero identity")
	}
	return NodeRecord{kind: NodeKindDirectory, nodeID: id.NodeID(), directoryID: id, syntheticRoot: true}, nil
}

func NewDirectoryNodeRecord(id DirectoryID, parent DirectoryID, name string, locator Locator, sourceIdentity SourceIdentity, modified ModifiedTime) (NodeRecord, error) {
	entry, err := NewDirectoryEntry(id, name, modified)
	if err != nil {
		return NodeRecord{}, err
	}
	if id.NodeID() == parent.NodeID() {
		return NodeRecord{}, errors.New("catalog directory identity collides with its parent")
	}
	if parent.IsZero() || locator.IsZero() || sourceIdentity.IsZero() {
		return NodeRecord{}, errors.New("catalog directory record requires parent and private source metadata")
	}
	return NodeRecord{kind: entry.kind, nodeID: entry.nodeID, directoryID: id, parent: parent, name: entry.name, locator: locator, sourceIdentity: sourceIdentity, modified: modified}, nil
}

func NewFileNodeRecord(id FileID, parent DirectoryID, name string, locator Locator, sourceIdentity SourceIdentity, candidate VersionCandidate, expectedSize uint64, modified ModifiedTime) (NodeRecord, error) {
	entry, err := NewFileEntry(id, name, expectedSize, modified)
	if err != nil {
		return NodeRecord{}, err
	}
	if id.NodeID() == parent.NodeID() {
		return NodeRecord{}, errors.New("catalog file identity collides with its parent")
	}
	if parent.IsZero() || locator.IsZero() || sourceIdentity.IsZero() || candidate.IsZero() {
		return NodeRecord{}, errors.New("catalog file record requires parent and private source metadata")
	}
	return NodeRecord{kind: entry.kind, nodeID: entry.nodeID, fileID: id, parent: parent, name: entry.name, locator: locator, sourceIdentity: sourceIdentity, versionCandidate: candidate, expectedSize: expectedSize, modified: modified}, nil
}

func (n NodeRecord) Kind() NodeKind                     { return n.kind }
func (n NodeRecord) NodeID() NodeID                     { return n.nodeID }
func (n NodeRecord) Parent() DirectoryID                { return n.parent }
func (n NodeRecord) Locator() Locator                   { return n.locator }
func (n NodeRecord) SourceIdentity() SourceIdentity     { return n.sourceIdentity }
func (n NodeRecord) VersionCandidate() VersionCandidate { return n.versionCandidate }
func (n NodeRecord) IsSyntheticRoot() bool              { return n.syntheticRoot }
func (n NodeRecord) FileID() (FileID, bool)             { return n.fileID, n.kind == NodeKindFile }
func (n NodeRecord) DirectoryID() (DirectoryID, bool) {
	return n.directoryID, n.kind == NodeKindDirectory
}

func (n NodeRecord) Entry() Entry {
	if n.syntheticRoot {
		return Entry{}
	}
	return Entry{kind: n.kind, nodeID: n.nodeID, directoryID: n.directoryID, fileID: n.fileID, name: n.name, expectedSize: n.expectedSize, modified: n.modified}
}

func (n NodeRecord) MatchesEntry(entry Entry) bool { return n.Entry() == entry }

func (n NodeRecord) EstimatedMemoryBytes() uint64 {
	return CatalogNodeMemoryOverhead + uint64(
		len(n.name)+len(n.locator.relativePath)+len(n.sourceIdentity.value)+len(n.versionCandidate.value),
	)
}

func (n NodeRecord) valid() bool {
	if n.syntheticRoot {
		return n.kind == NodeKindDirectory && !n.directoryID.IsZero() && n.nodeID == n.directoryID.NodeID()
	}
	return n.Entry().valid() && !n.parent.IsZero() && !n.locator.IsZero() && !n.sourceIdentity.IsZero() && (n.kind != NodeKindFile || !n.versionCandidate.IsZero())
}

const (
	MaxNameBytes             = 255
	MaxPathBytes             = 32 * 1024
	MaxPathDepth             = 256
	reservedOutputRootPrefix = ".wsresume"
)

var (
	ErrInvalidName = errors.New("invalid catalog name")
	ErrInvalidPath = errors.New("invalid catalog path")
	ErrPathTooDeep = errors.New("catalog path exceeds maximum depth")
)

var windowsReservedNames = map[string]struct{}{
	"CON": {}, "PRN": {}, "AUX": {}, "NUL": {}, "CONIN$": {}, "CONOUT$": {},
	"COM1": {}, "COM2": {}, "COM3": {}, "COM4": {}, "COM5": {}, "COM6": {}, "COM7": {}, "COM8": {}, "COM9": {},
	"LPT1": {}, "LPT2": {}, "LPT3": {}, "LPT4": {}, "LPT5": {}, "LPT6": {}, "LPT7": {}, "LPT8": {}, "LPT9": {},
	"COM¹": {}, "COM²": {}, "COM³": {}, "LPT¹": {}, "LPT²": {}, "LPT³": {},
}

// CanonicalName normalizes Unicode before validation so equality and ordering are
// stable across filesystems that expose different normalization forms.
func CanonicalName(name string) (string, error) {
	if !utf8.ValidString(name) {
		return "", fmt.Errorf("%w: invalid UTF-8", ErrInvalidName)
	}
	canonical := norm.NFC.String(name)
	if canonical == "" || canonical == "." || canonical == ".." {
		return "", fmt.Errorf("%w: empty or relative component", ErrInvalidName)
	}
	if !utf8.ValidString(canonical) || len(canonical) > MaxNameBytes {
		return "", fmt.Errorf("%w: invalid UTF-8 or name too long", ErrInvalidName)
	}
	if strings.ContainsAny(canonical, "/\\") {
		return "", fmt.Errorf("%w: separators are not allowed", ErrInvalidName)
	}
	if strings.HasSuffix(canonical, ".") || strings.HasSuffix(canonical, " ") {
		return "", fmt.Errorf("%w: trailing dots and spaces are not portable", ErrInvalidName)
	}
	for _, r := range canonical {
		if unicode.IsControl(r) || unicode.In(r, unicode.Cf) || strings.ContainsRune(`<>:"|?*`, r) {
			return "", fmt.Errorf("%w: contains a non-portable character", ErrInvalidName)
		}
	}
	stem := canonical
	if dot := strings.IndexByte(stem, '.'); dot >= 0 {
		stem = stem[:dot]
	}
	if _, reserved := windowsReservedNames[strings.ToUpper(stem)]; reserved {
		return "", fmt.Errorf("%w: reserved device name", ErrInvalidName)
	}
	return canonical, nil
}

func CanonicalPath(path string) (string, error) {
	if !utf8.ValidString(path) {
		return "", fmt.Errorf("%w: invalid UTF-8", ErrInvalidPath)
	}
	canonical := norm.NFC.String(path)
	if canonical == "" || !utf8.ValidString(canonical) || len(canonical) > MaxPathBytes {
		return "", fmt.Errorf("%w: empty, invalid UTF-8, or too long", ErrInvalidPath)
	}
	if strings.ContainsRune(canonical, '\\') || strings.HasPrefix(canonical, "/") || strings.HasSuffix(canonical, "/") {
		return "", fmt.Errorf("%w: path must be relative and slash-separated", ErrInvalidPath)
	}
	components := strings.Split(canonical, "/")
	if len(components) > MaxPathDepth {
		return "", fmt.Errorf("%w: got %d components", ErrPathTooDeep, len(components))
	}
	for index, component := range components {
		validated, err := CanonicalName(component)
		if err != nil {
			return "", fmt.Errorf("%w: component %d: %w", ErrInvalidPath, index, err)
		}
		components[index] = validated
	}
	if reservedOutputRootName(components[0]) {
		return "", fmt.Errorf("%w: root component uses the reserved %q prefix", ErrInvalidPath, reservedOutputRootPrefix)
	}
	return strings.Join(components, "/"), nil
}

// validateSourceLocator applies the portable containment policy without
// rewriting filesystem spelling. Public catalog names are NFC, but replacing a
// sender-private decomposed component with its NFC form can point at a different
// inode (or no inode) on filesystems whose lookup is byte-sensitive.
func validateSourceLocator(path string) error {
	if path == "" {
		return nil
	}
	if !utf8.ValidString(path) || len(path) > MaxPathBytes || strings.ContainsRune(path, '\\') ||
		strings.HasPrefix(path, "/") || strings.HasSuffix(path, "/") {
		return fmt.Errorf("%w: locator must be relative, valid UTF-8, and slash-separated", ErrInvalidPath)
	}
	components := strings.Split(path, "/")
	if len(components) > MaxPathDepth {
		return fmt.Errorf("%w: got %d components", ErrPathTooDeep, len(components))
	}
	for index, component := range components {
		if len(component) > MaxNameBytes {
			return fmt.Errorf("%w: component %d is too long", ErrInvalidPath, index)
		}
		if _, err := CanonicalName(component); err != nil {
			return fmt.Errorf("%w: component %d: %w", ErrInvalidPath, index, err)
		}
	}
	return nil
}

func siblingCollisionKey(name string) string {
	return norm.NFC.String(cases.Fold().String(name))
}

func reservedOutputRootName(name string) bool {
	return strings.HasPrefix(siblingCollisionKey(name), reservedOutputRootPrefix)
}
