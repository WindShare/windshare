//go:build windows

package osfs

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"github.com/windshare/windshare/core/transfer"
)

const windowsV3BatchAdmissionEntries = 64

func TestWindowsV3FirstComponentAdmissionUsesOneNativeBatch(t *testing.T) {
	rootPath := t.TempDir()
	files := make([]windowsSelectionMetadataFile, windowsV3BatchAdmissionEntries)
	for index := range files {
		files[index] = windowsSelectionMetadataFile{
			size: uint64(index), modified: v3RecoveryModifiedTime(t),
		}
		name := fmt.Sprintf("metadata-%03d.bin", index)
		if err := os.WriteFile(filepath.Join(rootPath, name), nil, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	selection := windowsSelectionMetadataSelection(t, files)
	authority := v3RecoveryAuthority(t, rootPath, nil)
	nativeFactory := authority.platformFactory
	var countedRoot *windowsV3BatchCountingDirectory
	authority.platformFactory = func(path string, create bool) (outputV3Platform, error) {
		platform, err := nativeFactory(path, create)
		if err != nil {
			return nil, err
		}
		countedRoot = &windowsV3BatchCountingDirectory{
			outputV3Directory: platform.Root(),
			counts:            &windowsV3BatchAdmissionCounts{},
		}
		return &windowsV3BatchCountingPlatform{
			outputV3Platform: platform,
			firstRoot:        countedRoot,
		}, nil
	}
	session, err := authority.OpenSelection(context.Background(), selection)
	if err != nil {
		t.Fatal(err)
	}
	concrete, ok := session.(*filesystemOutputSession)
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
	rootPath := t.TempDir()
	selection := windowsSelectionMetadataSelection(t, []windowsSelectionMetadataFile{{
		size: 1, modified: v3RecoveryModifiedTime(t),
	}})
	authority := v3RecoveryAuthority(t, rootPath, nil)
	nativeFactory := authority.platformFactory
	probeCalls := 0
	authority.platformFactory = func(path string, create bool) (outputV3Platform, error) {
		platform, err := nativeFactory(path, create)
		if err != nil {
			return nil, err
		}
		return &windowsV3AuthorityRejectPlatform{
			outputV3Platform: platform,
			root: &windowsV3AuthorityRejectDirectory{
				outputV3Directory: platform.Root(),
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
	assertWindowsV3StaticAdmissionLeftRootUntouched(t, rootPath)
}

type windowsV3BatchCountingPlatform struct {
	outputV3Platform
	firstRoot outputV3Directory
	rootCalls int
}

func (platform *windowsV3BatchCountingPlatform) Root() outputV3Directory {
	platform.rootCalls++
	if platform.rootCalls == 1 {
		return platform.firstRoot
	}
	return platform.outputV3Platform.Root()
}

func (platform *windowsV3BatchCountingPlatform) AcquirePublicOperationGuard() (
	outputV3PublicOperationGuard,
	error,
) {
	counted := platform.firstRoot.(*windowsV3BatchCountingDirectory)
	return acquireOutputV3DecoratedPublicOperationGuard(
		platform.outputV3Platform,
		func(root outputV3Directory) outputV3Directory {
			return &windowsV3BatchCountingDirectory{
				outputV3Directory: root,
				counts:            counted.counts,
			}
		},
	)
}

type windowsV3BatchAdmissionCounts struct {
	batchCalls  int
	singleCalls int
}

type windowsV3BatchCountingDirectory struct {
	outputV3Directory
	counts *windowsV3BatchAdmissionCounts
}

func (directory *windowsV3BatchCountingDirectory) Duplicate() (outputV3Directory, error) {
	duplicate, err := directory.outputV3Directory.Duplicate()
	if err != nil {
		return nil, err
	}
	return directory.wrap(duplicate), nil
}

func (directory *windowsV3BatchCountingDirectory) SameDirectory(other outputV3Directory) (bool, error) {
	if wrapped, ok := other.(*windowsV3BatchCountingDirectory); ok {
		other = wrapped.outputV3Directory
	}
	return directory.outputV3Directory.SameDirectory(other)
}

func (directory *windowsV3BatchCountingDirectory) OpenDirectory(
	name string,
	private bool,
) (outputV3Directory, error) {
	opened, err := directory.outputV3Directory.OpenDirectory(name, private)
	if err != nil || private {
		return opened, err
	}
	return directory.wrap(opened), nil
}

func (directory *windowsV3BatchCountingDirectory) CreateDirectory(
	name string,
	private bool,
) (outputV3Directory, error) {
	created, err := directory.outputV3Directory.CreateDirectory(name, private)
	if err != nil || private {
		return created, err
	}
	return directory.wrap(created), nil
}

func (directory *windowsV3BatchCountingDirectory) wrap(
	owned outputV3Directory,
) outputV3Directory {
	return &windowsV3BatchCountingDirectory{
		outputV3Directory: owned,
		counts:            directory.counts,
	}
}

func (directory *windowsV3BatchCountingDirectory) ValidatePublicEntryName(name string) error {
	directory.counts.singleCalls++
	return directory.outputV3Directory.ValidatePublicEntryName(name)
}

func (directory *windowsV3BatchCountingDirectory) ValidatePublicEntryNames(names []string) error {
	directory.counts.batchCalls++
	batch := directory.outputV3Directory.(outputV3PublicEntryNamesValidator)
	return batch.ValidatePublicEntryNames(names)
}

type windowsV3AuthorityRejectPlatform struct {
	outputV3Platform
	root       outputV3Directory
	probeCalls *int
}

func (platform *windowsV3AuthorityRejectPlatform) Root() outputV3Directory { return platform.root }

func (platform *windowsV3AuthorityRejectPlatform) AcquirePublicOperationGuard() (
	outputV3PublicOperationGuard,
	error,
) {
	return acquireOutputV3DecoratedPublicOperationGuard(
		platform.outputV3Platform,
		func(root outputV3Directory) outputV3Directory {
			return &windowsV3AuthorityRejectDirectory{outputV3Directory: root}
		},
	)
}

func (platform *windowsV3AuthorityRejectPlatform) ProbeRecoverableFeatures() error {
	(*platform.probeCalls)++
	return platform.outputV3Platform.ProbeRecoverableFeatures()
}

type windowsV3AuthorityRejectDirectory struct{ outputV3Directory }

func (directory *windowsV3AuthorityRejectDirectory) ValidatePublicEntryNames(names []string) error {
	batch := directory.outputV3Directory.(outputV3PublicEntryNamesValidator)
	return batch.ValidatePublicEntryNames(names)
}

func (*windowsV3AuthorityRejectDirectory) ValidateCreateAuthority() error {
	return errors.Join(errOutputV3Unsupported, errors.New("injected selected-parent authority rejection"))
}
