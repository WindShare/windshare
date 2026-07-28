//go:build windows

package osfs

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/windshare/windshare/core/catalog"
	"github.com/windshare/windshare/core/osfs/internal/outputcap"
	"github.com/windshare/windshare/core/osfs/internal/resumestate"
	"github.com/windshare/windshare/core/transfer"
	"golang.org/x/sys/windows"
)

func TestWindowsV3SelectionRejectsDOSAliasOfControlBeforeMutation(t *testing.T) {
	rootPath := t.TempDir()
	traces := &windowsV3FeatureProbeTrace{}
	authority := windowsV3BootstrapPublicControl(t, rootPath, traces)

	controlPath := filepath.Join(rootPath, resumestate.ControlDirectoryName)
	shortLeaf := windowsV3TestShortLeaf(t, controlPath)
	if strings.EqualFold(shortLeaf, resumestate.ControlDirectoryName) {
		t.Skip("test NTFS volume does not assign a DOS alias to the installed control directory")
	}
	longInfo, err := os.Stat(controlPath)
	if err != nil {
		t.Fatal(err)
	}
	aliasInfo, err := os.Stat(filepath.Join(rootPath, shortLeaf))
	if err != nil || !os.SameFile(longInfo, aliasInfo) {
		t.Fatalf("short control name %q is not an alias: same=%t error=%v", shortLeaf, os.SameFile(longInfo, aliasInfo), err)
	}
	rootBefore := windowsV3TestEntryNames(t, rootPath)
	controlBefore := windowsV3TestEntryNames(t, controlPath)
	traces.Reset()

	attacks := []struct {
		name      string
		selection transfer.OutputSelection
	}{
		{name: "directory-descent", selection: windowsV3TestDirectorySelection(t, shortLeaf+"/must-not-exist")},
		{name: "root-file-locator", selection: windowsV3TestFileSelection(t, []string{shortLeaf}, 1)},
	}
	for _, attack := range attacks {
		t.Run(attack.name, func(t *testing.T) {
			session, err := authority.OpenSelection(context.Background(), attack.selection)
			if err == nil || !errors.Is(err, outputcap.ErrUnsafeNamespace) {
				if session != nil {
					_, _ = session.PauseJob(context.Background(), transfer.JobPauseOutputFailure)
				}
				t.Fatalf("DOS alias selection error = %v, want unsafe-name rejection", err)
			}
			outputV3ControlSessionRequireFault(
				t, err, transfer.OutputFaultSession, transfer.OutputFaultNamespaceUnsafe,
			)
		})
	}
	if traces.ProbeCalls() != 0 {
		t.Fatalf("reserved DOS alias reached recoverability probe %d times", traces.ProbeCalls())
	}
	if got := windowsV3TestEntryNames(t, rootPath); strings.Join(got, "\x00") != strings.Join(rootBefore, "\x00") {
		t.Fatalf("rejected DOS alias changed output root: before=%v after=%v", rootBefore, got)
	}
	if got := windowsV3TestEntryNames(t, controlPath); strings.Join(got, "\x00") != strings.Join(controlBefore, "\x00") {
		t.Fatalf("rejected DOS alias changed control namespace: before=%v after=%v", controlBefore, got)
	}
	if _, err := os.Lstat(filepath.Join(controlPath, "must-not-exist")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("DOS alias selection created a child in control: %v", err)
	}
}

// windowsV3FeatureProbeTrace observes the facade's durable-operation milestones,
// avoiding a test-only hook into the runtime's native platform factory.
type windowsV3FeatureProbeTrace struct {
	probeCalls int
}

func (trace *windowsV3FeatureProbeTrace) TraceFilesystemOutput(event FilesystemOutputTrace) {
	if event.Operation == TraceFeatureProbeCompleted {
		trace.probeCalls++
	}
}

func (trace *windowsV3FeatureProbeTrace) Reset() { trace.probeCalls = 0 }

func (trace *windowsV3FeatureProbeTrace) ProbeCalls() int { return trace.probeCalls }

func windowsV3BootstrapPublicControl(
	t *testing.T,
	rootPath string,
	tracer FilesystemOutputTracer,
) *FilesystemOutputAuthority {
	t.Helper()
	authority, err := NewFilesystemOutputAuthority(FilesystemOutputAuthorityConfig{
		RootPath: rootPath, Tracer: tracer,
	})
	if err != nil {
		t.Fatal(err)
	}
	bootstrap, err := authority.OpenSelection(context.Background(), publicValuesSelection(t))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := bootstrap.PauseJob(context.Background(), transfer.JobPauseInterrupted); err != nil {
		t.Fatal(err)
	}
	return authority
}

func windowsV3TestDirectorySelection(t *testing.T, path string) transfer.OutputSelection {
	t.Helper()
	share := windowsV3TestIdentity16[catalog.ShareInstance](0x71)
	root := windowsV3TestIdentity16[catalog.DirectoryID](0x72)
	rootGeneration := windowsV3TestIdentity16[catalog.DirectoryGeneration](0x73)
	components := strings.Split(path, "/")
	directories := make([]transfer.OutputSelectionDirectory, 0, len(components))
	for index := range components {
		directories = append(directories, transfer.OutputSelectionDirectory{
			Path:         strings.Join(components[:index+1], "/"),
			DirectoryID:  windowsV3TestIdentity16[catalog.DirectoryID](byte(0x74 + index)),
			Generation:   windowsV3TestIdentity16[catalog.DirectoryGeneration](byte(0x84 + index)),
			ModifiedTime: windowsV3TestModifiedTime(t),
		})
	}
	plan, err := transfer.NewOutputSelection(share, root, rootGeneration, directories, nil)
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

func windowsV3TestFileSelection(t *testing.T, paths []string, size uint64) transfer.OutputSelection {
	t.Helper()
	share := windowsV3TestIdentity16[catalog.ShareInstance](0x91)
	root := windowsV3TestIdentity16[catalog.DirectoryID](0x92)
	rootGeneration := windowsV3TestIdentity16[catalog.DirectoryGeneration](0x93)
	files := make([]transfer.OutputSelectionFile, 0, len(paths))
	for index, path := range paths {
		files = append(files, transfer.OutputSelectionFile{
			Path: path, FileID: windowsV3TestIdentity16[catalog.FileID](byte(0x94 + index)),
			ParentDirectoryID: root, ParentGeneration: rootGeneration,
			ExpectedSize: size, ModifiedTime: windowsV3TestModifiedTime(t),
		})
	}
	plan, err := transfer.NewOutputSelection(share, root, rootGeneration, nil, files)
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

func windowsV3TestIdentity16[T ~[catalog.IdentityBytes]byte](value byte) T {
	var identity T
	for index := range identity {
		identity[index] = value
	}
	return identity
}

func windowsV3TestModifiedTime(t *testing.T) catalog.ModifiedTime {
	t.Helper()
	modified, err := catalog.NewModifiedTime(1_700_000_000, 0, catalog.TimePrecisionSeconds)
	if err != nil {
		t.Fatal(err)
	}
	return modified
}

func windowsV3TestShortLeaf(t *testing.T, path string) string {
	t.Helper()
	longPath, err := windows.UTF16PtrFromString(path)
	if err != nil {
		t.Fatal(err)
	}
	buffer := make([]uint16, 32<<10)
	count, err := windows.GetShortPathName(longPath, &buffer[0], uint32(len(buffer)))
	if err != nil {
		t.Fatal(err)
	}
	if count == 0 || count >= uint32(len(buffer)) {
		t.Fatalf("invalid short-path length %d", count)
	}
	return filepath.Base(windows.UTF16ToString(buffer[:count]))
}

func windowsV3TestEntryNames(t *testing.T, path string) []string {
	t.Helper()
	entries, err := os.ReadDir(path)
	if err != nil {
		t.Fatal(err)
	}
	names := make([]string, len(entries))
	for index := range entries {
		names[index] = entries[index].Name()
	}
	return names
}
