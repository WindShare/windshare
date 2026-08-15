package transfer

import (
	"bytes"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/binary"
	"errors"
	"slices"
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
		operation, reservationID, artifact, authority, "docs-selection", 0,
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
	secret := make([]byte, directoryAdmissionSecretBytes)
	for index := range secret {
		secret[index] = byte(index + 1)
	}
	rootDirectory := admissionTestDirectory(
		t, syntheticRoot, crossRuntimeID[catalog.DirectoryGeneration](40), DirectoryAdmission{}, "", catalog.ModifiedTime{},
	)
	rootAdmission, err := NewDirectoryAdmissionWithSecret(secret, scope, rootDirectory)
	if err != nil {
		t.Fatal(err)
	}
	modified, err := catalog.NewModifiedTime(-1234, 567_000_000, catalog.TimePrecisionMilliseconds)
	if err != nil {
		t.Fatal(err)
	}
	childDirectory := admissionTestDirectory(
		t, crossRuntimeID[catalog.DirectoryID](41), crossRuntimeID[catalog.DirectoryGeneration](42),
		rootAdmission, "child", modified,
	)
	message, err := CanonicalDirectoryAdmissionMessageV2(scope, childDirectory)
	if err != nil {
		t.Fatal(err)
	}
	childAdmission, err := NewDirectoryAdmissionWithSecret(secret, scope, childDirectory)
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
		"xs4aZXCn-OP6jUHf8nxuaLxtxxDJy52zdNfBlAWFKfE",
		"-UZ_B6qoEBFrOV7XkR2Osusr3mFEe5YVI0xIHsdTK5o",
		"ga_pazR-tSpAA1wIpn-VLH9bQQ82_PiBQSIGrp1q65Y",
		"yu8hvZ3nTB-n4vPvJ0XdehcivSIUPWfSdcs2P2coS7g",
	}
	if !slices.Equal(got, want) {
		t.Fatalf("cross-runtime values=%v want=%v", got, want)
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
	rootAdmission, err := NewDirectoryAdmissionWithSecret(secret, scope, rootDirectory)
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
	message, err := CanonicalDirectoryAdmissionMessageV2(scope, childDirectory)
	if err != nil {
		t.Fatal(err)
	}

	fields, version := parseDirectoryAdmissionMessage(t, message)
	if version != DirectoryAdmissionV2 || len(fields) != 8 {
		t.Fatalf("version=%d fields=%d", version, len(fields))
	}
	wantFields := [][]byte{
		intent.Digest().Bytes(),
		{DirectoryAdmissionLayoutV1},
		{byte(DirectoryAdmissionTreeCatalogRoot)},
		childDirectory.DirectoryID.Bytes(),
		childDirectory.Generation.Bytes(),
		rootAdmission.Bytes(),
		canonicalDirectoryAdmissionPath(childDirectory.SourcePath.String()),
		canonicalDirectoryAdmissionModifiedTime(modified),
	}
	for index, want := range wantFields {
		if !bytes.Equal(fields[index], want) {
			t.Fatalf("field[%d]=%x want=%x", index, fields[index], want)
		}
	}
	admission, err := NewDirectoryAdmissionWithSecret(secret, scope, childDirectory)
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
		admission.LayoutVersion() != DirectoryAdmissionLayoutV1 ||
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
	message, err := CanonicalDirectoryAdmissionMessageV2(scope, directory)
	if err != nil {
		t.Fatal(err)
	}
	fields, _ := parseDirectoryAdmissionMessage(t, message)
	if !bytes.Equal(fields[5], []byte{}) || !bytes.Equal(fields[6], []byte{1}) ||
		!bytes.Equal(fields[7], []byte{1}) {
		t.Fatalf("parent=%x path=%x modified=%x", fields[5], fields[6], fields[7])
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

	first, err := NewDirectoryAdmissionWithSecret(secret, firstScope, directory)
	if err != nil {
		t.Fatal(err)
	}
	second, err := NewDirectoryAdmissionWithSecret(secret, secondScope, directory)
	if err != nil {
		t.Fatal(err)
	}
	if first.Equal(second) || bytes.Equal(first.Bytes(), second.Bytes()) {
		t.Fatal("two durable intent namespaces shared a receipt")
	}
	if err := ValidateDirectoryAdmissionBinding(firstScope, first, directory); err != nil {
		t.Fatal(err)
	}
	if !errors.Is(ValidateDirectoryAdmissionBinding(secondScope, first, directory), ErrDirectoryAdmissionMismatch) {
		t.Fatal("foreign intent accepted the receipt")
	}

	wrongRoot := directory
	wrongRoot.DirectoryID = admissionTestDirectoryID(t, 0x23)
	if _, err := NewDirectoryAdmissionWithSecret(secret, firstScope, wrongRoot); !errors.Is(err, ErrInvalidDirectoryAdmission) {
		t.Fatalf("wrong synthetic root error=%v", err)
	}
	rootWithParent := directory
	rootWithParent.ParentAdmission = first
	if _, err := NewDirectoryAdmissionWithSecret(secret, firstScope, rootWithParent); !errors.Is(err, ErrInvalidDirectoryAdmission) {
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
	parent, err := NewDirectoryAdmissionWithSecret(secret, scope, rootDirectory)
	if err != nil {
		t.Fatal(err)
	}
	foreignParent, err := NewDirectoryAdmissionWithSecret(secret, foreignScope, rootDirectory)
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
			if _, err := NewDirectoryAdmissionWithSecret(secret, scope, directory); !errors.Is(err, ErrInvalidDirectoryAdmission) {
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
	admission, err := NewDirectoryAdmissionWithSecret(secret, scope, directory)
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
	retry, err := NewDirectoryAdmissionWithSecret(admissionTestSequence(0x70, directoryAdmissionSecretBytes), scope, directory)
	if err != nil || !admission.Equal(retry) {
		t.Fatalf("exact retry admission=%+v error=%v", retry, err)
	}
	if (DirectoryAdmission{}).Equal(DirectoryAdmission{}) {
		t.Fatal("zero receipts compared as usable authority")
	}
	if _, err := NewDirectoryAdmissionWithSecret(make([]byte, directoryAdmissionSecretBytes), scope, directory); !errors.Is(err, ErrInvalidDirectoryAdmission) {
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
	fields := make([][]byte, 0, 8)
	for range 8 {
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
