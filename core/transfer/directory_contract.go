package transfer

import (
	"crypto/hmac"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/binary"
	"strings"

	"github.com/windshare/windshare/core/catalog"
	"github.com/windshare/windshare/core/transfer/fault"
	"github.com/windshare/windshare/core/transfer/receivecontract"
)

const (
	DirectoryAdmissionV2       uint8 = 2
	DirectoryAdmissionLayoutV1 uint8 = 1

	directoryAdmissionTokenBytes  = sha256.Size
	directoryAdmissionSecretBytes = sha256.Size
	directoryAdmissionDomain      = "windshare/directory-admission/v2"
)

type DirectoryAdmissionLayout uint8

const (
	DirectoryAdmissionTreeSingleFile DirectoryAdmissionLayout = iota + 1
	DirectoryAdmissionTreeResultRoot
	DirectoryAdmissionTreeCatalogRoot
	DirectoryAdmissionZipResultRoot
)

// DirectoryAdmission is an immutable, runtime-only capability for one frozen
// directory claim. The token authenticates the fields but cannot be imported
// from bytes, preventing persisted or foreign data from manufacturing authority.
type DirectoryAdmission struct {
	version       uint8
	intent        ReceiveIntentDigest
	layoutVersion uint8
	layout        DirectoryAdmissionLayout
	token         [directoryAdmissionTokenBytes]byte
	directory     catalog.DirectoryID
	generation    catalog.DirectoryGeneration
	parent        [directoryAdmissionTokenBytes]byte
	path          string
	modified      catalog.ModifiedTime
}

// DirectoryAdmissionScope is the one-time projection of an already validated
// intent needed by the receipt codec. Keeping full intent validation out of the
// per-directory path prevents selection size from becoming admission cost.
type DirectoryAdmissionScope struct {
	intent        ReceiveIntentDigest
	layoutVersion uint8
	layout        DirectoryAdmissionLayout
	syntheticRoot catalog.DirectoryID
}

func NewDirectoryAdmissionScope(intent ReceiveIntent) (DirectoryAdmissionScope, error) {
	if !intent.valid() {
		return DirectoryAdmissionScope{}, ErrInvalidDirectoryAdmission
	}
	artifact := intent.ArtifactSpec()
	plan := intent.MaterializationPlan()
	var layout DirectoryAdmissionLayout
	switch plan.Kind() {
	case receivecontract.PlanDirectTree:
		tree, ok := artifact.DirectoryTree()
		if !ok {
			return DirectoryAdmissionScope{}, ErrInvalidDirectoryAdmission
		}
		switch tree.Kind() {
		case receivecontract.DirectoryTreeSingleFile:
			layout = DirectoryAdmissionTreeSingleFile
		case receivecontract.DirectoryTreeResultRoot:
			layout = DirectoryAdmissionTreeResultRoot
		case receivecontract.DirectoryTreeCatalogRoot:
			layout = DirectoryAdmissionTreeCatalogRoot
		default:
			return DirectoryAdmissionScope{}, ErrInvalidDirectoryAdmission
		}
	case receivecontract.PlanDirectAtomic:
		if artifact.Kind() != receivecontract.ArtifactZipArchive {
			return DirectoryAdmissionScope{}, ErrInvalidDirectoryAdmission
		}
		layout = DirectoryAdmissionZipResultRoot
	default:
		return DirectoryAdmissionScope{}, ErrInvalidDirectoryAdmission
	}
	return DirectoryAdmissionScope{
		intent: intent.Digest(), layoutVersion: DirectoryAdmissionLayoutV1,
		layout: layout, syntheticRoot: intent.SyntheticRoot(),
	}, nil
}

func (scope DirectoryAdmissionScope) ReceiveIntentDigest() ReceiveIntentDigest { return scope.intent }
func (scope DirectoryAdmissionScope) LayoutVersion() uint8                     { return scope.layoutVersion }
func (scope DirectoryAdmissionScope) Layout() DirectoryAdmissionLayout         { return scope.layout }
func (scope DirectoryAdmissionScope) SyntheticRoot() catalog.DirectoryID       { return scope.syntheticRoot }

func (scope DirectoryAdmissionScope) valid() bool {
	return !scope.intent.IsZero() && scope.layoutVersion == DirectoryAdmissionLayoutV1 &&
		scope.layout >= DirectoryAdmissionTreeSingleFile && scope.layout <= DirectoryAdmissionZipResultRoot &&
		!scope.syntheticRoot.IsZero()
}

func (admission DirectoryAdmission) SchemaVersion() uint8 { return admission.version }
func (admission DirectoryAdmission) ReceiveIntentDigest() ReceiveIntentDigest {
	return admission.intent
}
func (admission DirectoryAdmission) LayoutVersion() uint8             { return admission.layoutVersion }
func (admission DirectoryAdmission) Layout() DirectoryAdmissionLayout { return admission.layout }
func (admission DirectoryAdmission) DirectoryID() catalog.DirectoryID { return admission.directory }
func (admission DirectoryAdmission) Generation() catalog.DirectoryGeneration {
	return admission.generation
}
func (admission DirectoryAdmission) Path() string                       { return admission.path }
func (admission DirectoryAdmission) ModifiedTime() catalog.ModifiedTime { return admission.modified }

func (admission DirectoryAdmission) IsZero() bool {
	return admission.token == [directoryAdmissionTokenBytes]byte{}
}

func (admission DirectoryAdmission) Bytes() []byte {
	return append([]byte(nil), admission.token[:]...)
}

func (admission DirectoryAdmission) ParentToken() []byte {
	if admission.parent == [directoryAdmissionTokenBytes]byte{} {
		return nil
	}
	return append([]byte(nil), admission.parent[:]...)
}

// Equal compares only authenticated receipt material. Constant-time comparison
// prevents receipt validity from becoming a byte-prefix oracle at adapter edges.
func (admission DirectoryAdmission) Equal(other DirectoryAdmission) bool {
	return !admission.IsZero() && !other.IsZero() &&
		subtle.ConstantTimeCompare(admission.token[:], other.token[:]) == 1
}

// NewDirectoryAdmissionWithSecret mints a receipt under one already-validated
// immutable intent scope. The caller must create a fresh secret only after
// output/root authority is valid; binding the digest here prevents a session
// key from being reused accidentally across two durable output namespaces.
func NewDirectoryAdmissionWithSecret(
	secret []byte,
	scope DirectoryAdmissionScope,
	directory MaterializationDirectory,
) (DirectoryAdmission, error) {
	if !validDirectoryAdmissionSecret(secret) {
		return DirectoryAdmission{}, ErrInvalidDirectoryAdmission
	}
	message, err := CanonicalDirectoryAdmissionMessageV2(scope, directory)
	if err != nil {
		return DirectoryAdmission{}, err
	}
	mac := hmac.New(sha256.New, secret)
	_, _ = mac.Write(message)
	var token [directoryAdmissionTokenBytes]byte
	copy(token[:], mac.Sum(nil))
	return DirectoryAdmission{
		version:       DirectoryAdmissionV2,
		intent:        scope.ReceiveIntentDigest(),
		layoutVersion: scope.LayoutVersion(),
		layout:        scope.Layout(),
		token:         token,
		directory:     directory.DirectoryID,
		generation:    directory.Generation,
		parent:        directory.ParentAdmission.token,
		path:          directory.Path,
		modified:      directory.ModifiedTime,
	}, nil
}

// CanonicalDirectoryAdmissionMessageV2 returns the exact HMAC message shared by
// Go and Web. The secret is intentionally absent: it is the HMAC key, not one of
// the framed caller-controlled claim fields.
func CanonicalDirectoryAdmissionMessageV2(
	scope DirectoryAdmissionScope,
	directory MaterializationDirectory,
) ([]byte, error) {
	if err := validateMaterializationDirectoryForScope(scope, directory); err != nil {
		return nil, err
	}
	modified := canonicalDirectoryAdmissionModifiedTime(directory.ModifiedTime)
	path := canonicalDirectoryAdmissionPath(directory.Path)
	message := make([]byte, 0, 10*8+2+len(directoryAdmissionDomain)+
		ReceiveIntentDigestBytes+2*catalog.IdentityBytes+directoryAdmissionTokenBytes+
		len(directory.Path)+len(modified))
	message = append(message, directoryAdmissionDomain...)
	message = append(message, 0, DirectoryAdmissionV2)
	message = appendCanonicalField(message, scope.ReceiveIntentDigest().Bytes())
	message = appendCanonicalField(message, []byte{scope.LayoutVersion()})
	message = appendCanonicalField(message, []byte{byte(scope.Layout())})
	message = appendDirectoryAdmissionFrame(message, directory.DirectoryID.Bytes())
	message = appendDirectoryAdmissionFrame(message, directory.Generation.Bytes())
	if directory.ParentAdmission.IsZero() {
		message = appendDirectoryAdmissionFrame(message, nil)
	} else {
		message = appendDirectoryAdmissionFrame(message, directory.ParentAdmission.token[:])
	}
	message = appendDirectoryAdmissionFrame(message, path)
	message = appendDirectoryAdmissionFrame(message, modified)
	return message, nil
}

func validateMaterializationDirectoryForScope(scope DirectoryAdmissionScope, directory MaterializationDirectory) error {
	if !scope.valid() || directory.DirectoryID.IsZero() || directory.Generation.IsZero() {
		return ErrInvalidDirectoryAdmission
	}
	if directory.Path == "" {
		if !directory.ParentAdmission.IsZero() || directory.DirectoryID != scope.SyntheticRoot() {
			return ErrInvalidDirectoryAdmission
		}
		return nil
	}
	canonical, err := catalog.CanonicalPath(directory.Path)
	if err != nil || canonical != directory.Path || directory.ParentAdmission.IsZero() ||
		!directory.ParentAdmission.validSnapshot() ||
		directory.ParentAdmission.intent != scope.ReceiveIntentDigest() ||
		directory.ParentAdmission.layoutVersion != scope.LayoutVersion() ||
		directory.ParentAdmission.layout != scope.Layout() ||
		!immediateDirectoryChild(directory.ParentAdmission.path, directory.Path) {
		return ErrInvalidDirectoryAdmission
	}
	return nil
}

func immediateDirectoryChild(parent, child string) bool {
	separator := strings.LastIndexByte(child, '/')
	if separator < 0 {
		return parent == ""
	}
	return parent == child[:separator]
}

func (admission DirectoryAdmission) validSnapshot() bool {
	if admission.version != DirectoryAdmissionV2 || admission.intent.IsZero() || admission.IsZero() ||
		admission.layoutVersion != DirectoryAdmissionLayoutV1 ||
		admission.layout < DirectoryAdmissionTreeSingleFile || admission.layout > DirectoryAdmissionZipResultRoot ||
		admission.directory.IsZero() || admission.generation.IsZero() {
		return false
	}
	if admission.path == "" {
		return admission.parent == [directoryAdmissionTokenBytes]byte{}
	}
	canonical, err := catalog.CanonicalPath(admission.path)
	return err == nil && canonical == admission.path &&
		admission.parent != [directoryAdmissionTokenBytes]byte{}
}

func admissionMatchesDirectory(
	scope DirectoryAdmissionScope,
	admission DirectoryAdmission,
	directory MaterializationDirectory,
) bool {
	if !admission.validSnapshot() || validateMaterializationDirectoryForScope(scope, directory) != nil ||
		admission.version != DirectoryAdmissionV2 || admission.intent != scope.ReceiveIntentDigest() ||
		admission.layoutVersion != scope.LayoutVersion() || admission.layout != scope.Layout() ||
		admission.directory != directory.DirectoryID || admission.generation != directory.Generation ||
		admission.path != directory.Path || admission.modified != directory.ModifiedTime {
		return false
	}
	return subtle.ConstantTimeCompare(admission.parent[:], directory.ParentAdmission.token[:]) == 1
}

// ValidateDirectoryAdmissionBinding treats an output adapter's receipt as
// untrusted until its complete immutable claim matches the requested intent and
// generation. Authentication remains the issuing session ledger's responsibility.
func ValidateDirectoryAdmissionBinding(
	scope DirectoryAdmissionScope,
	admission DirectoryAdmission,
	directory MaterializationDirectory,
) error {
	if !admissionMatchesDirectory(scope, admission, directory) {
		return ErrDirectoryAdmissionMismatch
	}
	return nil
}

func validDirectoryAdmissionSecret(secret []byte) bool {
	if len(secret) != directoryAdmissionSecretBytes {
		return false
	}
	var combined byte
	for _, value := range secret {
		combined |= value
	}
	return combined != 0
}

func canonicalDirectoryAdmissionModifiedTime(modified catalog.ModifiedTime) []byte {
	if !modified.Present() {
		return []byte{1}
	}
	seconds := make([]byte, 8)
	binary.BigEndian.PutUint64(seconds, uint64(modified.Seconds()))
	nanoseconds := make([]byte, 4)
	binary.BigEndian.PutUint32(nanoseconds, modified.Nanoseconds())
	encoded := []byte{2}
	encoded = appendCanonicalField(encoded, seconds)
	encoded = appendCanonicalField(encoded, nanoseconds)
	encoded = appendCanonicalField(encoded, []byte{byte(modified.Precision())})
	return encoded
}

func appendDirectoryAdmissionFrame(target, value []byte) []byte {
	return appendCanonicalField(target, value)
}

func canonicalDirectoryAdmissionPath(path string) []byte {
	if path == "" {
		return []byte{1}
	}
	segments := strings.Split(path, "/")
	canonicalPath := appendCanonicalUint64Count(nil, uint64(len(segments)))
	for _, segment := range segments {
		canonicalPath = appendCanonicalField(canonicalPath, []byte(segment))
	}
	encoded := []byte{2}
	return append(encoded, appendCanonicalField(nil, canonicalPath)...)
}

// DirectorySettlementKind distinguishes a successful metadata seal from a
// reconciled, directory-local metadata failure. Ambiguous mutation is returned
// as an error because it cannot safely become a cached terminal settlement.
type DirectorySettlementKind uint8

const (
	DirectoryFinalized DirectorySettlementKind = iota + 1
	DirectoryIsolatedFailure
)

// DirectorySettlement is an immutable terminal result for exactly one
// authenticated admission. It retains only a normalized leaf fault, never the
// raw error graph observed by the directory authority.
type DirectorySettlement struct {
	kind      DirectorySettlementKind
	admission DirectoryAdmission
	failure   fault.Fault
}

func NewFinalizedDirectorySettlement(admission DirectoryAdmission) (DirectorySettlement, error) {
	settlement := DirectorySettlement{kind: DirectoryFinalized, admission: admission}
	if !settlement.valid() {
		return DirectorySettlement{}, ErrInvalidOutputSettlement
	}
	return settlement, nil
}

func NewIsolatedDirectorySettlement(
	admission DirectoryAdmission,
	failure fault.Fault,
) (DirectorySettlement, error) {
	settlement := DirectorySettlement{
		kind: DirectoryIsolatedFailure, admission: admission, failure: failure,
	}
	if !settlement.valid() {
		return DirectorySettlement{}, ErrInvalidOutputSettlement
	}
	return settlement, nil
}

func (settlement DirectorySettlement) Kind() DirectorySettlementKind {
	return settlement.kind
}

func (settlement DirectorySettlement) Admission() DirectoryAdmission {
	return settlement.admission
}

func (settlement DirectorySettlement) IsolatedFault() (fault.Fault, bool) {
	return settlement.failure,
		settlement.kind == DirectoryIsolatedFailure && settlement.failure.Valid()
}

func (settlement DirectorySettlement) valid() bool {
	if !settlement.admission.validSnapshot() {
		return false
	}
	switch settlement.kind {
	case DirectoryFinalized:
		return settlement.failure.IsZero()
	case DirectoryIsolatedFailure:
		code, outputFault := settlement.failure.OutputCode()
		return outputFault && settlement.failure.Scope() == fault.ScopeDirectoryLocal &&
			code == fault.OutputDirectoryMetadata
	default:
		return false
	}
}

func validateDirectorySettlement(
	expected DirectoryAdmission,
	settlement DirectorySettlement,
) error {
	// Compare the complete immutable snapshot, not just a directory/path tuple.
	// The receipt token is the authority and every retained field is authenticated.
	if !expected.validSnapshot() || !settlement.valid() ||
		!sameDirectoryAdmissionSnapshot(settlement.Admission(), expected) {
		return outputContractFault(nil)
	}
	return nil
}

func sameDirectoryAdmissionSnapshot(left, right DirectoryAdmission) bool {
	return left.Equal(right) &&
		left.version == right.version && left.intent == right.intent &&
		left.layoutVersion == right.layoutVersion && left.layout == right.layout &&
		left.directory == right.directory && left.generation == right.generation &&
		subtle.ConstantTimeCompare(left.parent[:], right.parent[:]) == 1 &&
		left.path == right.path && left.modified == right.modified
}
