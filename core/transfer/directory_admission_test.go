package transfer

import (
	"bytes"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/binary"
	"errors"
	"slices"
	"strings"
	"testing"

	"github.com/windshare/windshare/core/catalog"
	"github.com/windshare/windshare/core/transfer/ordinaryoutput"
	"github.com/windshare/windshare/core/transfer/receivecontract"
)

func TestDirectoryAdmissionCrossRuntimeCanonicalGolden(t *testing.T) {
	share := crossRuntimeID[catalog.ShareInstance](1)
	syntheticRoot := crossRuntimeID[catalog.DirectoryID](2)
	rules, err := NewSelectionRules(true, nil)
	if err != nil {
		t.Fatal(err)
	}
	selection, err := NewSelectionSpec(share, syntheticRoot, rules)
	if err != nil {
		t.Fatal(err)
	}
	resultRoot, err := receivecontract.NewDirectorySelectionResultRoot(
		crossRuntimeID[catalog.DirectoryID](10), "docs",
	)
	if err != nil {
		t.Fatal(err)
	}
	artifact, err := receivecontract.NewResultRootDirectoryTree(resultRoot)
	if err != nil {
		t.Fatal(err)
	}
	operationBytes := crossRuntimeID[receivecontract.OperationID](11)
	operation, err := receivecontract.OperationIDFromBytes(operationBytes[:])
	if err != nil {
		t.Fatal(err)
	}
	reservationBytes := crossRuntimeID[receivecontract.DestinationReservationID](12)
	reservationID, err := receivecontract.DestinationReservationIDFromBytes(reservationBytes[:])
	if err != nil {
		t.Fatal(err)
	}
	authority, err := receivecontract.AuthorityRefFromBytes(crossRuntimeOpaque(13))
	if err != nil {
		t.Fatal(err)
	}
	reservation, err := receivecontract.NewFSANamedEntryReservation(
		operation, reservationID, artifact, authority, "docs-selection", "docs-selection", 0,
	)
	if err != nil {
		t.Fatal(err)
	}
	plan, err := receivecontract.NewDirectTreePlan(artifact, reservation)
	if err != nil {
		t.Fatal(err)
	}
	intent, err := NewReceiveIntent(selection, artifact, plan)
	if err != nil {
		t.Fatal(err)
	}
	scope, err := NewDirectoryAdmissionScope(intent)
	if err != nil {
		t.Fatal(err)
	}
	rootExpectation := scope.RootExpectation()
	if scope.LayoutVersion() != DirectoryAdmissionLayoutV2 ||
		rootExpectation.Kind() != DirectoryAdmissionDirectoryAnchor ||
		rootExpectation.DirectoryID() != resultRoot.DirectoryID() || rootExpectation.Path() != "" {
		t.Fatalf("FSA root expectation = %+v", rootExpectation)
	}
	nativeReservation, err := receivecontract.NewNativeNamedEntryReservation(
		operation, reservationID, artifact, authority, resultRoot.Name(), 0,
	)
	if err != nil {
		t.Fatal(err)
	}
	nativePlan, err := receivecontract.NewDirectTreePlan(artifact, nativeReservation)
	if err != nil {
		t.Fatal(err)
	}
	nativeIntent, err := NewReceiveIntent(selection, artifact, nativePlan)
	if err != nil {
		t.Fatal(err)
	}
	nativeScope, err := NewDirectoryAdmissionScope(nativeIntent)
	if err != nil {
		t.Fatal(err)
	}
	if nativeRoot := nativeScope.RootExpectation(); nativeRoot.DirectoryID() != resultRoot.DirectoryID() || nativeRoot.Path() != resultRoot.Name() {
		t.Fatalf("native root expectation = %+v", nativeRoot)
	}
	secret := make([]byte, directoryAdmissionSecretBytes)
	for index := range secret {
		secret[index] = byte(index + 1)
	}
	rootDirectory := admissionTestDirectory(
		t, resultRoot.DirectoryID(), crossRuntimeID[catalog.DirectoryGeneration](40), DirectoryAdmission{}, "docs", catalog.ModifiedTime{},
	)
	logicalRootDirectory := admissionTestMaterializationDirectory(t, rootDirectory, resultRoot.Name())
	if _, err := NewDirectoryAdmissionWithSecret(secret, scope, logicalRootDirectory); !errors.Is(err, ErrInvalidDirectoryAdmission) {
		t.Fatalf("FSA logical root name accepted as a relative root: %v", err)
	}
	rootAdmission, err := NewDirectoryAdmissionWithSecret(
		secret, scope, admissionTestMaterializationDirectory(t, rootDirectory, ""),
	)
	if err != nil {
		t.Fatal(err)
	}
	modified, err := catalog.NewModifiedTime(-1234, 567_000_000, catalog.TimePrecisionMilliseconds)
	if err != nil {
		t.Fatal(err)
	}
	childDirectory := admissionTestDirectory(
		t, crossRuntimeID[catalog.DirectoryID](41), crossRuntimeID[catalog.DirectoryGeneration](42),
		rootAdmission, "docs/child", modified,
	)
	childMaterialization := admissionTestMaterializationDirectory(t, childDirectory, "child")
	message, err := CanonicalDirectoryAdmissionMessageV2(scope, childMaterialization)
	if err != nil {
		t.Fatal(err)
	}
	childAdmission, err := NewDirectoryAdmissionWithSecret(secret, scope, childMaterialization)
	if err != nil {
		t.Fatal(err)
	}
	messageDigest := sha256.Sum256(message)
	got := []string{
		base64.RawURLEncoding.EncodeToString(intent.Digest().Bytes()),
		base64.RawURLEncoding.EncodeToString(messageDigest[:]),
		base64.RawURLEncoding.EncodeToString(rootAdmission.Bytes()),
		base64.RawURLEncoding.EncodeToString(childAdmission.Bytes()),
	}
	want := []string{
		"vhhExXaw0i8sWcgd8Payakwul9IpNWKlE1WyPkMc_M4",
		"xvYKa9-QLfT5reKn9crdsf0Sth9l-zYdZjWP9nLmuYA",
		"qVfgMF4KQoXMTFpQu9G0syTxS8w5t9X6K5gnCJCYU6g",
		"LXmZtH7L3nXY66NOXhNSliwUHyeAXdt1sClzpXxwvr4",
	}
	if !slices.Equal(got, want) {
		t.Fatalf("cross-runtime values:\n%s\nwant:\n%s", strings.Join(got, "\n"), strings.Join(want, "\n"))
	}
}

func TestDirectoryAdmissionV2FramesAndAuthenticatesTheCompleteClaim(t *testing.T) {
	root := admissionTestDirectoryID(t, 0x20)
	intent := admissionTestIntent(t, root, 0x80)
	scope := admissionTestScope(t, intent)
	secret := admissionTestSequence(0xc0, directoryAdmissionSecretBytes)
	rootDirectory := admissionTestDirectory(
		t, root, admissionTestGeneration(t, 0x50), DirectoryAdmission{}, "", catalog.ModifiedTime{},
	)
	rootAdmission, err := NewDirectoryAdmissionWithSecret(
		secret, scope, admissionTestMaterializationDirectory(t, rootDirectory),
	)
	if err != nil {
		t.Fatal(err)
	}
	modified, err := catalog.NewModifiedTime(-2, 123_000_000, catalog.TimePrecisionMilliseconds)
	if err != nil {
		t.Fatal(err)
	}
	childDirectory := admissionTestDirectory(
		t, admissionTestDirectoryID(t, 0x30), admissionTestGeneration(t, 0x60),
		rootAdmission, "photos", modified,
	)
	childMaterialization := admissionTestMaterializationDirectory(t, childDirectory)
	message, err := CanonicalDirectoryAdmissionMessageV2(scope, childMaterialization)
	if err != nil {
		t.Fatal(err)
	}

	fields, version := parseDirectoryAdmissionMessage(t, message)
	if version != DirectoryAdmissionV2 || len(fields) != 11 {
		t.Fatalf("version=%d fields=%d", version, len(fields))
	}
	wantFields := [][]byte{
		intent.Digest().Bytes(),
		{DirectoryAdmissionLayoutV2},
		{byte(DirectoryAdmissionTreeCatalogRoot)},
		{byte(DirectoryAdmissionCatalogRoot)},
		root.Bytes(),
		canonicalDirectoryAdmissionPath(""),
		childDirectory.DirectoryID.Bytes(),
		childDirectory.Generation.Bytes(),
		rootAdmission.Bytes(),
		canonicalDirectoryAdmissionPath(childMaterialization.Path().String()),
		canonicalDirectoryAdmissionModifiedTime(modified),
	}
	for index, want := range wantFields {
		if !bytes.Equal(fields[index], want) {
			t.Fatalf("field[%d]=%x want=%x", index, fields[index], want)
		}
	}
	admission, err := NewDirectoryAdmissionWithSecret(secret, scope, childMaterialization)
	if err != nil {
		t.Fatal(err)
	}
	mac := hmac.New(sha256.New, secret)
	_, _ = mac.Write(message)
	if !bytes.Equal(admission.Bytes(), mac.Sum(nil)) {
		t.Fatalf("token=%x want=%x", admission.Bytes(), mac.Sum(nil))
	}
	if admission.SchemaVersion() != DirectoryAdmissionV2 ||
		admission.ReceiveIntentDigest() != intent.Digest() ||
		admission.LayoutVersion() != DirectoryAdmissionLayoutV2 ||
		admission.Layout() != DirectoryAdmissionTreeCatalogRoot ||
		admission.DirectoryID() != childDirectory.DirectoryID || admission.Generation() != childDirectory.Generation ||
		admission.Path() != childDirectory.SourcePath.String() || admission.ModifiedTime() != modified ||
		!bytes.Equal(admission.ParentToken(), rootAdmission.Bytes()) {
		t.Fatalf("admission snapshot=%+v", admission)
	}
}

func TestDirectoryAdmissionV2UsesClosedRootAndAbsentTimeUnions(t *testing.T) {
	root := admissionTestDirectoryID(t, 0x21)
	intent := admissionTestIntent(t, root, 0x81)
	scope := admissionTestScope(t, intent)
	directory := admissionTestDirectory(
		t, root, admissionTestGeneration(t, 0x51), DirectoryAdmission{}, "", catalog.ModifiedTime{},
	)
	message, err := CanonicalDirectoryAdmissionMessageV2(
		scope, admissionTestMaterializationDirectory(t, directory),
	)
	if err != nil {
		t.Fatal(err)
	}
	fields, _ := parseDirectoryAdmissionMessage(t, message)
	if !bytes.Equal(fields[8], []byte{}) || !bytes.Equal(fields[9], []byte{1}) ||
		!bytes.Equal(fields[10], []byte{1}) {
		t.Fatalf("parent=%x path=%x modified=%x", fields[8], fields[9], fields[10])
	}
}

func TestDirectoryAdmissionV2BindsIntentLayoutAndSyntheticRoot(t *testing.T) {
	if _, err := NewDirectoryAdmissionScope(ReceiveIntent{}); !errors.Is(err, ErrInvalidDirectoryAdmission) {
		t.Fatalf("zero intent scope error=%v", err)
	}
	root := admissionTestDirectoryID(t, 0x22)
	firstIntent := admissionTestIntent(t, root, 0x82)
	secondIntent := admissionTestIntent(t, root, 0x92)
	firstScope := admissionTestScope(t, firstIntent)
	secondScope := admissionTestScope(t, secondIntent)
	secret := admissionTestSequence(0x40, directoryAdmissionSecretBytes)
	directory := admissionTestDirectory(
		t, root, admissionTestGeneration(t, 0x52), DirectoryAdmission{}, "", catalog.ModifiedTime{},
	)

	materialization := admissionTestMaterializationDirectory(t, directory)
	first, err := NewDirectoryAdmissionWithSecret(secret, firstScope, materialization)
	if err != nil {
		t.Fatal(err)
	}
	second, err := NewDirectoryAdmissionWithSecret(secret, secondScope, materialization)
	if err != nil {
		t.Fatal(err)
	}
	if first.Equal(second) || bytes.Equal(first.Bytes(), second.Bytes()) {
		t.Fatal("two durable intent namespaces shared a receipt")
	}
	if err := ValidateDirectoryAdmissionBinding(firstScope, first, materialization); err != nil {
		t.Fatal(err)
	}
	if !errors.Is(ValidateDirectoryAdmissionBinding(secondScope, first, materialization), ErrDirectoryAdmissionMismatch) {
		t.Fatal("foreign intent accepted the receipt")
	}

	wrongRoot := directory
	wrongRoot.DirectoryID = admissionTestDirectoryID(t, 0x23)
	if _, err := NewDirectoryAdmissionWithSecret(
		secret, firstScope, admissionTestMaterializationDirectory(t, wrongRoot),
	); !errors.Is(err, ErrInvalidDirectoryAdmission) {
		t.Fatalf("wrong synthetic root error=%v", err)
	}
	rootWithParent := directory
	rootWithParent.ParentAdmission = first
	if _, err := NewDirectoryAdmissionWithSecret(
		secret, firstScope, admissionTestMaterializationDirectory(t, rootWithParent),
	); !errors.Is(err, ErrInvalidDirectoryAdmission) {
		t.Fatalf("parented synthetic root error=%v", err)
	}
}

func TestDirectoryAdmissionV2RejectsForeignOrNonImmediateParentClaims(t *testing.T) {
	root := admissionTestDirectoryID(t, 0x24)
	intent := admissionTestIntent(t, root, 0x84)
	foreignIntent := admissionTestIntent(t, root, 0x94)
	scope := admissionTestScope(t, intent)
	foreignScope := admissionTestScope(t, foreignIntent)
	secret := admissionTestSequence(0x60, directoryAdmissionSecretBytes)
	rootDirectory := admissionTestDirectory(
		t, root, admissionTestGeneration(t, 0x54), DirectoryAdmission{}, "", catalog.ModifiedTime{},
	)
	parent, err := NewDirectoryAdmissionWithSecret(
		secret, scope, admissionTestMaterializationDirectory(t, rootDirectory),
	)
	if err != nil {
		t.Fatal(err)
	}
	foreignParent, err := NewDirectoryAdmissionWithSecret(
		secret, foreignScope, admissionTestMaterializationDirectory(t, rootDirectory),
	)
	if err != nil {
		t.Fatal(err)
	}
	for name, directory := range map[string]AuthenticatedSourceDirectory{
		"missing parent": admissionTestDirectory(
			t, admissionTestDirectoryID(t, 0x34), admissionTestGeneration(t, 0x64),
			DirectoryAdmission{}, "child", catalog.ModifiedTime{},
		),
		"foreign parent": admissionTestDirectory(
			t, admissionTestDirectoryID(t, 0x35), admissionTestGeneration(t, 0x65),
			foreignParent, "child", catalog.ModifiedTime{},
		),
		"non-immediate parent": admissionTestDirectory(
			t, admissionTestDirectoryID(t, 0x36), admissionTestGeneration(t, 0x66),
			parent, "child/grandchild", catalog.ModifiedTime{},
		),
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := NewDirectoryAdmissionWithSecret(
				secret, scope, admissionTestMaterializationDirectory(t, directory),
			); !errors.Is(err, ErrInvalidDirectoryAdmission) {
				t.Fatalf("error=%v", err)
			}
		})
	}
}

func TestDirectoryAdmissionV2SnapshotsKeyAndReceiptBytes(t *testing.T) {
	root := admissionTestDirectoryID(t, 0x25)
	intent := admissionTestIntent(t, root, 0x85)
	scope := admissionTestScope(t, intent)
	secret := admissionTestSequence(0x70, directoryAdmissionSecretBytes)
	directory := admissionTestDirectory(
		t, root, admissionTestGeneration(t, 0x55), DirectoryAdmission{}, "", catalog.ModifiedTime{},
	)
	materialization := admissionTestMaterializationDirectory(t, directory)
	admission, err := NewDirectoryAdmissionWithSecret(secret, scope, materialization)
	if err != nil {
		t.Fatal(err)
	}
	want := admission.Bytes()
	secret[0] ^= 0xff
	returned := admission.Bytes()
	returned[0] ^= 0xff
	if !bytes.Equal(admission.Bytes(), want) {
		t.Fatal("caller mutation changed the immutable receipt")
	}
	retry, err := NewDirectoryAdmissionWithSecret(
		admissionTestSequence(0x70, directoryAdmissionSecretBytes), scope, materialization,
	)
	if err != nil || !admission.Equal(retry) {
		t.Fatalf("exact retry admission=%+v error=%v", retry, err)
	}
	if (DirectoryAdmission{}).Equal(DirectoryAdmission{}) {
		t.Fatal("zero receipts compared as usable authority")
	}
	if _, err := NewDirectoryAdmissionWithSecret(
		make([]byte, directoryAdmissionSecretBytes), scope, materialization,
	); !errors.Is(err, ErrInvalidDirectoryAdmission) {
		t.Fatalf("zero key error=%v", err)
	}
}

func parseDirectoryAdmissionMessage(t *testing.T, message []byte) ([][]byte, uint8) {
	t.Helper()
	prefix := append([]byte(directoryAdmissionDomain), 0)
	if !bytes.HasPrefix(message, prefix) || len(message) < len(prefix)+1 {
		t.Fatal("directory-admission domain is missing")
	}
	offset := len(prefix)
	version := message[offset]
	offset++
	readFrame := func() []byte {
		t.Helper()
		if len(message)-offset < 8 {
			t.Fatalf("truncated frame length at %d", offset)
		}
		length := binary.BigEndian.Uint64(message[offset : offset+8])
		offset += 8
		if length > uint64(len(message)-offset) {
			t.Fatalf("truncated frame payload at %d length=%d", offset, length)
		}
		value := append([]byte(nil), message[offset:offset+int(length)]...)
		offset += int(length)
		return value
	}
	fields := make([][]byte, 0, 11)
	for range 11 {
		fields = append(fields, readFrame())
	}
	if offset != len(message) {
		t.Fatalf("trailing bytes=%d", len(message)-offset)
	}
	return fields, version
}

func admissionTestIntent(t *testing.T, root catalog.DirectoryID, targetFirst byte) ReceiveIntent {
	t.Helper()
	rules, err := NewSelectionRules(true, nil)
	if err != nil {
		t.Fatal(err)
	}
	return testReceiveIntent(t, admissionTestShare(t, targetFirst), root, rules)
}

func admissionTestScope(t *testing.T, intent ReceiveIntent) DirectoryAdmissionScope {
	t.Helper()
	scope, err := NewDirectoryAdmissionScope(intent)
	if err != nil {
		t.Fatal(err)
	}
	return scope
}

func admissionTestDirectory(
	t *testing.T,
	directory catalog.DirectoryID,
	generation catalog.DirectoryGeneration,
	parent DirectoryAdmission,
	path string,
	modified catalog.ModifiedTime,
) AuthenticatedSourceDirectory {
	t.Helper()
	sourcePath := ordinaryoutput.EmptySourceCatalogPath()
	if path != "" {
		var err error
		sourcePath, err = ordinaryoutput.NewSourceCatalogPath(path)
		if err != nil {
			t.Fatal(err)
		}
	}
	return AuthenticatedSourceDirectory{
		DirectoryID: directory, Generation: generation, ParentAdmission: parent,
		SourcePath: sourcePath, ModifiedTime: modified,
	}
}

func admissionTestMaterializationDirectory(
	t *testing.T,
	source AuthenticatedSourceDirectory,
	paths ...string,
) MaterializationDirectory {
	t.Helper()
	path := source.SourcePath.String()
	if len(paths) > 1 {
		t.Fatal("at most one materialization path is allowed")
	}
	if len(paths) == 1 {
		path = paths[0]
	}
	relative, err := NewMaterializationRootRelativePath(path)
	if err != nil {
		t.Fatal(err)
	}
	directory, err := NewMaterializationDirectory(
		source.DirectoryID, source.Generation, relative, source.ParentAdmission, source.ModifiedTime,
	)
	if err != nil {
		t.Fatal(err)
	}
	return directory
}

func admissionTestShare(t *testing.T, first byte) catalog.ShareInstance {
	t.Helper()
	value, err := catalog.ShareInstanceFromBytes(admissionTestSequence(first, catalog.IdentityBytes))
	if err != nil {
		t.Fatal(err)
	}
	return value
}

func admissionTestDirectoryID(t *testing.T, first byte) catalog.DirectoryID {
	t.Helper()
	value, err := catalog.DirectoryIDFromBytes(admissionTestSequence(first, catalog.IdentityBytes))
	if err != nil {
		t.Fatal(err)
	}
	return value
}

func admissionTestGeneration(t *testing.T, first byte) catalog.DirectoryGeneration {
	t.Helper()
	value, err := catalog.DirectoryGenerationFromBytes(admissionTestSequence(first, catalog.IdentityBytes))
	if err != nil {
		t.Fatal(err)
	}
	return value
}

func admissionTestSequence(first byte, count int) []byte {
	value := make([]byte, count)
	for index := range value {
		value[index] = first + byte(index)
	}
	return value
}
