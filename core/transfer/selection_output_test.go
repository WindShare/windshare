package transfer

import (
	"errors"
	"math"
	"testing"

	"github.com/windshare/windshare/core/catalog"
	"github.com/windshare/windshare/core/content"
)

func TestSelectionRulesInheritanceAndDiscoveryIndex(t *testing.T) {
	root := transferID[catalog.DirectoryID](1)
	excluded := transferID[catalog.DirectoryID](2)
	reincludedDirectory := transferID[catalog.DirectoryID](3)
	reincludedFile := transferID[catalog.FileID](4)
	excludedFile := transferID[catalog.FileID](5)
	rules, err := NewSelectionRules(true, []SelectionOverride{
		{DirectoryID: excluded, Selected: false, Ancestors: []catalog.DirectoryID{root}},
		{DirectoryID: reincludedDirectory, Selected: true, Ancestors: []catalog.DirectoryID{root, excluded}},
		{FileID: reincludedFile, Selected: true, Ancestors: []catalog.DirectoryID{root, excluded}},
		{FileID: excludedFile, Selected: false, Ancestors: []catalog.DirectoryID{root}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if !rules.DefaultSelected() || rules.DirectorySelected(excluded, true) ||
		!rules.DirectorySelected(reincludedDirectory, false) || !rules.FileSelected(reincludedFile, false) ||
		rules.FileSelected(excludedFile, true) {
		t.Fatal("nearest override did not replace inherited selection")
	}
	if !rules.ShouldDiscoverDirectory(excluded, false) || !rules.ShouldDiscoverDirectory(transferID[catalog.DirectoryID](9), false) {
		t.Fatal("a caller-provided ancestry hint became pruning authority")
	}
	if !rules.HasSelection() {
		t.Fatal("selected rule set was reported empty")
	}
	unindexed, err := NewSelectionRules(false, []SelectionOverride{{FileID: reincludedFile, Selected: true}})
	if err != nil || !unindexed.ShouldDiscoverDirectory(transferID[catalog.DirectoryID](9), false) {
		t.Fatalf("unindexed selected descendant was pruned: %v", err)
	}
	partial, err := NewSelectionRules(false, []SelectionOverride{{
		FileID: reincludedFile, Selected: true, Ancestors: []catalog.DirectoryID{root},
	}})
	if err != nil || !partial.ShouldDiscoverDirectory(transferID[catalog.DirectoryID](10), false) {
		t.Fatalf("partially indexed selected descendant was pruned: %v", err)
	}
	empty, err := NewSelectionRules(false, nil)
	if err != nil || empty.HasSelection() {
		t.Fatalf("empty rules = (%+v, %v)", empty, err)
	}
}

func TestSelectionRulesRejectAmbiguousAndDuplicateTargets(t *testing.T) {
	directory := transferID[catalog.DirectoryID](1)
	file := catalog.FileID(directory.NodeID())
	cases := [][]SelectionOverride{
		{{}},
		{{DirectoryID: directory, FileID: file}},
		{{DirectoryID: directory}, {FileID: file}},
		{{FileID: transferID[catalog.FileID](2), Selected: true, Ancestors: []catalog.DirectoryID{{}}}},
		{{FileID: transferID[catalog.FileID](2), Selected: true, Ancestors: []catalog.DirectoryID{directory, directory}}},
	}
	for index, overrides := range cases {
		if _, err := NewSelectionRules(false, overrides); !errors.Is(err, ErrInvalidSelectionRules) {
			t.Fatalf("case %d error=%v", index, err)
		}
	}
}

func TestPathSelectionRulesDiscoverOnlyAuthenticatedTargetAncestors(t *testing.T) {
	rules, err := NewPathSelectionRules([]string{"folder/file.bin"})
	if err != nil {
		t.Fatal(err)
	}
	folder := transferID[catalog.DirectoryID](10)
	file := transferID[catalog.FileID](11)
	if rules.DirectorySelectedAt(folder, "folder", false) {
		t.Fatal("path ancestor became selected instead of discovery-only")
	}
	if !rules.ShouldDiscoverDirectoryAt(folder, "folder", false) {
		t.Fatal("authenticated path ancestor was pruned")
	}
	if !rules.FileSelectedAt(file, "folder/file.bin", false) || rules.FileSelectedAt(file, "other/file.bin", false) {
		t.Fatal("path target selection did not use the authenticated cursor path")
	}
	if missing := rules.missingPathTargets(map[string]struct{}{"folder/file.bin": {}}); len(missing) != 0 {
		t.Fatalf("matched path remained missing: %v", missing)
	}
	if _, err := NewPathSelectionRules(nil); !errors.Is(err, ErrInvalidSelectionRules) {
		t.Fatalf("empty path rules error=%v", err)
	}
	if _, err := NewPathSelectionRules([]string{"folder/file.bin", "folder/file.bin"}); !errors.Is(err, ErrInvalidSelectionRules) {
		t.Fatalf("duplicate path rules error=%v", err)
	}
	if _, err := NewSelectionRules(false, make([]SelectionOverride, MaxSelectionRuleOverrides+1)); !errors.Is(err, ErrInvalidSelectionRules) {
		t.Fatalf("oversized override rules error=%v", err)
	}
	idRules, _ := NewSelectionRules(false, []SelectionOverride{{FileID: file, Selected: true}})
	pathRules, _ := NewPathSelectionRules([]string{"folder/file.bin"})
	if idRules.Mode() != SelectionByNodeID || pathRules.Mode() != SelectionByCatalogPath {
		t.Fatalf("selection modes id=%v path=%v", idRules.Mode(), pathRules.Mode())
	}
	idRules.pathTargets = []string{"folder/file.bin"}
	idRules.pathTargetSet = map[string]struct{}{"folder/file.bin": {}}
	if idRules.validSnapshot() {
		t.Fatal("mixed node-ID and catalog-path authority was accepted")
	}
}

func TestSelectionMeasureExclusiveThresholdsAndAbsorbingLarge(t *testing.T) {
	tests := []struct {
		name     string
		files    uint64
		bytes    uint64
		terminal bool
		failed   bool
		want     ConnectionSizeClass
	}{
		{name: "empty terminal", terminal: true, want: ConnectionSizeSmall},
		{name: "twenty nine", files: 29, terminal: true, want: ConnectionSizeSmall},
		{name: "thirty", files: 30, want: ConnectionSizeLarge},
		{name: "byte below", bytes: SmallTransferByteLimit - 1, terminal: true, want: ConnectionSizeSmall},
		{name: "byte exact", bytes: SmallTransferByteLimit, want: ConnectionSizeLarge},
		{name: "unfinished", files: 29, want: ConnectionSizeUnknown},
		{name: "failed", files: 1, terminal: true, failed: true, want: ConnectionSizeUnknown},
		{name: "failed after large", files: 30, terminal: true, failed: true, want: ConnectionSizeLarge},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			discovery := DiscoveryOpen
			if test.terminal && test.failed {
				discovery = DiscoveryFailed
			} else if test.terminal {
				discovery = DiscoveryComplete
			}
			snapshot := ReceiveProgressSnapshot{
				DiscoveredFiles: test.files, DiscoveredBytes: test.bytes,
				Discovery: discovery, CountersExact: true,
			}
			if got := snapshot.ConnectionSizeClass(); got != test.want {
				t.Fatalf("class=%v progress=%+v want=%v", got, snapshot, test.want)
			}
		})
	}
	tracker := newReceiveProgressTracker()
	tracker.snapshot.DiscoveredBytes = math.MaxUint64 - 1
	tracker.addDiscovery(discoveredSelection{bytes: 2, exact: true})
	if got := tracker.snapshotValue(); got.ConnectionSizeClass() != ConnectionSizeLarge ||
		got.DiscoveredBytes != math.MaxUint64 || got.CountersExact {
		t.Fatalf("overflow progress=%+v", got)
	}
}

func TestRangeAlgebraProducesCanonicalSparseResume(t *testing.T) {
	left, _ := content.NewRangeSet([]content.Range{{Offset: 0, End: 10}, {Offset: 30, End: 40}})
	right, _ := content.NewRangeSet([]content.Range{{Offset: 10, End: 20}, {Offset: 35, End: 50}})
	merged, err := MergeRanges(left, right)
	if err != nil {
		t.Fatal(err)
	}
	want, _ := content.NewRangeSet([]content.Range{{Offset: 0, End: 20}, {Offset: 30, End: 50}})
	if !rangesEqual(merged, want) {
		t.Fatalf("merged=%v want=%v", merged.Ranges(), want.Ranges())
	}
	missing, err := MissingRanges(60, merged)
	if err != nil {
		t.Fatal(err)
	}
	wantMissing, _ := content.NewRangeSet([]content.Range{{Offset: 20, End: 30}, {Offset: 50, End: 60}})
	if !rangesEqual(missing, wantMissing) || RangesCoverFile(60, merged) {
		t.Fatalf("missing=%v", missing.Ranges())
	}
	full, _ := content.NewRangeSet([]content.Range{{Offset: 0, End: 60}})
	if !RangesCoverFile(60, full) || !RangesCoverFile(0, content.RangeSet{}) {
		t.Fatal("full coverage detection failed")
	}
	outside, _ := content.NewRangeSet([]content.Range{{Offset: 0, End: 61}})
	if _, err := MissingRanges(60, outside); !errors.Is(err, ErrInvalidOutputBinding) {
		t.Fatalf("outside range error=%v", err)
	}
}

func TestMaterializationBindingsDurableRangesAndCapabilities(t *testing.T) {
	descriptor := transferDescriptor(t, 1)
	session := transferID[OutputSessionID](8)
	locator, err := NewPathMaterializationLocator("folder/file.bin")
	if err != nil {
		t.Fatal(err)
	}
	var identity OwnedObjectID
	identity[0] = 9
	binding, err := NewMaterializedFileBinding(session, descriptor, locator, identity)
	if err != nil {
		t.Fatal(err)
	}
	ranges, _ := content.NewRangeSet([]content.Range{{Offset: 0, End: 10}})
	verified, err := VerifyDurableRanges(binding, 4, ranges)
	if err != nil || verified.Binding() != binding || verified.CheckpointGeneration() != 4 || !rangesEqual(verified.Ranges(), ranges) {
		t.Fatalf("verified=%+v err=%v", verified, err)
	}
	outside, _ := content.NewRangeSet([]content.Range{{Offset: 0, End: descriptor.ExactSize() + 1}})
	if _, err := VerifyDurableRanges(binding, 0, outside); !errors.Is(err, ErrInvalidOutputBinding) {
		t.Fatalf("outside durable error=%v", err)
	}
	if _, err := NewPathMaterializationLocator("../escape"); err == nil {
		t.Fatal("escaping output locator accepted")
	}
	if _, err := NewMaterializationObjectLocator(make([]byte, 31)); err == nil {
		t.Fatal("short handle digest accepted")
	}

	capabilities, err := NewDirectTreeCapabilities(DirectTreeCapabilities{
		Durability: DurabilityPowerLoss, RandomWrite: true,
		FileFailureIsolation: true, ModifiedTime: true,
	})
	if err != nil || !capabilities.FileFailureIsolation || !capabilities.RandomWrite {
		t.Fatalf("DirectTree capabilities=%+v err=%v", capabilities, err)
	}
	if _, err := NewDirectTreeCapabilities(DirectTreeCapabilities{Durability: DurabilityPowerLoss + 1}); err == nil {
		t.Fatal("unknown durability level accepted")
	}
}

func TestOutputIdentityLocatorAndErrorValidationBranches(t *testing.T) {
	sessionBytes := make([]byte, OutputSessionIdentityBytes)
	sessionBytes[0] = 1
	session, err := OutputSessionIDFromBytes(sessionBytes)
	if err != nil || session.IsZero() || len(session.Bytes()) != OutputSessionIdentityBytes {
		t.Fatalf("session=%x err=%v", session, err)
	}
	if _, err := OutputSessionIDFromBytes(sessionBytes[:len(sessionBytes)-1]); err == nil {
		t.Fatal("short output session identity accepted")
	}
	if _, err := OutputSessionIDFromBytes(make([]byte, OutputSessionIdentityBytes)); err == nil {
		t.Fatal("zero output session identity accepted")
	}
	objectBytes := make([]byte, OwnedObjectIdentityBytes)
	objectBytes[0] = 2
	object, err := OwnedObjectIDFromBytes(objectBytes)
	if err != nil || object.IsZero() || len(object.Bytes()) != OwnedObjectIdentityBytes {
		t.Fatalf("object=%x err=%v", object, err)
	}
	if _, err := OwnedObjectIDFromBytes(objectBytes[:8]); err == nil {
		t.Fatal("short object identity accepted")
	}
	if _, err := OwnedObjectIDFromBytes(make([]byte, OwnedObjectIdentityBytes)); err == nil {
		t.Fatal("zero object identity accepted")
	}
	handleDigest := make([]byte, 32)
	handleDigest[0] = 3
	handle, err := NewMaterializationObjectLocator(handleDigest)
	if err != nil || handle.Kind() != MaterializationObjectLocator || handle.Digest() == (MaterializationLocatorDigest{}) || handle.CanonicalPath() != "" {
		t.Fatalf("handle=%+v err=%v", handle, err)
	}
	if _, err := NewMaterializationObjectLocator(make([]byte, 32)); err == nil {
		t.Fatal("zero handle digest accepted")
	}

	descriptor := transferDescriptor(t, 1)
	binding, err := NewMaterializedFileBinding(session, descriptor, handle, object)
	if err != nil || binding.ObjectIdentity() != object || binding.Locator().Kind() != MaterializationObjectLocator {
		t.Fatalf("binding=%+v err=%v", binding, err)
	}
	if _, err := NewMaterializedFileBinding(OutputSessionID{}, descriptor, handle, object); err == nil {
		t.Fatal("binding without an opened DirectTree session accepted")
	}
	if _, err := VerifyDurableRanges(MaterializedFileBinding{}, 0, content.RangeSet{}); err == nil {
		t.Fatal("durable ranges without binding accepted")
	}
	empty, err := MergeRanges()
	if err != nil || !empty.IsEmpty() {
		t.Fatalf("empty union=%v err=%v", empty.Ranges(), err)
	}
	if _, err := MissingRanges(catalog.MaxFileSize+1, content.RangeSet{}); err == nil {
		t.Fatal("oversized file resume accepted")
	}

	invalidCapabilities := []DirectTreeCapabilities{{Durability: DurabilityPowerLoss + 1}}
	for index, capabilities := range invalidCapabilities {
		if _, err := NewDirectTreeCapabilities(capabilities); err == nil {
			t.Fatalf("invalid capabilities %d accepted: %+v", index, capabilities)
		}
	}
}

func rangesEqual(left, right content.RangeSet) bool {
	leftRanges, rightRanges := left.Ranges(), right.Ranges()
	if len(leftRanges) != len(rightRanges) {
		return false
	}
	for index := range leftRanges {
		if leftRanges[index] != rightRanges[index] {
			return false
		}
	}
	return true
}
