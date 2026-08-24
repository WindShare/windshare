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
	DirectoryAdmissionLayoutV2 uint8 = 2

	directoryAdmissionTokenBytes  = sha256.Size
	directoryAdmissionSecretBytes = sha256.Size
	directoryAdmissionDomain      = "windshare/directory-admission/v2"
)

type DirectoryAdmissionLayout uint8
type DirectoryAdmissionRootKind uint8

const (
	DirectoryAdmissionTreeSingleFile DirectoryAdmissionLayout = iota + 1
	DirectoryAdmissionTreeResultRoot
	DirectoryAdmissionTreeCatalogRoot
	DirectoryAdmissionZipResultRoot
)

const (
	DirectoryAdmissionNoRoot DirectoryAdmissionRootKind = iota + 1
	DirectoryAdmissionDirectoryAnchor
	DirectoryAdmissionSyntheticRoot
	DirectoryAdmissionCatalogRoot
)

type DirectoryAdmissionRootExpectation struct {
	kind      DirectoryAdmissionRootKind
	directory catalog.DirectoryID
	path      string
}

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
	root          DirectoryAdmissionRootExpectation
}

func NewDirectoryAdmissionScope(intent ReceiveIntent) (DirectoryAdmissionScope, error) {
	if !intent.valid() {
		return DirectoryAdmissionScope{}, ErrInvalidDirectoryAdmission
	}
	artifact := intent.ArtifactSpec()
	plan := intent.MaterializationPlan()
	var layout DirectoryAdmissionLayout
	var root DirectoryAdmissionRootExpectation
	switch plan.Kind() {
	case receivecontract.PlanDirectTree:
		tree, ok := artifact.DirectoryTree()
		if !ok {
			return DirectoryAdmissionScope{}, ErrInvalidDirectoryAdmission
		}
		switch tree.Kind() {
		case receivecontract.DirectoryTreeSingleFile:
			layout = DirectoryAdmissionTreeSingleFile
			root = DirectoryAdmissionRootExpectation{kind: DirectoryAdmissionNoRoot}
		case receivecontract.DirectoryTreeResultRoot:
			layout = DirectoryAdmissionTreeResultRoot
			resultRoot, ok := tree.ResultRoot()
			if !ok {
				return DirectoryAdmissionScope{}, ErrInvalidDirectoryAdmission
			}
			root = directoryAdmissionResultRootExpectation(intent, plan, resultRoot)
		case receivecontract.DirectoryTreeCatalogRoot:
			layout = DirectoryAdmissionTreeCatalogRoot
			root = DirectoryAdmissionRootExpectation{
				kind: DirectoryAdmissionCatalogRoot, directory: intent.SyntheticRoot(),
			}
		default:
			return DirectoryAdmissionScope{}, ErrInvalidDirectoryAdmission
		}
	case receivecontract.PlanDirectResumableZIP:
		zip, ok := artifact.ZipArchive()
		if !ok {
			return DirectoryAdmissionScope{}, ErrInvalidDirectoryAdmission
		}
		layout = DirectoryAdmissionZipResultRoot
		root = directoryAdmissionArchiveRootExpectation(intent, zip.Layout)
	default:
		return DirectoryAdmissionScope{}, ErrInvalidDirectoryAdmission
	}
	scope := DirectoryAdmissionScope{
		intent: intent.Digest(), layoutVersion: DirectoryAdmissionLayoutV2,
		layout: layout, root: root,
	}
	if !scope.valid() {
		return DirectoryAdmissionScope{}, ErrInvalidDirectoryAdmission
	}
	return scope, nil
}

func directoryAdmissionResultRootExpectation(
	intent ReceiveIntent,
	plan receivecontract.MaterializationPlan,
	root receivecontract.ResultRootLayout,
) DirectoryAdmissionRootExpectation {
	expectation := directoryAdmissionArchiveRootExpectation(intent, root)
	if reservation, ok := plan.DestinationReservation(); ok &&
		reservation.AuthorityKind() != receivecontract.AuthorityFSAContainer {
		expectation.path = root.Name()
	}
	return expectation
}

func directoryAdmissionArchiveRootExpectation(
	intent ReceiveIntent,
	root receivecontract.ResultRootLayout,
) DirectoryAdmissionRootExpectation {
	if root.AnchorKind() == receivecontract.ResultRootDirectoryAnchor {
		return DirectoryAdmissionRootExpectation{
			kind: DirectoryAdmissionDirectoryAnchor, directory: root.DirectoryID(),
		}
	}
	return DirectoryAdmissionRootExpectation{
		kind: DirectoryAdmissionSyntheticRoot, directory: intent.SyntheticRoot(),
	}
}

func (scope DirectoryAdmissionScope) ReceiveIntentDigest() ReceiveIntentDigest { return scope.intent }
func (scope DirectoryAdmissionScope) LayoutVersion() uint8                     { return scope.layoutVersion }
func (scope DirectoryAdmissionScope) Layout() DirectoryAdmissionLayout         { return scope.layout }
func (scope DirectoryAdmissionScope) RootExpectation() DirectoryAdmissionRootExpectation {
	return scope.root
}

func (root DirectoryAdmissionRootExpectation) Kind() DirectoryAdmissionRootKind { return root.kind }
func (root DirectoryAdmissionRootExpectation) DirectoryID() catalog.DirectoryID {
	return root.directory
}
func (root DirectoryAdmissionRootExpectation) Path() string { return root.path }

func (scope DirectoryAdmissionScope) valid() bool {
	return !scope.intent.IsZero() && scope.layoutVersion == DirectoryAdmissionLayoutV2 &&
		scope.layout >= DirectoryAdmissionTreeSingleFile && scope.layout <= DirectoryAdmissionZipResultRoot &&
		scope.root.valid()
}

func (root DirectoryAdmissionRootExpectation) valid() bool {
	if root.kind == DirectoryAdmissionNoRoot {
		return root.directory.IsZero() && root.path == ""
	}
	if root.kind < DirectoryAdmissionDirectoryAnchor || root.kind > DirectoryAdmissionCatalogRoot ||
		root.directory.IsZero() {
		return false
	}
	if root.path == "" {
		return true
	}
	canonical, err := catalog.CanonicalPath(root.path)
	return err == nil && canonical == root.path
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
	directory, err := normalizeMaterializationDirectory(directory)
	if err != nil {
		return DirectoryAdmission{}, err
	}
	if !validDirectoryAdmissionSecret(secret) {
		return DirectoryAdmission{}, ErrInvalidDirectoryAdmission
	}
	message, err := canonicalAuthenticatedDirectoryAdmissionMessageV2(scope, directory)
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
		directory:     directory.DirectoryID(),
		generation:    directory.Generation(),
		parent:        directory.ParentAdmission().token,
		path:          directory.Path().String(),
		modified:      directory.ModifiedTime(),
	}, nil
}

// CanonicalDirectoryAdmissionMessageV2 returns the exact HMAC message shared by
// Go and Web. The secret is intentionally absent: it is the HMAC key, not one of
// the framed caller-controlled claim fields.
func CanonicalDirectoryAdmissionMessageV2(
	scope DirectoryAdmissionScope,
	directory MaterializationDirectory,
) ([]byte, error) {
	return canonicalAuthenticatedDirectoryAdmissionMessageV2(scope, directory)
}

func canonicalAuthenticatedDirectoryAdmissionMessageV2(
	scope DirectoryAdmissionScope,
	directory MaterializationDirectory,
) ([]byte, error) {
	directory, normalizationErr := normalizeMaterializationDirectory(directory)
	if normalizationErr != nil {
		return nil, normalizationErr
	}
	if err := validateMaterializationDirectoryForScope(scope, directory); err != nil {
		return nil, err
	}
	modified := canonicalDirectoryAdmissionModifiedTime(directory.ModifiedTime())
	materializationPath := directory.Path().String()
	path := canonicalDirectoryAdmissionPath(materializationPath)
	message := make([]byte, 0, 10*8+2+len(directoryAdmissionDomain)+
		ReceiveIntentDigestBytes+2*catalog.IdentityBytes+directoryAdmissionTokenBytes+
		len(materializationPath)+len(modified))
	message = append(message, directoryAdmissionDomain...)
	message = append(message, 0, DirectoryAdmissionV2)
	message = appendCanonicalField(message, scope.ReceiveIntentDigest().Bytes())
	message = appendCanonicalField(message, []byte{scope.LayoutVersion()})
	message = appendCanonicalField(message, []byte{byte(scope.Layout())})
	message = appendDirectoryAdmissionRootExpectation(message, scope.RootExpectation())
	message = appendDirectoryAdmissionFrame(message, directory.DirectoryID().Bytes())
	message = appendDirectoryAdmissionFrame(message, directory.Generation().Bytes())
	parentAdmission := directory.ParentAdmission()
	if parentAdmission.IsZero() {
		message = appendDirectoryAdmissionFrame(message, nil)
	} else {
		message = appendDirectoryAdmissionFrame(message, parentAdmission.token[:])
	}
	message = appendDirectoryAdmissionFrame(message, path)
	message = appendDirectoryAdmissionFrame(message, modified)
	return message, nil
}

func validateMaterializationDirectoryForScope(scope DirectoryAdmissionScope, directory MaterializationDirectory) error {
	directory, err := normalizeMaterializationDirectory(directory)
	if err != nil {
		return err
	}
	if !scope.valid() || directory.DirectoryID().IsZero() || directory.Generation().IsZero() {
		return ErrInvalidDirectoryAdmission
	}
	if !directory.Path().Valid() {
		return ErrInvalidDirectoryAdmission
	}
	materializationPath := directory.Path().String()
	if directory.ParentAdmission().IsZero() {
		root := scope.RootExpectation()
		if root.Kind() == DirectoryAdmissionNoRoot ||
			directory.DirectoryID() != root.DirectoryID() ||
			materializationPath != root.Path() {
			return ErrInvalidDirectoryAdmission
		}
		return nil
	}
	if scope.RootExpectation().Kind() == DirectoryAdmissionNoRoot {
		return ErrInvalidDirectoryAdmission
	}
	if materializationPath != "" {
		canonical, err := catalog.CanonicalPath(materializationPath)
		if err != nil || canonical != materializationPath {
			return ErrInvalidDirectoryAdmission
		}
	}
	if !directory.ParentAdmission().validSnapshot() ||
		directory.ParentAdmission().intent != scope.ReceiveIntentDigest() ||
		directory.ParentAdmission().layoutVersion != scope.LayoutVersion() ||
		directory.ParentAdmission().layout != scope.Layout() ||
		!immediateDirectoryChild(directory.ParentAdmission().path, materializationPath) {
		return ErrInvalidDirectoryAdmission
	}
	return nil
}

func normalizeMaterializationDirectory(
	directory MaterializationDirectory,
) (MaterializationDirectory, error) {
	if !directory.Valid() {
		return MaterializationDirectory{}, ErrInvalidDirectoryAdmission
	}
	return directory, nil
}

func immediateDirectoryChild(parent, child string) bool {
	if child == "" {
		return false
	}
	separator := strings.LastIndexByte(child, '/')
	if separator < 0 {
		return parent == ""
	}
	return parent == child[:separator]
}

func (admission DirectoryAdmission) validSnapshot() bool {
	if admission.version != DirectoryAdmissionV2 || admission.intent.IsZero() || admission.IsZero() ||
		admission.layoutVersion != DirectoryAdmissionLayoutV2 ||
		admission.layout < DirectoryAdmissionTreeSingleFile || admission.layout > DirectoryAdmissionZipResultRoot ||
		admission.directory.IsZero() || admission.generation.IsZero() {
		return false
	}
	if admission.path == "" {
		return true
	}
	canonical, err := catalog.CanonicalPath(admission.path)
	return err == nil && canonical == admission.path
}

func admissionMatchesDirectory(
	scope DirectoryAdmissionScope,
	admission DirectoryAdmission,
	directory MaterializationDirectory,
) bool {
	var err error
	directory, err = normalizeMaterializationDirectory(directory)
	if err != nil {
		return false
	}
	if !admission.validSnapshot() || validateMaterializationDirectoryForScope(scope, directory) != nil ||
		admission.version != DirectoryAdmissionV2 || admission.intent != scope.ReceiveIntentDigest() ||
		admission.layoutVersion != scope.LayoutVersion() || admission.layout != scope.Layout() ||
		admission.directory != directory.DirectoryID() || admission.generation != directory.Generation() ||
		admission.path != directory.Path().String() || admission.modified != directory.ModifiedTime() {
		return false
	}
	parentAdmission := directory.ParentAdmission()
	return subtle.ConstantTimeCompare(admission.parent[:], parentAdmission.token[:]) == 1
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

func appendDirectoryAdmissionRootExpectation(
	target []byte,
	root DirectoryAdmissionRootExpectation,
) []byte {
	target = appendDirectoryAdmissionFrame(target, []byte{byte(root.Kind())})
	if root.Kind() == DirectoryAdmissionNoRoot {
		target = appendDirectoryAdmissionFrame(target, nil)
		return appendDirectoryAdmissionFrame(target, nil)
	}
	target = appendDirectoryAdmissionFrame(target, root.DirectoryID().Bytes())
	return appendDirectoryAdmissionFrame(target, canonicalDirectoryAdmissionPath(root.Path()))
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
	encoded := make([]byte, 0, 1+8+len(canonicalPath))
	encoded = append(encoded, 2)
	return appendCanonicalField(encoded, canonicalPath)
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
