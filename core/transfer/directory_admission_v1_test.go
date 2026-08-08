package transfer

import (
	"bytes"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/binary"
	"errors"
	"testing"

	"github.com/windshare/windshare/core/catalog"
)

func TestDirectoryAdmissionV1FramesAndAuthenticatesTheCompleteClaim(t *testing.T) {
	root := admissionTestDirectoryID(t, 0x20)
	intent := admissionTestIntent(t, root, 0x80)
	scope := admissionTestScope(t, intent)
	secret := admissionTestSequence(0xc0, directoryAdmissionSecretBytes)
	rootDirectory := OutputDirectory{
		DirectoryID: root,
		Generation:  admissionTestGeneration(t, 0x50),
	}
	rootAdmission, err := NewDirectoryAdmissionWithSecret(secret, scope, rootDirectory)
	if err != nil {
		t.Fatal(err)
	}
	modified, err := catalog.NewModifiedTime(-2, 123_000_000, catalog.TimePrecisionMilliseconds)
	if err != nil {
		t.Fatal(err)
	}
	childDirectory := OutputDirectory{
		DirectoryID:     admissionTestDirectoryID(t, 0x30),
		Generation:      admissionTestGeneration(t, 0x60),
		Path:            "photos",
		ParentAdmission: rootAdmission,
		ModifiedTime:    modified,
	}
	message, err := CanonicalDirectoryAdmissionMessageV1(scope, childDirectory)
	if err != nil {
		t.Fatal(err)
	}

	fields, version := parseDirectoryAdmissionMessage(t, message)
	if version != DirectoryAdmissionV1 || len(fields) != 7 {
		t.Fatalf("version=%d fields=%d", version, len(fields))
	}
	wantFields := [][]byte{
		[]byte(directoryAdmissionDomain),
		intent.Digest().Bytes(),
		childDirectory.DirectoryID.Bytes(),
		childDirectory.Generation.Bytes(),
		rootAdmission.Bytes(),
		[]byte(childDirectory.Path),
	}
	for index, want := range wantFields {
		if !bytes.Equal(fields[index], want) {
			t.Fatalf("field[%d]=%x want=%x", index, fields[index], want)
		}
	}
	modifiedBytes := fields[6]
	if len(modifiedBytes) != 14 || modifiedBytes[0] != 1 ||
		int64(binary.BigEndian.Uint64(modifiedBytes[1:9])) != modified.Seconds() ||
		binary.BigEndian.Uint32(modifiedBytes[9:13]) != modified.Nanoseconds() ||
		modifiedBytes[13] != byte(modified.Precision()) {
		t.Fatalf("modified frame=%x", modifiedBytes)
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
	if admission.SchemaVersion() != DirectoryAdmissionV1 || admission.IntentDigest() != intent.Digest() ||
		admission.DirectoryID() != childDirectory.DirectoryID || admission.Generation() != childDirectory.Generation ||
		admission.Path() != childDirectory.Path || admission.ModifiedTime() != modified ||
		!bytes.Equal(admission.ParentToken(), rootAdmission.Bytes()) {
		t.Fatalf("admission snapshot=%+v", admission)
	}
}

func TestDirectoryAdmissionV1UsesMinimalAbsentModifiedTimeEncoding(t *testing.T) {
	root := admissionTestDirectoryID(t, 0x21)
	intent := admissionTestIntent(t, root, 0x81)
	scope := admissionTestScope(t, intent)
	directory := OutputDirectory{DirectoryID: root, Generation: admissionTestGeneration(t, 0x51)}
	message, err := CanonicalDirectoryAdmissionMessageV1(scope, directory)
	if err != nil {
		t.Fatal(err)
	}
	fields, _ := parseDirectoryAdmissionMessage(t, message)
	if !bytes.Equal(fields[4], []byte{}) || !bytes.Equal(fields[5], []byte{}) ||
		!bytes.Equal(fields[6], []byte{0}) {
		t.Fatalf("parent=%x path=%x modified=%x", fields[4], fields[5], fields[6])
	}
}

func TestDirectoryAdmissionV1BindsIntentAndSyntheticRoot(t *testing.T) {
	if _, err := NewDirectoryAdmissionScope(TransferIntent{}); !errors.Is(err, ErrInvalidDirectoryAdmission) {
		t.Fatalf("zero intent scope error=%v", err)
	}
	root := admissionTestDirectoryID(t, 0x22)
	firstIntent := admissionTestIntent(t, root, 0x82)
	secondIntent := admissionTestIntent(t, root, 0x92)
	firstScope := admissionTestScope(t, firstIntent)
	secondScope := admissionTestScope(t, secondIntent)
	secret := admissionTestSequence(0x40, directoryAdmissionSecretBytes)
	directory := OutputDirectory{DirectoryID: root, Generation: admissionTestGeneration(t, 0x52)}

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

func TestDirectoryAdmissionV1RejectsForeignOrNonImmediateParentClaims(t *testing.T) {
	root := admissionTestDirectoryID(t, 0x24)
	intent := admissionTestIntent(t, root, 0x84)
	foreignIntent := admissionTestIntent(t, root, 0x94)
	scope := admissionTestScope(t, intent)
	foreignScope := admissionTestScope(t, foreignIntent)
	secret := admissionTestSequence(0x60, directoryAdmissionSecretBytes)
	rootDirectory := OutputDirectory{DirectoryID: root, Generation: admissionTestGeneration(t, 0x54)}
	parent, err := NewDirectoryAdmissionWithSecret(secret, scope, rootDirectory)
	if err != nil {
		t.Fatal(err)
	}
	foreignParent, err := NewDirectoryAdmissionWithSecret(secret, foreignScope, rootDirectory)
	if err != nil {
		t.Fatal(err)
	}
	for name, directory := range map[string]OutputDirectory{
		"missing parent": {
			DirectoryID: admissionTestDirectoryID(t, 0x34), Generation: admissionTestGeneration(t, 0x64), Path: "child",
		},
		"foreign parent": {
			DirectoryID: admissionTestDirectoryID(t, 0x35), Generation: admissionTestGeneration(t, 0x65),
			Path: "child", ParentAdmission: foreignParent,
		},
		"non-immediate parent": {
			DirectoryID: admissionTestDirectoryID(t, 0x36), Generation: admissionTestGeneration(t, 0x66),
			Path: "child/grandchild", ParentAdmission: parent,
		},
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := NewDirectoryAdmissionWithSecret(secret, scope, directory); !errors.Is(err, ErrInvalidDirectoryAdmission) {
				t.Fatalf("error=%v", err)
			}
		})
	}
}

func TestDirectoryAdmissionV1SnapshotsKeyAndReceiptBytes(t *testing.T) {
	root := admissionTestDirectoryID(t, 0x25)
	intent := admissionTestIntent(t, root, 0x85)
	scope := admissionTestScope(t, intent)
	secret := admissionTestSequence(0x70, directoryAdmissionSecretBytes)
	directory := OutputDirectory{DirectoryID: root, Generation: admissionTestGeneration(t, 0x55)}
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

func parseDirectoryAdmissionMessage(t *testing.T, message []byte) ([][]byte, uint16) {
	t.Helper()
	offset := 0
	readFrame := func() []byte {
		t.Helper()
		if len(message)-offset < 4 {
			t.Fatalf("truncated frame length at %d", offset)
		}
		length := int(binary.BigEndian.Uint32(message[offset : offset+4]))
		offset += 4
		if length < 0 || len(message)-offset < length {
			t.Fatalf("truncated frame payload at %d length=%d", offset, length)
		}
		value := append([]byte(nil), message[offset:offset+length]...)
		offset += length
		return value
	}
	fields := [][]byte{readFrame()}
	if len(message)-offset < 2 {
		t.Fatal("missing schema version")
	}
	version := binary.BigEndian.Uint16(message[offset : offset+2])
	offset += 2
	for range 6 {
		fields = append(fields, readFrame())
	}
	if offset != len(message) {
		t.Fatalf("trailing bytes=%d", len(message)-offset)
	}
	return fields, version
}

func admissionTestIntent(t *testing.T, root catalog.DirectoryID, targetFirst byte) TransferIntent {
	t.Helper()
	rules, err := NewSelectionRules(true, nil)
	if err != nil {
		t.Fatal(err)
	}
	target, err := NewOpaqueOutputTarget(admissionTestSequence(targetFirst, OutputRootIdentityBytes))
	if err != nil {
		t.Fatal(err)
	}
	intent, err := NewTransferIntent(
		admissionTestShare(t, 0x10), root, rules, target,
		OutputBackendID("test/directory-admission"), OutputNativeTree,
	)
	if err != nil {
		t.Fatal(err)
	}
	return intent
}

func admissionTestScope(t *testing.T, intent TransferIntent) DirectoryAdmissionScope {
	t.Helper()
	scope, err := NewDirectoryAdmissionScope(intent)
	if err != nil {
		t.Fatal(err)
	}
	return scope
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
