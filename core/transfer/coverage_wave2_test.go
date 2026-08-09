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
	parentOutput := MaterializationDirectory{
		DirectoryID: root,
		Generation:  transferID[catalog.DirectoryGeneration](0x62),
	}
	parent, err := NewDirectoryAdmissionWithSecret(secret, scope, parentOutput)
	if err != nil {
		t.Fatal(err)
	}
	childOutput := MaterializationDirectory{
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
	for name, invalid := range map[string]MaterializationDirectory{
		"path": func() MaterializationDirectory { value := childOutput; value.Path = "other"; return value }(),
		"generation": func() MaterializationDirectory {
			value := childOutput
			value.Generation = transferID[catalog.DirectoryGeneration](0x65)
			return value
		}(),
		"parent": func() MaterializationDirectory {
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
	for name, invalid := range map[string]MaterializationDirectory{
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
