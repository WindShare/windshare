package osfs

import (
	"bytes"
	"context"
	"errors"
	"path/filepath"
	"sync"
	"testing"

	"github.com/windshare/windshare/core/catalog"
	"github.com/windshare/windshare/core/content"
	"github.com/windshare/windshare/core/osfs/internal/resumestate"
	"github.com/windshare/windshare/core/transfer"
)

const v3RecoveryFilePath = "file.bin"

func v3RecoveryIdentity16[T ~[catalog.IdentityBytes]byte](value byte) T {
	var identity T
	for index := range identity {
		identity[index] = value
	}
	return identity
}

func v3RecoveryModifiedTime(t *testing.T) catalog.ModifiedTime {
	t.Helper()
	modified, err := catalog.NewModifiedTime(1_700_000_000, 0, catalog.TimePrecisionSeconds)
	if err != nil {
		t.Fatal(err)
	}
	return modified
}

func v3RecoverySelection(t *testing.T, withFile bool, exactSize uint64) transfer.OutputSelection {
	t.Helper()
	var paths []string
	if withFile {
		paths = []string{v3RecoveryFilePath}
	}
	return v3RecoverySelectionPaths(t, paths, exactSize)
}

func v3RecoverySelectionPaths(t *testing.T, paths []string, exactSize uint64) transfer.OutputSelection {
	t.Helper()
	share := v3RecoveryIdentity16[catalog.ShareInstance](1)
	root := v3RecoveryIdentity16[catalog.DirectoryID](2)
	generation := v3RecoveryIdentity16[catalog.DirectoryGeneration](3)
	files := make([]transfer.OutputSelectionFile, 0, len(paths))
	for index, path := range paths {
		files = append(files, transfer.OutputSelectionFile{
			Path: path, FileID: v3RecoveryIdentity16[catalog.FileID](byte(4 + index)),
			ParentDirectoryID: root, ParentGeneration: generation,
			ExpectedSize: exactSize, ModifiedTime: v3RecoveryModifiedTime(t),
		})
	}
	plan, err := transfer.NewOutputSelection(share, root, generation, nil, files)
	if err != nil {
		t.Fatal(err)
	}
	rules, err := transfer.NewSelectionRules(true, nil)
	if err != nil {
		t.Fatal(err)
	}
	request, err := transfer.NewCanonicalSelectionRequest(share, root, rules)
	if err != nil {
		t.Fatal(err)
	}
	canonical, err := transfer.NewCanonicalSelectionV1(request, plan)
	if err != nil {
		t.Fatal(err)
	}
	selection, err := canonical.BindPlan(plan)
	if err != nil {
		t.Fatal(err)
	}
	return selection
}

func v3RecoveryAncestryBinding(
	t *testing.T,
	root resumestate.OutputRootBinding,
	selection transfer.OutputSelection,
) resumestate.OutputAncestryBinding {
	t.Helper()
	claims := []resumestate.OutputAncestryIdentityClaim{
		{CanonicalPath: "", IdentityClaim: []byte("test-root-ancestry")},
	}
	for _, directory := range selection.Directories() {
		claims = append(claims, resumestate.OutputAncestryIdentityClaim{
			CanonicalPath: directory.Path,
			IdentityClaim: []byte("test-directory-ancestry:" + directory.Path),
		})
	}
	binding, err := resumestate.NewOutputAncestryBinding(root, selection.Identity(), claims)
	if err != nil {
		t.Fatal(err)
	}
	return binding
}

type v3RecoverySessionIDs struct {
	mu   sync.Mutex
	next byte
}

func (generator *v3RecoverySessionIDs) NewOutputSessionID() (transfer.OutputSessionID, error) {
	generator.mu.Lock()
	defer generator.mu.Unlock()
	generator.next++
	return v3RecoveryIdentity16[transfer.OutputSessionID](generator.next), nil
}

type v3RecoveryObjectIDs struct {
	mu   sync.Mutex
	next byte
}

func (generator *v3RecoveryObjectIDs) NewOutputObjectID() (resumestate.OutputObjectID, error) {
	generator.mu.Lock()
	defer generator.mu.Unlock()
	generator.next++
	raw := bytes.Repeat([]byte{generator.next}, resumestate.OutputObjectIDBytes)
	return resumestate.OutputObjectIDFromBytes(raw)
}

func v3RecoveryAuthority(t *testing.T, root string, sessions *v3RecoverySessionIDs) *FilesystemOutputAuthority {
	t.Helper()
	if sessions == nil {
		sessions = &v3RecoverySessionIDs{}
	}
	authority, err := NewFilesystemOutputAuthority(FilesystemOutputAuthorityConfig{RootPath: root})
	if err != nil {
		t.Fatal(err)
	}
	authority.sessionIDs = sessions
	authority.objectIDs = &v3RecoveryObjectIDs{}
	authority.random = bytes.NewReader(bytes.Repeat([]byte{0xa5}, 64*1024))
	return authority
}

func v3RecoveryRoot(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	platform, err := openOutputV3Platform(root, false)
	if errors.Is(err, errUnsupportedOutputVolume) {
		t.Skipf("certified output filesystem unavailable: %v", err)
	}
	if err != nil {
		t.Fatal(err)
	}
	if err := platform.ProbeRecoverableFeatures(); err != nil {
		_ = platform.Close()
		t.Fatalf("probe certified output filesystem: %v", err)
	}
	if err := platform.Close(); err != nil {
		t.Fatal(err)
	}
	return root
}

type v3OpenedSelection struct {
	Session *filesystemOutputSession
}

func v3OpenSelection(
	ctx context.Context,
	authority *FilesystemOutputAuthority,
	selection transfer.OutputSelection,
) (v3OpenedSelection, error) {
	var contract transfer.OutputAuthority = authority
	opened, err := contract.OpenSelection(ctx, selection)
	if err != nil {
		return v3OpenedSelection{}, err
	}
	session, ok := opened.(*filesystemOutputSession)
	if !ok || session == nil {
		return v3OpenedSelection{}, transfer.ErrInvalidOutputBinding
	}
	return v3OpenedSelection{Session: session}, nil
}

func v3RecoveryOpen(
	t *testing.T,
	authority *FilesystemOutputAuthority,
	root string,
	selection transfer.OutputSelection,
) v3OpenedSelection {
	t.Helper()
	if authority == nil || authority.rootPath != root {
		t.Fatal("recovery fixture authority is not bound to the requested root")
	}
	opened, err := v3OpenSelection(context.Background(), authority, selection)
	if err != nil {
		t.Fatal(err)
	}
	return opened
}

func v3RecoverySessionPath(root string, selection transfer.OutputSelection, sessionID transfer.OutputSessionID) string {
	return filepath.Join(
		root,
		resumestate.ControlDirectoryName,
		resumestate.SessionsDirectoryName,
		resumestate.ResumeNamespaceName(selection.ResumeIntent()),
		resumestate.SessionDirectoryName(sessionID),
	)
}

func v3RecoveryOutputFile(
	t *testing.T,
	session *filesystemOutputSession,
	selection transfer.OutputSelection,
	exactSize uint64,
) transfer.OutputFile {
	t.Helper()
	if len(selection.Files()) == 0 || selection.Files()[0].ExpectedSize != exactSize {
		t.Fatal("recovery output file size differs from its canonical selection")
	}
	return v3RecoveryOutputFileAt(t, session, selection, 0)
}

func v3RecoveryOutputFileAt(
	t *testing.T,
	session *filesystemOutputSession,
	selection transfer.OutputSelection,
	index int,
) transfer.OutputFile {
	t.Helper()
	selected := selection.Files()[index]
	geometry, err := content.NewFileGeometry(selected.ExpectedSize, catalog.DefaultChunkSize)
	if err != nil {
		t.Fatal(err)
	}
	descriptor, err := content.NewFileRevisionDescriptor(
		selection.ShareInstance(), selected.FileID,
		v3RecoveryIdentity16[content.FileRevision](byte(5+index)), geometry, selected.ModifiedTime,
	)
	if err != nil {
		t.Fatal(err)
	}
	target, err := outputTargetForDescriptor(session.SessionID(), descriptor, selected.Path)
	if err != nil {
		t.Fatal(err)
	}
	return transfer.OutputFile{
		Path: selected.Path, ExpectedSize: selected.ExpectedSize, Descriptor: descriptor, Target: target,
	}
}

func v3RecoveryCloseSession(t *testing.T, session *filesystemOutputSession) {
	t.Helper()
	if err := session.closeHandles(); err != nil {
		t.Fatal(err)
	}
}

func v3RecoveryCloseInventory(t *testing.T, inventory *ResumeStateInventory) {
	t.Helper()
	if err := inventory.Close(); err != nil {
		t.Errorf("close resume-state inventory: %v", err)
	}
}
