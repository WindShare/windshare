package transfer

import (
	"bytes"
	"errors"
	"path/filepath"
	"testing"

	"github.com/windshare/windshare/core/catalog"
)

func TestTransferWave2TargetAndIdentityEdges(t *testing.T) {
	raw := bytes.Repeat([]byte{0x3a}, OutputRootIdentityBytes)
	opaque, err := NewOpaqueOutputTarget(raw)
	if err != nil || opaque.IsZero() || opaque.Kind() != OutputOpaqueTarget || opaque.RootPath() != "" {
		t.Fatalf("opaque target = %+v, %v", opaque, err)
	}
	identityBytes := opaque.Identity().Bytes()
	identityBytes[0] ^= 0xff
	if opaque.Identity().Bytes()[0] != 0x3a {
		t.Fatal("output-root identity accessor leaked mutable storage")
	}
	if !opaque.Equal(opaque) {
		t.Fatal("target did not equal itself")
	}
	for _, invalid := range [][]byte{nil, make([]byte, OutputRootIdentityBytes-1), make([]byte, OutputRootIdentityBytes+1), make([]byte, OutputRootIdentityBytes)} {
		if len(invalid) == OutputRootIdentityBytes {
			if _, err := NewOpaqueOutputTarget(invalid); err == nil {
				t.Fatal("zero opaque identity accepted")
			}
			continue
		}
		if _, err := NewOpaqueOutputTarget(invalid); !errors.Is(err, ErrInvalidOutputBinding) {
			t.Fatalf("opaque length %d error = %v", len(invalid), err)
		}
	}

	filesystem, err := NewFilesystemOutputRootTarget(filepath.Join(t.TempDir(), "out"))
	if err != nil {
		t.Fatal(err)
	}
	if filesystem.Equal(opaque) || !filesystem.valid() {
		t.Fatal("filesystem and opaque target kinds were conflated")
	}
	malformedFilesystem := filesystem
	malformedFilesystem.rootPath = "relative"
	if malformedFilesystem.valid() {
		t.Fatal("relative filesystem target was accepted by the semantic validator")
	}
	malformedOpaque := opaque
	malformedOpaque.rootPath = "unexpected"
	if malformedOpaque.valid() {
		t.Fatal("opaque target carried a filesystem path")
	}
	unknownKind := filesystem
	unknownKind.kind = OutputTargetKind(255)
	if unknownKind.valid() {
		t.Fatal("unknown output target kind was accepted")
	}

	digestRaw := bytes.Repeat([]byte{0x4b}, TransferIntentDigestBytes)
	digest, err := TransferIntentDigestFromBytes(digestRaw)
	if err != nil || digest.IsZero() {
		t.Fatalf("intent digest = %x, %v", digest, err)
	}
	digestRaw[0] = 0
	if digest.Bytes()[0] != 0x4b {
		t.Fatal("intent digest accessor leaked mutable storage")
	}
	for _, invalid := range [][]byte{nil, make([]byte, TransferIntentDigestBytes-1), make([]byte, TransferIntentDigestBytes+1), make([]byte, TransferIntentDigestBytes)} {
		if len(invalid) == TransferIntentDigestBytes {
			if _, err := TransferIntentDigestFromBytes(invalid); err == nil {
				t.Fatal("zero intent digest accepted")
			}
			continue
		}
		if _, err := TransferIntentDigestFromBytes(invalid); !errors.Is(err, ErrInvalidTransferIntent) {
			t.Fatalf("intent digest length %d error = %v", len(invalid), err)
		}
	}

	jobRaw := bytes.Repeat([]byte{0x5c}, TransferJobIdentityBytes)
	jobID, err := TransferJobIDFromBytes(jobRaw)
	if err != nil || jobID.IsZero() || len(jobID.Bytes()) != TransferJobIdentityBytes {
		t.Fatalf("job ID = %x, %v", jobID, err)
	}
	for _, invalid := range [][]byte{nil, make([]byte, TransferJobIdentityBytes-1), make([]byte, TransferJobIdentityBytes+1), make([]byte, TransferJobIdentityBytes)} {
		if len(invalid) == TransferJobIdentityBytes {
			if _, err := TransferJobIDFromBytes(invalid); err == nil {
				t.Fatal("zero job ID accepted")
			}
			continue
		}
		if _, err := TransferJobIDFromBytes(invalid); !errors.Is(err, ErrInvalidTransferIntent) {
			t.Fatalf("job ID length %d error = %v", len(invalid), err)
		}
	}
}

func TestTransferWave2DirectoryAdmissionProjection(t *testing.T) {
	share := transferID[catalog.ShareInstance](0x60)
	root := transferID[catalog.DirectoryID](0x61)
	rules, err := NewSelectionRules(true, nil)
	if err != nil {
		t.Fatal(err)
	}
	scope, err := NewDirectoryAdmissionScope(testTransferIntent(t, share, root, rules, jobOutputBackend))
	if err != nil {
		t.Fatal(err)
	}
	secret := bytes.Repeat([]byte{0x71}, directoryAdmissionSecretBytes)
	parentOutput := OutputDirectory{
		DirectoryID: root,
		Generation:  transferID[catalog.DirectoryGeneration](0x62),
	}
	parent, err := NewDirectoryAdmissionWithSecret(secret, scope, parentOutput)
	if err != nil {
		t.Fatal(err)
	}
	childOutput := OutputDirectory{
		DirectoryID:     transferID[catalog.DirectoryID](0x63),
		Generation:      transferID[catalog.DirectoryGeneration](0x64),
		ParentAdmission: parent,
		Path:            "child",
	}
	child, err := NewDirectoryAdmissionWithSecret(secret, scope, childOutput)
	if err != nil {
		t.Fatal(err)
	}
	if child.IsZero() || child.DirectoryID() != childOutput.DirectoryID || child.Generation() != childOutput.Generation || child.Path() != "child" {
		t.Fatalf("child admission projection = %+v", child)
	}
	if !bytes.Equal(child.Bytes(), child.Bytes()) || child.Equal(parent) || !child.Equal(child) {
		t.Fatal("admission identity comparison is not token-based")
	}
	token := child.Bytes()
	token[0] ^= 1
	if child.Bytes()[0] == token[0] {
		t.Fatal("directory admission bytes leaked mutable storage")
	}
	if !bytes.Equal(child.ParentToken(), parent.Bytes()) || parent.ParentToken() != nil {
		t.Fatalf("parent tokens child=%x root=%x", child.ParentToken(), parent.ParentToken())
	}
	if err := ValidateDirectoryAdmissionBinding(scope, child, childOutput); err != nil {
		t.Fatalf("valid admission binding = %v", err)
	}
	for name, invalid := range map[string]OutputDirectory{
		"path": func() OutputDirectory { value := childOutput; value.Path = "other"; return value }(),
		"generation": func() OutputDirectory {
			value := childOutput
			value.Generation = transferID[catalog.DirectoryGeneration](0x65)
			return value
		}(),
		"parent": func() OutputDirectory {
			value := childOutput
			value.ParentAdmission = DirectoryAdmission{}
			return value
		}(),
	} {
		t.Run("mismatch/"+name, func(t *testing.T) {
			if !errors.Is(ValidateDirectoryAdmissionBinding(scope, child, invalid), ErrDirectoryAdmissionMismatch) {
				t.Fatalf("mismatch %s was accepted", name)
			}
		})
	}
	for _, badSecret := range [][]byte{
		nil,
		make([]byte, directoryAdmissionSecretBytes-1),
		make([]byte, directoryAdmissionSecretBytes),
		make([]byte, directoryAdmissionSecretBytes+1),
	} {
		if _, err := NewDirectoryAdmissionWithSecret(badSecret, scope, parentOutput); !errors.Is(err, ErrInvalidDirectoryAdmission) {
			t.Fatalf("secret length %d error = %v", len(badSecret), err)
		}
	}
	for name, invalid := range map[string]OutputDirectory{
		"zero identity":       {Generation: parentOutput.Generation},
		"zero generation":     {DirectoryID: parentOutput.DirectoryID},
		"path without parent": {DirectoryID: parentOutput.DirectoryID, Generation: parentOutput.Generation, Path: "child"},
		"noncanonical path":   {DirectoryID: parentOutput.DirectoryID, Generation: parentOutput.Generation, ParentAdmission: parent, Path: "child/../other"},
		"parent on root":      {DirectoryID: parentOutput.DirectoryID, Generation: parentOutput.Generation, ParentAdmission: parent},
	} {
		t.Run("invalid/"+name, func(t *testing.T) {
			if _, err := NewDirectoryAdmissionWithSecret(secret, scope, invalid); !errors.Is(err, ErrInvalidDirectoryAdmission) {
				t.Fatalf("invalid directory %s error = %v", name, err)
			}
		})
	}
}
