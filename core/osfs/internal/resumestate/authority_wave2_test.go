package resumestate

import (
	"bytes"
	"errors"
	"testing"

	"github.com/windshare/windshare/core/catalog"
	"github.com/windshare/windshare/core/content"
	"github.com/windshare/windshare/core/transfer"
)

func TestSessionAuthorityWave2LiveSelectionIsBoundAndCopyOnWrite(t *testing.T) {
	authority := testSessionAuthority(t, SessionActive)
	if count, err := authority.AuthorizedFileCount(); err != nil || count != 1 {
		t.Fatalf("static authorized count = %d, %v", count, err)
	}
	if _, found := authority.selectedFile("missing"); found {
		t.Fatal("missing static file was selected")
	}
	if _, found := authority.liveFile("missing"); found {
		t.Fatal("missing live file was selected")
	}

	selection := authority.Selection()
	directories := selection.Directories()
	if len(directories) != 1 {
		t.Fatalf("fixture directories = %d", len(directories))
	}
	secret := bytes.Repeat([]byte{0x61}, 32)
	rootAdmission, err := transfer.NewDirectoryAdmissionWithSecret(secret, transfer.OutputDirectory{
		DirectoryID: selection.SyntheticRoot(), Generation: selection.RootGeneration(),
	})
	if err != nil {
		t.Fatalf("root admission: %v", err)
	}
	child := directories[0]
	childAdmission, err := transfer.NewDirectoryAdmissionWithSecret(secret, transfer.OutputDirectory{
		DirectoryID: child.DirectoryID, Generation: child.Generation, ParentAdmission: rootAdmission,
		Path: child.Path, ModifiedTime: child.ModifiedTime,
	})
	if err != nil {
		t.Fatalf("child admission: %v", err)
	}
	var fileID catalog.FileID
	fileID[0] = 0xaa
	var revision content.FileRevision
	revision[0] = 0xbb
	live := LiveFileSelection{
		IntentDigest: authority.Header().IntentDigest(),
		Selection: transfer.OutputSelectionFile{
			Path: "folder/new.bin", FileID: fileID, ParentDirectoryID: child.DirectoryID,
			ParentGeneration: child.Generation, ExpectedSize: 7,
		},
		Revision:        revision,
		ParentAdmission: childAdmission,
	}
	key, err := live.Key()
	if err != nil || !live.valid() || key.CanonicalPath != live.Selection.Path || key.ExactSize != 7 {
		t.Fatalf("live key = %+v, %v", key, err)
	}
	withLive, err := authority.WithLiveFileSelection(live)
	if err != nil {
		t.Fatalf("add live file: %v", err)
	}
	if count, err := withLive.AuthorizedFileCount(); err != nil || count != 2 {
		t.Fatalf("live authorized count = %d, %v", count, err)
	}
	selected, found := withLive.selectedFile(live.Selection.Path)
	if !found || selected != live.Selection {
		t.Fatalf("selected live file = %+v, found=%v", selected, found)
	}
	if got, found := withLive.liveFile(live.Selection.Path); !found || got != live {
		t.Fatalf("live lookup = %+v, found=%v", got, found)
	}
	// Re-adding the exact key is idempotent and returns the unchanged authority.
	idempotent, err := withLive.WithLiveFileSelection(live)
	idempotentCount, countErr := idempotent.AuthorizedFileCount()
	if err != nil || countErr != nil || idempotentCount != 2 {
		t.Fatalf("idempotent live add = %v/%v", err, countErr)
	}
	if _, found := authority.liveFile(live.Selection.Path); found {
		t.Fatal("copy-on-write mutated original authority")
	}

	shadow := live
	shadow.Selection.Path = "folder/file.bin"
	if _, err := authority.WithLiveFileSelection(shadow); err == nil || !errors.Is(err, ErrInvalidState) {
		t.Fatalf("static shadow accepted: %v", err)
	}
	changedIntent := live
	changedIntent.IntentDigest[0]++
	if _, err := withLive.WithLiveFileSelection(changedIntent); err == nil || !errors.Is(err, ErrInvalidState) {
		t.Fatalf("intent change accepted: %v", err)
	}
	invalid := live
	invalid.Revision = content.FileRevision{}
	if _, err := authority.WithLiveFileSelection(invalid); err == nil || !errors.Is(err, ErrInvalidState) {
		t.Fatalf("invalid live file accepted: %v", err)
	}

	var nilAuthority SessionAuthority
	if _, err := nilAuthority.AuthorizedFileCount(); err == nil || !errors.Is(err, ErrInvalidState) {
		t.Fatalf("invalid authority count = %v", err)
	}
}
