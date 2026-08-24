package transfer

import (
	"bytes"
	"errors"
	"testing"

	"github.com/windshare/windshare/core/catalog"
)

func TestReceiveIntentAndRunIdentityEdges(t *testing.T) {
	digestRaw := bytes.Repeat([]byte{0x4b}, ReceiveIntentDigestBytes)
	digest, err := ReceiveIntentDigestFromBytes(digestRaw)
	if err != nil || digest.IsZero() {
		t.Fatalf("intent digest = %x, %v", digest, err)
	}
	digestRaw[0] = 0
	if digest.Bytes()[0] != 0x4b {
		t.Fatal("intent digest accessor leaked mutable storage")
	}
	for _, invalid := range [][]byte{nil, make([]byte, ReceiveIntentDigestBytes-1), make([]byte, ReceiveIntentDigestBytes+1), make([]byte, ReceiveIntentDigestBytes)} {
		if len(invalid) == ReceiveIntentDigestBytes {
			if _, err := ReceiveIntentDigestFromBytes(invalid); err == nil {
				t.Fatal("zero intent digest accepted")
			}
			continue
		}
		if _, err := ReceiveIntentDigestFromBytes(invalid); !errors.Is(err, ErrInvalidReceiveIntent) {
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
		if _, err := TransferJobIDFromBytes(invalid); !errors.Is(err, ErrInvalidReceiveIntent) {
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
	scope, err := NewDirectoryAdmissionScope(testReceiveIntent(t, share, root, rules))
	if err != nil {
		t.Fatal(err)
	}
	secret := bytes.Repeat([]byte{0x71}, directoryAdmissionSecretBytes)
	parentOutput := admissionTestDirectory(
		t, root, transferID[catalog.DirectoryGeneration](0x62), DirectoryAdmission{}, "", catalog.ModifiedTime{},
	)
	parent, err := NewDirectoryAdmissionWithSecret(
		secret, scope, admissionTestMaterializationDirectory(t, parentOutput),
	)
	if err != nil {
		t.Fatal(err)
	}
	childOutput := admissionTestDirectory(
		t, transferID[catalog.DirectoryID](0x63), transferID[catalog.DirectoryGeneration](0x64),
		parent, "child", catalog.ModifiedTime{},
	)
	childMaterialization := admissionTestMaterializationDirectory(t, childOutput)
	child, err := NewDirectoryAdmissionWithSecret(secret, scope, childMaterialization)
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
	if err := ValidateDirectoryAdmissionBinding(scope, child, childMaterialization); err != nil {
		t.Fatalf("valid admission binding = %v", err)
	}
	for name, invalid := range map[string]AuthenticatedSourceDirectory{
		"path": func() AuthenticatedSourceDirectory {
			value := childOutput
			value.SourcePath = admissionTestDirectory(
				t, value.DirectoryID, value.Generation, value.ParentAdmission, "other", value.ModifiedTime,
			).SourcePath
			return value
		}(),
		"generation": func() AuthenticatedSourceDirectory {
			value := childOutput
			value.Generation = transferID[catalog.DirectoryGeneration](0x65)
			return value
		}(),
		"parent": func() AuthenticatedSourceDirectory {
			value := childOutput
			value.ParentAdmission = DirectoryAdmission{}
			return value
		}(),
	} {
		t.Run("mismatch/"+name, func(t *testing.T) {
			if !errors.Is(ValidateDirectoryAdmissionBinding(
				scope, child, admissionTestMaterializationDirectory(t, invalid),
			), ErrDirectoryAdmissionMismatch) {
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
		if _, err := NewDirectoryAdmissionWithSecret(
			badSecret, scope, admissionTestMaterializationDirectory(t, parentOutput),
		); !errors.Is(err, ErrInvalidDirectoryAdmission) {
			t.Fatalf("secret length %d error = %v", len(badSecret), err)
		}
	}
	for name, invalid := range map[string]AuthenticatedSourceDirectory{
		"zero identity": admissionTestDirectory(
			t, catalog.DirectoryID{}, parentOutput.Generation, DirectoryAdmission{}, "", catalog.ModifiedTime{},
		),
		"zero generation": admissionTestDirectory(
			t, parentOutput.DirectoryID, catalog.DirectoryGeneration{}, DirectoryAdmission{}, "", catalog.ModifiedTime{},
		),
		"path without parent": admissionTestDirectory(
			t, parentOutput.DirectoryID, parentOutput.Generation, DirectoryAdmission{}, "child", catalog.ModifiedTime{},
		),
		"parent on root": admissionTestDirectory(
			t, parentOutput.DirectoryID, parentOutput.Generation, parent, "", catalog.ModifiedTime{},
		),
	} {
		t.Run("invalid/"+name, func(t *testing.T) {
			path, pathErr := NewMaterializationRootRelativePath(invalid.SourcePath.String())
			if pathErr != nil {
				t.Fatal(pathErr)
			}
			directory := MaterializationDirectory{
				directory: invalid.DirectoryID, generation: invalid.Generation,
				path: path, parent: invalid.ParentAdmission, modified: invalid.ModifiedTime,
			}
			if _, err := NewDirectoryAdmissionWithSecret(
				secret, scope, directory,
			); !errors.Is(err, ErrInvalidDirectoryAdmission) {
				t.Fatalf("invalid directory %s error = %v", name, err)
			}
		})
	}
}
