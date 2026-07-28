package outputruntime

import (
	"bytes"
	"context"
	"errors"
	"path/filepath"
	"sync"
	"testing"

	"github.com/windshare/windshare/core/catalog"
	"github.com/windshare/windshare/core/content"
	"github.com/windshare/windshare/core/osfs/internal/outputcap"
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

func v3SemanticNestedSelection(t *testing.T, exactSize uint64) transfer.OutputSelection {
	t.Helper()
	share := v3RecoveryIdentity16[catalog.ShareInstance](0x41)
	root := v3RecoveryIdentity16[catalog.DirectoryID](0x42)
	rootGeneration := v3RecoveryIdentity16[catalog.DirectoryGeneration](0x43)
	directory := transfer.OutputSelectionDirectory{
		Path: "folder", DirectoryID: v3RecoveryIdentity16[catalog.DirectoryID](0x44),
		Generation: v3RecoveryIdentity16[catalog.DirectoryGeneration](0x45), ModifiedTime: v3RecoveryModifiedTime(t),
	}
	file := transfer.OutputSelectionFile{
		Path: "folder/file.bin", FileID: v3RecoveryIdentity16[catalog.FileID](0x46),
		ParentDirectoryID: directory.DirectoryID, ParentGeneration: directory.Generation,
		ExpectedSize: exactSize, ModifiedTime: v3RecoveryModifiedTime(t),
	}
	plan, err := transfer.NewOutputSelection(
		share, root, rootGeneration,
		[]transfer.OutputSelectionDirectory{directory}, []transfer.OutputSelectionFile{file},
	)
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

func v3RecoveryFaultScope(err error) transfer.OutputFaultScope {
	if fault, found := errors.AsType[*transfer.OutputFault](err); found {
		return fault.Scope()
	}
	return 0
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

func v3RecoveryAuthority(t *testing.T, root string, sessions *v3RecoverySessionIDs) *Authority {
	t.Helper()
	if sessions == nil {
		sessions = &v3RecoverySessionIDs{}
	}
	authority, err := New(Config{
		RootPath:        root,
		PlatformFactory: openOutputRuntimeTestPlatform,
	})
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
	fixture := newRuntimeTestRootSpec(t)
	root := fixture.path
	platform, err := openOutputRuntimeTestPlatform(root, fixture.create)
	if errors.Is(err, outputcap.ErrRecoverableOutputUnsupported) {
		t.Skipf("certified output filesystem unavailable: %v", err)
	}
	if err != nil {
		t.Fatal(err)
	}
	if err := platform.Close(); err != nil {
		t.Fatalf("close output-runtime root reservation: %v", err)
	}
	// The authority open below is the feature-probe boundary. Probing here too
	// doubled the native crash-cut workload for every recovery test while adding
	// no independent evidence; this open only establishes a usable root for
	// fixtures that inspect the namespace directly.
	return root
}

type v3OpenedSelection struct {
	Session *Session
}

func v3OpenSelection(
	ctx context.Context,
	authority *Authority,
	selection transfer.OutputSelection,
) (v3OpenedSelection, error) {
	var contract transfer.OutputAuthority = authority
	opened, err := contract.OpenSelection(ctx, selection)
	if err != nil {
		return v3OpenedSelection{}, err
	}
	session, ok := opened.(*Session)
	if !ok || session == nil {
		return v3OpenedSelection{}, transfer.ErrInvalidOutputBinding
	}
	return v3OpenedSelection{Session: session}, nil
}

func v3RecoveryOpen(
	t *testing.T,
	authority *Authority,
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
	session *Session,
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
	session *Session,
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

func v3RecoveryCloseSession(t *testing.T, session *Session) {
	t.Helper()
	if err := session.shutdownOwner(); err != nil {
		t.Fatal(err)
	}
}

func v3TransactionPublicationWitness(transaction *FileTransaction) *publicationWitness {
	if transaction == nil {
		return nil
	}
	return &publicationWitness{stage: transaction.data, anchor: transaction.anchor}
}

func v3RecoveryCloseInventory(t *testing.T, inventory *ResumeStateInventory) {
	t.Helper()
	if err := inventory.Close(); err != nil {
		t.Errorf("close resume-state inventory: %v", err)
	}
}
