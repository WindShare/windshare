//go:build windows

package outputruntime

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"github.com/windshare/windshare/core/catalog"
	"github.com/windshare/windshare/core/osfs/internal/outputcap"
	"github.com/windshare/windshare/core/transfer"
)

const windowsV3BatchAdmissionEntries = 64

func TestWindowsV3FirstComponentAdmissionUsesOneNativeBatch(t *testing.T) {
	t.Parallel()
	rootPath := v3RecoveryRoot(t)
	files := make([]windowsRuntimeBatchAdmissionFile, windowsV3BatchAdmissionEntries)
	for index := range files {
		files[index] = windowsRuntimeBatchAdmissionFile{
			size: uint64(index), modified: v3RecoveryModifiedTime(t),
		}
		name := fmt.Sprintf("metadata-%03d.bin", index)
		if err := os.WriteFile(filepath.Join(rootPath, name), nil, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	selection := windowsRuntimeBatchAdmissionSelection(t, files)
	authority := v3RecoveryAuthority(t, rootPath, nil)
	nativeFactory := authority.platformFactory
	var countedRoot *windowsV3BatchCountingDirectory
	authority.platformFactory = func(path string, create bool) (outputcap.Platform, error) {
		platform, err := nativeFactory(path, create)
		if err != nil {
			return nil, err
		}
		countedRoot = &windowsV3BatchCountingDirectory{
			Directory: platform.Root(),
			counts:    &windowsV3BatchAdmissionCounts{},
		}
		return &windowsV3BatchCountingPlatform{
			Platform:  platform,
			firstRoot: countedRoot,
		}, nil
	}
	session, err := authority.OpenSelection(context.Background(), selection)
	if err != nil {
		t.Fatal(err)
	}
	concrete, ok := session.(*Session)
	if !ok {
		t.Fatalf("session type = %T", session)
	}
	defer func() { _ = concrete.closeHandles() }()
	if countedRoot == nil {
		t.Fatal("platform factory did not expose the counted root")
	}
	if countedRoot.counts.batchCalls != 1 || countedRoot.counts.singleCalls != 0 {
		t.Fatalf("first-component validation batch=%d single=%d",
			countedRoot.counts.batchCalls, countedRoot.counts.singleCalls)
	}
	if len(selection.Files()) != windowsV3BatchAdmissionEntries {
		t.Fatal("test selection did not preserve its declared admission scale")
	}
}

func TestOutputV3SelectionAuthorityRejectsBeforeNativeProbe(t *testing.T) {
	t.Parallel()
	rootPath := v3RecoveryRoot(t)
	selection := windowsRuntimeBatchAdmissionSelection(t, []windowsRuntimeBatchAdmissionFile{{
		size: 1, modified: v3RecoveryModifiedTime(t),
	}})
	authority := v3RecoveryAuthority(t, rootPath, nil)
	nativeFactory := authority.platformFactory
	probeCalls := 0
	authority.platformFactory = func(path string, create bool) (outputcap.Platform, error) {
		platform, err := nativeFactory(path, create)
		if err != nil {
			return nil, err
		}
		return &windowsV3AuthorityRejectPlatform{
			Platform: platform,
			root: &windowsV3AuthorityRejectDirectory{
				Directory: platform.Root(),
			},
			probeCalls: &probeCalls,
		}, nil
	}
	session, err := authority.OpenSelection(context.Background(), selection)
	var fault *transfer.OutputFault
	if session != nil || !errors.As(err, &fault) || fault.Scope() != transfer.OutputFaultRoot ||
		fault.Code() != transfer.OutputFaultUnsupportedFilesystem {
		t.Fatalf("authority rejection session=%v error=%v", session, err)
	}
	if probeCalls != 0 {
		t.Fatalf("authority rejection reached native probe %d times", probeCalls)
	}
	assertWindowsRuntimeBatchAdmissionRootUntouched(t, rootPath)
}

type windowsV3BatchCountingPlatform struct {
	outputcap.Platform
	firstRoot outputcap.Directory
	rootCalls int
}

func (platform *windowsV3BatchCountingPlatform) Root() outputcap.Directory {
	platform.rootCalls++
	if platform.rootCalls == 1 {
		return platform.firstRoot
	}
	return platform.Platform.Root()
}

func (platform *windowsV3BatchCountingPlatform) AcquirePublicOperationGuard() (
	outputcap.PublicOperationGuard,
	error,
) {
	counted := platform.firstRoot.(*windowsV3BatchCountingDirectory)
	return acquireRuntimeTestDecoratedPublicOperationGuard(
		platform.Platform,
		func(root outputcap.Directory) outputcap.Directory {
			return &windowsV3BatchCountingDirectory{
				Directory: root,
				counts:    counted.counts,
			}
		},
	)
}

type windowsV3BatchAdmissionCounts struct {
	batchCalls  int
	singleCalls int
}

type windowsV3BatchCountingDirectory struct {
	outputcap.Directory
	counts *windowsV3BatchAdmissionCounts
}

func (directory *windowsV3BatchCountingDirectory) Duplicate() (outputcap.Directory, error) {
	duplicate, err := directory.Directory.Duplicate()
	if err != nil {
		return nil, err
	}
	return directory.wrap(duplicate), nil
}

func (directory *windowsV3BatchCountingDirectory) SameDirectory(other outputcap.Directory) (bool, error) {
	if wrapped, ok := other.(*windowsV3BatchCountingDirectory); ok {
		other = wrapped.Directory
	}
	return directory.Directory.SameDirectory(other)
}

func (directory *windowsV3BatchCountingDirectory) OpenDirectory(
	name string,
	private bool,
) (outputcap.Directory, error) {
	opened, err := directory.Directory.OpenDirectory(name, private)
	if err != nil || private {
		return opened, err
	}
	return directory.wrap(opened), nil
}

func (directory *windowsV3BatchCountingDirectory) CreateDirectory(
	name string,
	private bool,
) (outputcap.Directory, error) {
	created, err := directory.Directory.CreateDirectory(name, private)
	if err != nil || private {
		return created, err
	}
	return directory.wrap(created), nil
}

func (directory *windowsV3BatchCountingDirectory) wrap(
	owned outputcap.Directory,
) outputcap.Directory {
	return &windowsV3BatchCountingDirectory{
		Directory: owned,
		counts:    directory.counts,
	}
}

func (directory *windowsV3BatchCountingDirectory) ValidatePublicEntryName(name string) error {
	directory.counts.singleCalls++
	return directory.Directory.ValidatePublicEntryName(name)
}

func (directory *windowsV3BatchCountingDirectory) ValidatePublicEntryNames(names []string) error {
	directory.counts.batchCalls++
	batch := directory.Directory.(outputcap.PublicEntryNamesValidator)
	return batch.ValidatePublicEntryNames(names)
}

type windowsV3AuthorityRejectPlatform struct {
	outputcap.Platform
	root       outputcap.Directory
	probeCalls *int
}

func (platform *windowsV3AuthorityRejectPlatform) Root() outputcap.Directory { return platform.root }

func (platform *windowsV3AuthorityRejectPlatform) AcquirePublicOperationGuard() (
	outputcap.PublicOperationGuard,
	error,
) {
	return acquireRuntimeTestDecoratedPublicOperationGuard(
		platform.Platform,
		func(root outputcap.Directory) outputcap.Directory {
			return &windowsV3AuthorityRejectDirectory{Directory: root}
		},
	)
}

func (platform *windowsV3AuthorityRejectPlatform) ProbeRecoverableFeatures() error {
	(*platform.probeCalls)++
	return platform.Platform.ProbeRecoverableFeatures()
}

type windowsV3AuthorityRejectDirectory struct{ outputcap.Directory }

func (directory *windowsV3AuthorityRejectDirectory) ValidatePublicEntryNames(names []string) error {
	batch := directory.Directory.(outputcap.PublicEntryNamesValidator)
	return batch.ValidatePublicEntryNames(names)
}

func (*windowsV3AuthorityRejectDirectory) ValidateCreateAuthority() error {
	return errors.Join(outputcap.ErrRecoverableOutputUnsupported, errors.New("injected selected-parent authority rejection"))
}

type windowsRuntimeBatchAdmissionFile struct {
	size     uint64
	modified catalog.ModifiedTime
}

func windowsRuntimeBatchAdmissionSelection(
	t *testing.T,
	files []windowsRuntimeBatchAdmissionFile,
) transfer.OutputSelection {
	t.Helper()
	share := v3RecoveryIdentity16[catalog.ShareInstance](0xb1)
	root := v3RecoveryIdentity16[catalog.DirectoryID](0xb2)
	generation := v3RecoveryIdentity16[catalog.DirectoryGeneration](0xb3)
	selected := make([]transfer.OutputSelectionFile, len(files))
	for index, file := range files {
		selected[index] = transfer.OutputSelectionFile{
			Path:              fmt.Sprintf("metadata-%03d.bin", index),
			FileID:            v3RecoveryIdentity16[catalog.FileID](byte(0xc0 + index)),
			ParentDirectoryID: root,
			ParentGeneration:  generation,
			ExpectedSize:      file.size,
			ModifiedTime:      file.modified,
		}
	}
	plan, err := transfer.NewOutputSelection(share, root, generation, nil, selected)
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

func assertWindowsRuntimeBatchAdmissionRootUntouched(t *testing.T, root string) {
	t.Helper()
	entries, err := os.ReadDir(root)
	if err != nil || len(entries) != 0 {
		t.Fatalf("static admission rejection changed output root: entries=%v error=%v", entries, err)
	}
}
