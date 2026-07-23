package transfer

import (
	"errors"
	"math"
	"strings"
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
	rules, err := NewPathSelectionRules([]string{"folder/file.bin", "folder/file.bin"})
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

func TestSelectionIdentityClaimsFailClosedAtNamedBudget(t *testing.T) {
	root := transferID[catalog.DirectoryID](12)
	claims := &selectionIdentityClaims{
		seen: map[catalog.NodeID]struct{}{root.NodeID(): {}}, max: 2,
	}
	first := transferID[catalog.FileID](13).NodeID()
	if err := claims.claim(first); err != nil {
		t.Fatal(err)
	}
	if err := claims.claim(transferID[catalog.FileID](14).NodeID()); !errors.Is(err, ErrSelectionIdentityBudget) ||
		!isJobTerminalError(err) || isSessionFailure(err) {
		t.Fatalf("budget error=%v", err)
	}
	if err := claims.claim(first); !errors.Is(err, ErrCatalogIdentity) {
		t.Fatalf("duplicate error=%v", err)
	}
}

func TestSelectionMeasureExclusiveThresholdsAndAbsorbingLarge(t *testing.T) {
	tests := []struct {
		name     string
		files    uint64
		bytes    uint64
		terminal bool
		failed   bool
		want     SelectionClass
	}{
		{name: "empty terminal", terminal: true, want: SelectionSmall},
		{name: "twenty nine", files: 29, terminal: true, want: SelectionSmall},
		{name: "thirty", files: 30, want: SelectionLarge},
		{name: "byte below", bytes: SmallTransferByteLimit - 1, terminal: true, want: SelectionSmall},
		{name: "byte exact", bytes: SmallTransferByteLimit, want: SelectionLarge},
		{name: "unfinished", files: 29, want: SelectionUnknown},
		{name: "failed", files: 1, terminal: true, failed: true, want: SelectionUnknown},
		{name: "failed after large", files: 30, terminal: true, failed: true, want: SelectionLarge},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			tracker := selectionTracker{}
			for range test.files {
				tracker.addFile(0)
			}
			if test.bytes != 0 {
				tracker.addFile(test.bytes)
				tracker.mu.Lock()
				tracker.measure.DiscoveredFiles = test.files
				tracker.mu.Unlock()
			}
			if test.failed {
				tracker.failDiscovery()
			}
			if test.terminal {
				tracker.finishDiscovery()
			}
			if got := tracker.snapshot().Class(); got != test.want {
				t.Fatalf("class=%v measure=%+v want=%v", got, tracker.snapshot(), test.want)
			}
		})
	}
	tracker := selectionTracker{measure: SelectionMeasure{DiscoveredBytes: math.MaxUint64 - 1}}
	tracker.addFile(2)
	if got := tracker.snapshot(); got.Class() != SelectionLarge || got.DiscoveredBytes != math.MaxUint64 {
		t.Fatalf("overflow measure=%+v", got)
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

func TestOutputBindingsDurableRangesAndCapabilities(t *testing.T) {
	descriptor := transferDescriptor(t, 1)
	backend, _ := NewOutputBackendID("test/backend")
	session := transferID[OutputSessionID](8)
	locator, err := NewPathOutputLocator("folder/file.bin")
	if err != nil {
		t.Fatal(err)
	}
	var identity OutputObjectIdentity
	identity[0] = 9
	binding, err := NewOutputFileBinding(backend, session, descriptor, locator, identity)
	if err != nil {
		t.Fatal(err)
	}
	ranges, _ := content.NewRangeSet([]content.Range{{Offset: 0, End: 10}})
	verified, err := VerifyDurableRanges(binding, 4, ranges)
	if err != nil || verified.Binding() != binding || verified.Generation() != 4 || !rangesEqual(verified.Ranges(), ranges) {
		t.Fatalf("verified=%+v err=%v", verified, err)
	}
	outside, _ := content.NewRangeSet([]content.Range{{Offset: 0, End: descriptor.ExactSize() + 1}})
	if _, err := VerifyDurableRanges(binding, 0, outside); !errors.Is(err, ErrInvalidOutputBinding) {
		t.Fatalf("outside durable error=%v", err)
	}
	if _, err := NewPathOutputLocator("../escape"); err == nil {
		t.Fatal("escaping output locator accepted")
	}
	if _, err := NewPersistentHandleOutputLocator(make([]byte, 31)); err == nil {
		t.Fatal("short handle digest accepted")
	}

	zip, err := NewOutputCapabilities(OutputCapabilities{
		Durability: DurabilityNone, Mode: OutputZIPStream, ArchiveBoundary: ArchiveFailureAtMemberStart,
	})
	if err != nil || zip.FileFailureIsolation || zip.RandomWrite {
		t.Fatalf("zip capabilities=%+v err=%v", zip, err)
	}
	if _, err := NewOutputCapabilities(OutputCapabilities{
		Mode: OutputZIPStream, FileFailureIsolation: true, ArchiveBoundary: ArchiveFailureAtMemberStart,
	}); err == nil {
		t.Fatal("ZIP falsely claimed file isolation")
	}
	if _, err := NewOutputCapabilities(OutputCapabilities{Mode: OutputNativeTree, ArchiveBoundary: ArchiveFailureAtMemberStart}); err == nil {
		t.Fatal("native tree accepted ZIP member boundary")
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
	objectBytes := make([]byte, OutputObjectIdentityBytes)
	objectBytes[0] = 2
	object, err := OutputObjectIdentityFromBytes(objectBytes)
	if err != nil || object.IsZero() || len(object.Bytes()) != OutputObjectIdentityBytes {
		t.Fatalf("object=%x err=%v", object, err)
	}
	if _, err := OutputObjectIdentityFromBytes(objectBytes[:8]); err == nil {
		t.Fatal("short object identity accepted")
	}
	if _, err := OutputObjectIdentityFromBytes(make([]byte, OutputObjectIdentityBytes)); err == nil {
		t.Fatal("zero object identity accepted")
	}
	for _, backend := range []string{"", " leading", "trailing ", strings.Repeat("x", MaxOutputBackendIDBytes+1)} {
		if _, err := NewOutputBackendID(backend); err == nil {
			t.Fatalf("invalid backend %q accepted", backend)
		}
	}
	handleDigest := make([]byte, 32)
	handleDigest[0] = 3
	handle, err := NewPersistentHandleOutputLocator(handleDigest)
	if err != nil || handle.Kind() != OutputPersistentHandleLocator || handle.Digest() == (OutputLocatorDigest{}) || handle.CanonicalPath() != "" {
		t.Fatalf("handle=%+v err=%v", handle, err)
	}
	if _, err := NewPersistentHandleOutputLocator(make([]byte, 32)); err == nil {
		t.Fatal("zero handle digest accepted")
	}

	descriptor := transferDescriptor(t, 1)
	backend, _ := NewOutputBackendID("test/backend")
	binding, err := NewOutputFileBinding(backend, session, descriptor, handle, object)
	if err != nil || binding.ObjectIdentity() != object || binding.Locator().Kind() != OutputPersistentHandleLocator {
		t.Fatalf("binding=%+v err=%v", binding, err)
	}
	if _, err := NewOutputFileBinding("", session, descriptor, handle, object); err == nil {
		t.Fatal("binding without backend accepted")
	}
	if _, err := VerifyDurableRanges(OutputFileBinding{}, 0, content.RangeSet{}); err == nil {
		t.Fatal("durable ranges without binding accepted")
	}
	empty, err := MergeRanges()
	if err != nil || !empty.IsEmpty() {
		t.Fatalf("empty union=%v err=%v", empty.Ranges(), err)
	}
	if _, err := MissingRanges(catalog.MaxFileSize+1, content.RangeSet{}); err == nil {
		t.Fatal("oversized file resume accepted")
	}

	invalidCapabilities := []OutputCapabilities{
		{Durability: DurabilityPowerLoss + 1, Mode: OutputNativeTree},
		{Mode: 0},
		{Mode: OutputSingleFileStream, RandomWrite: true},
		{Mode: OutputSingleFileStream, FileFailureIsolation: true},
		{Durability: DurabilityProcessRestart, Mode: OutputSingleFileStream},
		{Mode: OutputZIPStream, ArchiveBoundary: ArchiveFailureNotApplicable},
	}
	for index, capabilities := range invalidCapabilities {
		if _, err := NewOutputCapabilities(capabilities); err == nil {
			t.Fatalf("invalid capabilities %d accepted: %+v", index, capabilities)
		}
	}
	nilCause := NewOutputSessionError(nil, false)
	if !errors.Is(nilCause, errors.Unwrap(nilCause)) || errors.Unwrap(nilCause) == nil {
		t.Fatal("output error did not retain its synthesized cause")
	}
	var outputError *OutputSessionError
	if !errors.As(nilCause, &outputError) || outputError.RequiresJobAbort() {
		t.Fatal("nonfatal output error classification failed")
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
