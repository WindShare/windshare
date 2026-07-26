package transfer

import (
	"errors"
	"slices"
	"testing"

	"github.com/windshare/windshare/core/catalog"
	"github.com/windshare/windshare/core/content"
)

func TestOutputAndResumeIdentityByteBoundariesOwnTheirInput(t *testing.T) {
	selectionBytes := make([]byte, SelectionIdentityBytes)
	selectionBytes[0] = 11
	selection, err := SelectionIdentityFromBytes(selectionBytes)
	if err != nil {
		t.Fatal(err)
	}
	selectionBytes[0] = 12
	selectionCopy := selection.Bytes()
	selectionCopy[0] = 13
	if selection.Bytes()[0] != 11 {
		t.Fatal("selection identity retained caller-owned byte storage")
	}
	if _, err := SelectionIdentityFromBytes(make([]byte, SelectionIdentityBytes-1)); !errors.Is(err, ErrInvalidOutputSelection) {
		t.Fatalf("short selection identity error = %v", err)
	}
	if _, err := SelectionIdentityFromBytes(make([]byte, SelectionIdentityBytes)); !errors.Is(err, ErrInvalidOutputSelection) {
		t.Fatalf("zero selection identity error = %v", err)
	}

	intentBytes := make([]byte, ResumeIntentBytes)
	intentBytes[0] = 21
	intent, err := ResumeIntentFromBytes(intentBytes)
	if err != nil {
		t.Fatal(err)
	}
	intentBytes[0] = 22
	intentCopy := intent.Bytes()
	intentCopy[0] = 23
	if intent.Bytes()[0] != 21 {
		t.Fatal("resume intent retained caller-owned byte storage")
	}
	if _, err := ResumeIntentFromBytes(make([]byte, ResumeIntentBytes-1)); !errors.Is(err, ErrInvalidOutputSelection) {
		t.Fatalf("short resume intent error = %v", err)
	}
	if _, err := ResumeIntentFromBytes(make([]byte, ResumeIntentBytes)); !errors.Is(err, ErrInvalidOutputSelection) {
		t.Fatalf("zero resume intent error = %v", err)
	}
}

func TestOutputSelectionRejectsMalformedCanonicalGraph(t *testing.T) {
	share := transferID[catalog.ShareInstance](31)
	root := transferID[catalog.DirectoryID](32)
	rootGeneration := transferID[catalog.DirectoryGeneration](33)
	directory := OutputSelectionDirectory{
		Path: "folder", DirectoryID: transferID[catalog.DirectoryID](34),
		Generation: transferID[catalog.DirectoryGeneration](35),
	}
	file := OutputSelectionFile{
		Path: "file.bin", FileID: transferID[catalog.FileID](36),
		ParentDirectoryID: root, ParentGeneration: rootGeneration, ExpectedSize: 1,
	}

	tests := []struct {
		name        string
		share       catalog.ShareInstance
		directories []OutputSelectionDirectory
		files       []OutputSelectionFile
	}{
		{name: "missing share authority", directories: []OutputSelectionDirectory{directory}},
		{name: "invalid directory path", share: share, directories: []OutputSelectionDirectory{{
			DirectoryID: directory.DirectoryID, Generation: directory.Generation,
		}}},
		{name: "duplicate directory path", share: share, directories: []OutputSelectionDirectory{directory, directory}},
		{name: "invalid file payload", share: share, files: []OutputSelectionFile{{
			Path: "file.bin", FileID: file.FileID, ParentDirectoryID: root,
			ParentGeneration: rootGeneration, ExpectedSize: catalog.MaxFileSize + 1,
		}}},
		{name: "duplicate file path", share: share, files: []OutputSelectionFile{file, file}},
		{name: "foreign root binding", share: share, files: []OutputSelectionFile{{
			Path: "file.bin", FileID: file.FileID,
			ParentDirectoryID: transferID[catalog.DirectoryID](37), ParentGeneration: rootGeneration,
		}}},
		{name: "missing nested parent", share: share, files: []OutputSelectionFile{{
			Path: "folder/file.bin", FileID: file.FileID,
			ParentDirectoryID: directory.DirectoryID, ParentGeneration: directory.Generation,
		}}},
		{name: "mismatched nested parent", share: share, directories: []OutputSelectionDirectory{directory}, files: []OutputSelectionFile{{
			Path: "folder/file.bin", FileID: file.FileID,
			ParentDirectoryID: transferID[catalog.DirectoryID](38), ParentGeneration: directory.Generation,
		}}},
		{name: "orphan directory", share: share, directories: []OutputSelectionDirectory{{
			Path: "missing/child", DirectoryID: directory.DirectoryID, Generation: directory.Generation,
		}}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			_, err := NewOutputSelection(
				test.share,
				root,
				rootGeneration,
				test.directories,
				test.files,
			)
			if !errors.Is(err, ErrInvalidOutputSelection) {
				t.Fatalf("malformed graph error = %v", err)
			}
		})
	}
}

func TestCanonicalOutputIdentityIsOrderedDomainSeparatedAndImmutable(t *testing.T) {
	share := transferID[catalog.ShareInstance](41)
	root := transferID[catalog.DirectoryID](42)
	rootGeneration := transferID[catalog.DirectoryGeneration](43)
	directories := []OutputSelectionDirectory{
		{Path: "zeta", DirectoryID: transferID[catalog.DirectoryID](44), Generation: transferID[catalog.DirectoryGeneration](45)},
		{Path: "alpha", DirectoryID: transferID[catalog.DirectoryID](46), Generation: transferID[catalog.DirectoryGeneration](47)},
	}
	files := []OutputSelectionFile{
		{Path: "z.bin", FileID: transferID[catalog.FileID](48), ParentDirectoryID: root, ParentGeneration: rootGeneration, ExpectedSize: 2},
		{Path: "a.bin", FileID: transferID[catalog.FileID](49), ParentDirectoryID: root, ParentGeneration: rootGeneration, ExpectedSize: 1},
	}
	plan, err := NewOutputSelection(share, root, rootGeneration, directories, files)
	if err != nil {
		t.Fatal(err)
	}
	reorderedPlan, err := NewOutputSelection(
		share,
		root,
		rootGeneration,
		[]OutputSelectionDirectory{directories[1], directories[0]},
		[]OutputSelectionFile{files[1], files[0]},
	)
	if err != nil || reorderedPlan.Identity() != plan.Identity() {
		t.Fatalf("reordered plan identity = %x, want %x; error = %v", reorderedPlan.Identity(), plan.Identity(), err)
	}

	directories[0].Path = "mutated"
	files[0].Path = "mutated.bin"
	returnedDirectories := plan.Directories()
	returnedFiles := plan.Files()
	if returnedDirectories[0].Path != "alpha" || returnedFiles[0].Path != "a.bin" {
		t.Fatalf("canonical order = (%v, %v)", returnedDirectories, returnedFiles)
	}
	returnedDirectories[0].Path = "caller-mutated"
	returnedFiles[0].Path = "caller-mutated.bin"
	if plan.Directories()[0].Path != "alpha" || plan.Files()[0].Path != "a.bin" {
		t.Fatal("selection accessors exposed internal slice storage")
	}
	identityBytes := plan.Identity().Bytes()
	identityBytes[0] ^= 0xff
	if slices.Equal(identityBytes, plan.Identity().Bytes()) {
		t.Fatal("selection identity accessor exposed internal byte storage")
	}

	rulesA, err := NewPathSelectionRules([]string{"z.bin", "a.bin"})
	if err != nil {
		t.Fatal(err)
	}
	rulesB, err := NewPathSelectionRules([]string{"a.bin", "z.bin"})
	if err != nil {
		t.Fatal(err)
	}
	requestA, err := NewCanonicalSelectionRequest(share, root, rulesA)
	if err != nil {
		t.Fatal(err)
	}
	requestB, err := NewCanonicalSelectionRequest(share, root, rulesB)
	if err != nil || !slices.Equal(requestA.Bytes(), requestB.Bytes()) {
		t.Fatalf("path-order normalization failed: %v", err)
	}
	requestBytes := requestA.Bytes()
	requestBytes[0] ^= 0xff
	if slices.Equal(requestBytes, requestA.Bytes()) {
		t.Fatal("canonical request exposed internal byte storage")
	}

	canonical, err := NewCanonicalSelectionV1(requestA, plan)
	if err != nil || canonical.ResumeIntent().IsZero() {
		t.Fatalf("canonical selection = %+v, error = %v", canonical, err)
	}
	canonicalBytes := canonical.Bytes()
	canonicalBytes[0] ^= 0xff
	if slices.Equal(canonicalBytes, canonical.Bytes()) {
		t.Fatal("canonical selection exposed internal byte storage")
	}
	if _, err := NewCanonicalSelectionV1(CanonicalSelectionRequest{}, plan); !errors.Is(err, ErrInvalidOutputSelection) {
		t.Fatalf("unbound canonical request error = %v", err)
	}
	invalidRules := SelectionRules{}
	if _, err := NewCanonicalSelectionRequest(share, root, invalidRules); !errors.Is(err, ErrInvalidSelectionRules) {
		t.Fatalf("invalid canonical rules error = %v", err)
	}
}

func TestOutputTargetAndBindingAccessorsPreserveCompleteAuthority(t *testing.T) {
	binding, _ := outputLifecycleFixture(t)
	target := binding.Target()
	descriptor := target.Descriptor()
	if target.BackendID() != binding.BackendID() ||
		target.OutputSessionID() != binding.OutputSessionID() ||
		target.ShareInstance() != binding.ShareInstance() ||
		target.FileID() != binding.FileID() ||
		target.FileRevision() != binding.FileRevision() ||
		target.ExactSize() != binding.ExactSize() ||
		target.Locator() != binding.Locator() ||
		binding.Descriptor() != descriptor {
		t.Fatalf("target/binding authority diverged: target=%+v binding=%+v", target, binding)
	}
	if _, err := BindOutputFileTarget(OutputFileTarget{}, binding.ObjectIdentity()); !errors.Is(err, ErrInvalidOutputBinding) {
		t.Fatalf("zero target binding error = %v", err)
	}

	short, err := content.NewRangeSet([]content.Range{{Offset: 0, End: 5}})
	if err != nil {
		t.Fatal(err)
	}
	long, err := content.NewRangeSet([]content.Range{{Offset: 0, End: 10}})
	if err != nil {
		t.Fatal(err)
	}
	merged, err := MergeRanges(short, long)
	if err != nil || !slices.Equal(merged.Ranges(), []content.Range{{Offset: 0, End: 10}}) {
		t.Fatalf("same-origin range dominance = %v, error = %v", merged.Ranges(), err)
	}
	reversed, err := MergeRanges(long, short)
	if err != nil || !slices.Equal(reversed.Ranges(), merged.Ranges()) {
		t.Fatalf("range merge depended on caller order: %v, error = %v", reversed.Ranges(), err)
	}
	idempotent, err := MergeRanges(short, short)
	if err != nil || !slices.Equal(idempotent.Ranges(), short.Ranges()) {
		t.Fatalf("duplicate range merge = %v, error = %v", idempotent.Ranges(), err)
	}
}
