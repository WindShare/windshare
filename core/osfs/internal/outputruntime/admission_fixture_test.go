package outputruntime

import (
	"context"
	"errors"
	"path/filepath"
	"testing"

	"github.com/windshare/windshare/core/catalog"
	"github.com/windshare/windshare/core/osfs/internal/outputcap"
	"github.com/windshare/windshare/core/osfs/internal/resumestate"
	"github.com/windshare/windshare/core/transfer"
)

// runtimeTestDecoratedPublicOperationGuard keeps the native guard alive while
// fault decorators observe the exact borrowed root that owns placement authority.
// Substituting Platform.Root here would bypass the scoped Windows rename guard.
type runtimeTestDecoratedPublicOperationGuard struct {
	outputcap.PublicOperationGuard
	root outputcap.Directory
}

func (guard *runtimeTestDecoratedPublicOperationGuard) Root() outputcap.Directory {
	if guard == nil {
		return nil
	}
	return guard.root
}

func (guard *runtimeTestDecoratedPublicOperationGuard) Close() error {
	if guard == nil {
		return nil
	}
	guard.root = nil
	if guard.PublicOperationGuard == nil {
		return nil
	}
	err := guard.PublicOperationGuard.Close()
	guard.PublicOperationGuard = nil
	return err
}

func acquireRuntimeTestDecoratedPublicOperationGuard(
	platform outputcap.Platform,
	decorate func(outputcap.Directory) outputcap.Directory,
) (outputcap.PublicOperationGuard, error) {
	guard, err := platform.AcquirePublicOperationGuard()
	if err != nil {
		return nil, err
	}
	return &runtimeTestDecoratedPublicOperationGuard{
		PublicOperationGuard: guard,
		root:                 decorate(guard.Root()),
	}, nil
}

func newAdmissionTestRoot(t *testing.T) string {
	t.Helper()
	root := newRuntimeTestRootSpec(t)
	platform, err := openOutputRuntimeTestPlatform(root.path, root.create)
	if errors.Is(err, outputcap.ErrRecoverableOutputUnsupported) {
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
	return root.path
}

type runtimeTestRootSpec struct {
	path   string
	create bool
}

func newAdmissionTestAuthority(
	t *testing.T,
	root string,
	platformFactory PlatformFactory,
) *Authority {
	t.Helper()
	if platformFactory == nil {
		platformFactory = openOutputRuntimeTestPlatform
	}
	authority, err := New(Config{RootPath: root, PlatformFactory: platformFactory})
	if err != nil {
		t.Fatal(err)
	}
	return authority
}

type admissionTestOpenedSelection struct {
	Session *Session
}

func openAdmissionTestSelection(
	ctx context.Context,
	authority *Authority,
	selection transfer.OutputSelection,
) (admissionTestOpenedSelection, error) {
	opened, err := authority.OpenSelection(ctx, selection)
	if err != nil {
		return admissionTestOpenedSelection{}, err
	}
	session, ok := opened.(*Session)
	if !ok || session == nil {
		return admissionTestOpenedSelection{}, transfer.ErrInvalidOutputBinding
	}
	return admissionTestOpenedSelection{Session: session}, nil
}

func openRequiredAdmissionTestSelection(
	t *testing.T,
	authority *Authority,
	root string,
	selection transfer.OutputSelection,
) admissionTestOpenedSelection {
	t.Helper()
	if authority == nil || authority.rootPath != root {
		t.Fatal("admission fixture authority is not bound to the requested root")
	}
	opened, err := openAdmissionTestSelection(context.Background(), authority, selection)
	if err != nil {
		t.Fatal(err)
	}
	return opened
}

func closeAdmissionTestSession(t *testing.T, session *Session) {
	t.Helper()
	if session == nil {
		t.Fatal("admission fixture returned no session")
	}
	if err := session.shutdownOwner(); err != nil {
		t.Fatal(err)
	}
}

func admissionTestSessionPath(
	root string,
	selection transfer.OutputSelection,
	sessionID transfer.OutputSessionID,
) string {
	return filepath.Join(
		root,
		resumestate.ControlDirectoryName,
		resumestate.SessionsDirectoryName,
		resumestate.ResumeNamespaceName(selection.ResumeIntent()),
		resumestate.SessionDirectoryName(sessionID),
	)
}

func admissionTestIdentity16[T ~[catalog.IdentityBytes]byte](value byte) T {
	var identity T
	for index := range identity {
		identity[index] = value
	}
	return identity
}

func admissionTestModifiedTime(t *testing.T) catalog.ModifiedTime {
	t.Helper()
	modified, err := catalog.NewModifiedTime(1_700_000_000, 0, catalog.TimePrecisionSeconds)
	if err != nil {
		t.Fatal(err)
	}
	return modified
}

func admissionTestSelectionPaths(
	t *testing.T,
	paths []string,
	exactSize uint64,
) transfer.OutputSelection {
	t.Helper()
	share := admissionTestIdentity16[catalog.ShareInstance](1)
	root := admissionTestIdentity16[catalog.DirectoryID](2)
	generation := admissionTestIdentity16[catalog.DirectoryGeneration](3)
	files := make([]transfer.OutputSelectionFile, 0, len(paths))
	for index, path := range paths {
		files = append(files, transfer.OutputSelectionFile{
			Path: path, FileID: admissionTestIdentity16[catalog.FileID](byte(4 + index)),
			ParentDirectoryID: root, ParentGeneration: generation,
			ExpectedSize: exactSize, ModifiedTime: admissionTestModifiedTime(t),
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

func requireAdmissionTestFault(
	t *testing.T,
	err error,
	scope transfer.OutputFaultScope,
	code transfer.OutputFaultCode,
) {
	t.Helper()
	var fault *transfer.OutputFault
	if !errors.As(err, &fault) || fault.Scope() != scope || fault.Code() != code {
		t.Fatalf("output fault = %v, want scope=%v code=%v", err, scope, code)
	}
}
