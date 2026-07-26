//go:build windows

package osfs

import (
	"context"
	"errors"
	"fmt"
	"os"
	"testing"

	"github.com/windshare/windshare/core/catalog"
	"github.com/windshare/windshare/core/transfer"
)

func TestWindowsSelectionMetadataValidatesExactNativeValuesAndCleansProbe(t *testing.T) {
	platform := openWindowsV3TestPlatform(t)
	defer platform.Close()
	root := windowsV3MetadataTestRoot(t, platform)
	if err := root.probeRecoverableFeatures(); err != nil {
		t.Fatal(err)
	}
	modified := windowsSelectionMetadataModified(t, 1_700_000_001, 123_456_700, catalog.TimePrecisionNanoseconds)
	selection := windowsSelectionMetadataSelection(t, []windowsSelectionMetadataFile{
		{size: 4096, modified: modified},
		{size: 8192, modified: v3RecoveryModifiedTime(t)},
		{size: 4096, modified: modified},
	})
	if err := root.validateSelectionMetadata(selection); err != nil {
		t.Fatal(err)
	}
	if names, err := root.names(0); err != nil || len(names) != 0 {
		t.Fatalf("metadata admission left output-root entries %v: %v", names, err)
	}
}

func TestWindowsSelectionMetadataRejectsUnrepresentablePrecisionWithoutStateOrContent(t *testing.T) {
	rootPath := t.TempDir()
	selection := windowsSelectionMetadataSelection(t, []windowsSelectionMetadataFile{
		{size: 1, modified: windowsSelectionMetadataModified(t, 1_700_000_000, 0, catalog.TimePrecisionNanoseconds)},
		// This invalid middle value proves planning validates every claim instead
		// of checking only the min/max witnesses selected for native I/O.
		{size: 2, modified: windowsSelectionMetadataModified(t, 1_700_000_001, 1, catalog.TimePrecisionNanoseconds)},
		{size: 3, modified: windowsSelectionMetadataModified(t, 1_700_000_002, 999_999_900, catalog.TimePrecisionNanoseconds)},
	})
	authority := v3RecoveryAuthority(t, rootPath, nil)
	probeCalls := 0
	nativeFactory := authority.platformFactory
	authority.platformFactory = func(path string, create bool) (outputV3Platform, error) {
		platform, err := nativeFactory(path, create)
		if err != nil {
			return nil, err
		}
		return &windowsV3ReservedProbeCountingPlatform{outputV3Platform: platform, calls: &probeCalls}, nil
	}
	session, err := authority.OpenSelection(context.Background(), selection)
	if session != nil {
		_, _ = session.PauseJob(context.Background(), transfer.JobPauseOutputFailure)
		t.Fatal("unrepresentable NTFS timestamp returned a session")
	}
	if !errors.Is(err, errOutputV3Unsupported) {
		t.Fatalf("unrepresentable NTFS timestamp error = %v, want native unsupported cause", err)
	}
	requireWindowsV3FreshSelectionFault(t, err)
	if probeCalls != 0 {
		t.Fatalf("unrepresentable static timestamp reached recoverability probe %d times", probeCalls)
	}
	assertWindowsV3StaticAdmissionLeftRootUntouched(t, rootPath)
}

func TestWindowsSelectionMetadataMaximumPlanHasBoundedNativeCallCount(t *testing.T) {
	platform := openWindowsV3TestPlatform(t)
	defer platform.Close()
	root := windowsV3MetadataTestRoot(t, platform)
	if err := root.probeRecoverableFeatures(); err != nil {
		t.Fatal(err)
	}
	files := []windowsSelectionMetadataFile{
		{size: 1, modified: windowsSelectionMetadataModified(t, 1_600_000_000, 0, catalog.TimePrecisionSeconds)},
		{size: 8192, modified: windowsSelectionMetadataModified(t, 1_700_000_000, 0, catalog.TimePrecisionSeconds)},
		{size: 2, modified: windowsSelectionMetadataModified(t, 1_800_000_000, 0, catalog.TimePrecisionSeconds)},
		{size: 3, modified: windowsSelectionMetadataModified(t, 1_600_000_000, 123_000_000, catalog.TimePrecisionMilliseconds)},
		{size: 4, modified: windowsSelectionMetadataModified(t, 1_700_000_000, 456_000_000, catalog.TimePrecisionMilliseconds)},
		{size: 5, modified: windowsSelectionMetadataModified(t, 1_800_000_000, 789_000_000, catalog.TimePrecisionMilliseconds)},
		{size: 6, modified: windowsSelectionMetadataModified(t, 1_600_000_000, 100, catalog.TimePrecisionNanoseconds)},
		{size: 7, modified: windowsSelectionMetadataModified(t, 1_700_000_000, 500_000_000, catalog.TimePrecisionNanoseconds)},
		{size: 8, modified: windowsSelectionMetadataModified(t, 1_800_000_000, 999_999_900, catalog.TimePrecisionNanoseconds)},
	}
	executor := &countingWindowsV3MetadataProbeExecutor{}
	if err := root.validateSelectionMetadataWithExecutor(
		windowsSelectionMetadataSelection(t, files), executor,
	); err != nil {
		t.Fatal(err)
	}
	if len(executor.sizes) != 1 || executor.sizes[0] != 8192 {
		t.Fatalf("size probe calls = %v, want one maximum-size call", executor.sizes)
	}
	if len(executor.times) != 6 {
		t.Fatalf("time probe calls = %d, want at most two for each of three precisions", len(executor.times))
	}
	var precisionCalls [3]int
	for index, witness := range executor.times {
		precisionIndex, err := windowsV3MetadataPrecisionIndex(witness.modified.Precision())
		if err != nil {
			t.Fatal(err)
		}
		precisionCalls[precisionIndex]++
		if executor.expectedSizes[index] != 8192 {
			t.Fatalf("time probe %d expected size = %d, want maximum 8192", index, executor.expectedSizes[index])
		}
	}
	if precisionCalls != [3]int{2, 2, 2} {
		t.Fatalf("time probe precision calls = %v, want [2 2 2]", precisionCalls)
	}
	if names, err := root.names(0); err != nil || len(names) != 0 {
		t.Fatalf("bounded metadata probe left output-root entries %v: %v", names, err)
	}
}

func TestWindowsSelectionMetadataExecutorFailureUsesVerifiedCleanup(t *testing.T) {
	platform := openWindowsV3TestPlatform(t)
	defer platform.Close()
	root := windowsV3MetadataTestRoot(t, platform)
	if err := root.probeRecoverableFeatures(); err != nil {
		t.Fatal(err)
	}
	selection := windowsSelectionMetadataSelection(t, []windowsSelectionMetadataFile{
		{size: 4096, modified: v3RecoveryModifiedTime(t)},
	})
	executor := &failingWindowsV3MetadataProbeExecutor{}
	if err := root.validateSelectionMetadataWithExecutor(selection, executor); !errors.Is(err, errWindowsMetadataProbeInjected) {
		t.Fatalf("injected metadata probe error = %v", err)
	}
	if names, err := root.names(0); err != nil || len(names) != 0 {
		t.Fatalf("failed metadata probe left output-root entries %v: %v", names, err)
	}
}

func TestWindowsSelectionMetadataCatalogMaximumIsActuallyProbedAndAlwaysCleaned(t *testing.T) {
	platform := openWindowsV3TestPlatform(t)
	defer platform.Close()
	root := windowsV3MetadataTestRoot(t, platform)
	if err := root.probeRecoverableFeatures(); err != nil {
		t.Fatal(err)
	}
	selection := windowsSelectionMetadataSelection(t, []windowsSelectionMetadataFile{
		{size: catalog.MaxFileSize, modified: v3RecoveryModifiedTime(t)},
	})
	err := root.validateSelectionMetadata(selection)
	if err != nil && !errors.Is(err, errWindowsV3OutputUnsupported) {
		t.Fatalf("catalog maximum metadata probe error = %v", err)
	}
	if names, namesErr := root.names(0); namesErr != nil || len(names) != 0 {
		t.Fatalf("maximum-size metadata probe left output-root entries %v: %v (probe error %v)", names, namesErr, err)
	}
}

func windowsV3MetadataTestRoot(t *testing.T, platform *windowsV3OutputPlatform) *windowsV3Directory {
	t.Helper()
	guard, err := platform.acquirePublicOperationGuard()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := guard.Close(); err != nil {
			t.Error(err)
		}
	})
	root := guard.Root()
	if root == nil {
		t.Fatal("metadata test guard has no root authority")
	}
	return root
}

func TestWindowsSelectionMetadataMaximumRejectionPrecedesStateAndContent(t *testing.T) {
	rootPath := t.TempDir()
	selection := windowsSelectionMetadataSelection(t, []windowsSelectionMetadataFile{
		{size: catalog.MaxFileSize, modified: v3RecoveryModifiedTime(t)},
	})
	authority := v3RecoveryAuthority(t, rootPath, nil)
	nativeFactory := authority.platformFactory
	probeCalls := 0
	authority.platformFactory = func(path string, create bool) (outputV3Platform, error) {
		platform, err := nativeFactory(path, create)
		if err != nil {
			return nil, err
		}
		return &windowsMaximumMetadataRejectPlatform{outputV3Platform: platform, probeCalls: &probeCalls}, nil
	}
	session, err := authority.OpenSelection(context.Background(), selection)
	if session != nil {
		_, _ = session.PauseJob(context.Background(), transfer.JobPauseOutputFailure)
		t.Fatal("native maximum-size rejection returned a session")
	}
	if !errors.Is(err, errOutputV3Unsupported) {
		t.Fatalf("native maximum-size rejection error = %v, want native unsupported cause", err)
	}
	requireWindowsV3FreshSelectionFault(t, err)
	if probeCalls != 1 {
		t.Fatalf("native maximum-size rejection recoverability probe calls = %d, want 1", probeCalls)
	}
	assertWindowsMetadataAdmissionLeftNoStateOrContent(t, rootPath, selection.Files()[0].Path)
}

type windowsMaximumMetadataRejectPlatform struct {
	outputV3Platform
	probeCalls *int
}

func (platform *windowsMaximumMetadataRejectPlatform) ProbeRecoverableFeatures() error {
	(*platform.probeCalls)++
	return platform.outputV3Platform.ProbeRecoverableFeatures()
}

type countingWindowsV3MetadataProbeExecutor struct {
	sizes         []uint64
	times         []windowsV3MetadataTimeWitness
	expectedSizes []uint64
}

func (executor *countingWindowsV3MetadataProbeExecutor) ProbeSize(_ *windowsV3File, size uint64) error {
	executor.sizes = append(executor.sizes, size)
	return nil
}

func (executor *countingWindowsV3MetadataProbeExecutor) ProbeModifiedTime(
	_ *windowsV3File,
	expectedSize uint64,
	witness windowsV3MetadataTimeWitness,
) error {
	executor.expectedSizes = append(executor.expectedSizes, expectedSize)
	executor.times = append(executor.times, witness)
	return nil
}

var errWindowsMetadataProbeInjected = errors.New("injected Windows metadata probe failure")

type failingWindowsV3MetadataProbeExecutor struct{}

func (*failingWindowsV3MetadataProbeExecutor) ProbeSize(_ *windowsV3File, _ uint64) error {
	return errWindowsMetadataProbeInjected
}

func (*failingWindowsV3MetadataProbeExecutor) ProbeModifiedTime(
	_ *windowsV3File,
	_ uint64,
	_ windowsV3MetadataTimeWitness,
) error {
	return errWindowsMetadataProbeInjected
}

func (platform *windowsMaximumMetadataRejectPlatform) ValidateSelectionMetadata(
	selection transfer.OutputSelection,
) error {
	for _, file := range selection.Files() {
		if file.ExpectedSize == catalog.MaxFileSize {
			return errors.Join(errOutputV3Unsupported, errors.New("injected native maximum-size rejection"))
		}
	}
	return platform.outputV3Platform.ValidateSelectionMetadata(selection)
}

type windowsSelectionMetadataFile struct {
	size     uint64
	modified catalog.ModifiedTime
}

func windowsSelectionMetadataSelection(
	t *testing.T,
	files []windowsSelectionMetadataFile,
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
			ParentDirectoryID: root, ParentGeneration: generation,
			ExpectedSize: file.size, ModifiedTime: file.modified,
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

func windowsSelectionMetadataModified(
	t *testing.T,
	seconds int64,
	nanoseconds uint32,
	precision catalog.TimePrecision,
) catalog.ModifiedTime {
	t.Helper()
	modified, err := catalog.NewModifiedTime(seconds, nanoseconds, precision)
	if err != nil {
		t.Fatal(err)
	}
	return modified
}

func assertWindowsMetadataAdmissionLeftNoStateOrContent(t *testing.T, rootPath, final string) {
	t.Helper()
	entries, err := os.ReadDir(rootPath)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 0 {
		t.Fatalf("metadata rejection left output-root entries %v", entries)
	}
	if _, err := os.Lstat(rootPath + string(os.PathSeparator) + final); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("metadata rejection created final content path: %v", err)
	}
}
