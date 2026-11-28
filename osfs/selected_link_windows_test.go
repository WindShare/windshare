//go:build windows

package osfs

import (
	"bytes"
	"context"
	"errors"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/windshare/windshare/core/catalog"
	"github.com/windshare/windshare/core/content"
	diag "github.com/windshare/windshare/core/diagnostics"
	"github.com/windshare/windshare/osfs/internal/linkresolve"
	"golang.org/x/sys/windows"
)

func createTestWindowsJunction(t *testing.T, link, target string) {
	t.Helper()
	out, err := exec.Command("cmd", "/d", "/c", "mklink", "/J", link, target).CombinedOutput()
	if err != nil {
		t.Fatalf("mklink /J %q %q: %v (%s)", link, target, err, strings.TrimSpace(string(out)))
	}
	t.Cleanup(func() { _ = os.Remove(link) })
}

func TestWindowsSelectedJunctionSharesTargetAcrossPathForms(t *testing.T) {
	base := t.TempDir()
	target := filepath.Join(base, "real-target")
	nested := filepath.Join(target, "sub", "nested")
	outside := filepath.Join(base, "outside")
	if err := os.MkdirAll(nested, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(outside, 0o700); err != nil {
		t.Fatal(err)
	}
	payload := []byte("shared through windows junction")
	if err := os.WriteFile(filepath.Join(target, "a.txt"), payload, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(outside, "secret.txt"), []byte("secret"), 0o600); err != nil {
		t.Fatal(err)
	}
	// An inner junction inside the target must stay omitted and never escape.
	createTestWindowsJunction(t, filepath.Join(target, "inner-junction"), outside)

	jlink := filepath.Join(base, "my-junction")
	createTestWindowsJunction(t, jlink, target)
	jchain := filepath.Join(base, "my-chain")
	createTestWindowsJunction(t, jchain, jlink)

	subViaJunction := filepath.Join(jlink, "sub")
	nestedViaJunction := filepath.Join(jlink, "sub", "nested")
	cases := []struct {
		name        string
		cwd         string
		selection   string
		wantRoot    string
		wantDisplay string
	}{
		{"dot", jlink, ".", "my-junction", jlink},
		{"dot backslash", jlink, `.\`, "my-junction", jlink},
		{"dot slash", jlink, "./", "my-junction", jlink},
		{"parent from sub", subViaJunction, "..", "my-junction", jlink},
		{"parent backslash", subViaJunction, `..\`, "my-junction", jlink},
		{"parent slash", subViaJunction, "../", "my-junction", jlink},
		{"dot slash parent", subViaJunction, "./..", "my-junction", jlink},
		{"grandparent backslash", nestedViaJunction, `..\..`, "my-junction", jlink},
		{"grandparent slash", nestedViaJunction, "../..", "my-junction", jlink},
		{"named absolute junction", base, jlink, "my-junction", jlink},
		{"named junction backslash", base, jlink + `\`, "my-junction", jlink},
		{"named junction slash", base, jlink + "/", "my-junction", jlink},
		{"named junction dot suffix", base, jlink + `\.`, "my-junction", jlink},
		{"named relative junction", base, "my-junction", "my-junction", jlink},
		{"junction chain dot", jchain, ".", "my-chain", jchain},
		{"junction chain named", base, jchain, "my-chain", jchain},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Chdir(tc.cwd)
			stream, err := diag.NewStream(16, nil)
			if err != nil {
				t.Fatal(err)
			}
			var omissions []catalog.SourceOmissions
			source, err := openLinkedTestSource(t, []string{tc.selection}, stream.Sink(), catalog.SourceOmissionObserverFunc(func(summary catalog.SourceOmissions) {
				omissions = append(omissions, summary)
			}))
			if err != nil {
				t.Fatalf("open %q from %q failed: %v", tc.selection, tc.cwd, err)
			}
			root := source.SelectedRoots()[0]
			if _, ok := root.DirectoryID(); !ok || root.Entry().Name() != tc.wantRoot {
				t.Fatalf("root = (name=%q, dir=%v), want %q", root.Entry().Name(), ok, tc.wantRoot)
			}
			children := &collectingScanChildren{}
			result, err := source.ScanDirectory(context.Background(), catalog.ScanRequest{
				Directory: root, Work: &countingScanWork{}, Children: children,
			})
			if err != nil {
				t.Fatalf("ScanDirectory failed: %v", err)
			}
			result.PublishOmissions(0, nil)
			stream.Close()

			if result.OmittedCount != 1 || len(children.items) != 2 {
				t.Fatalf("scan = (children=%d, omitted=%d), want (2, 1)", len(children.items), result.OmittedCount)
			}
			if len(omissions) != 1 || omissions[0].DirectoryPath != tc.wantDisplay ||
				len(omissions[0].Samples) != 1 || omissions[0].Samples[0].Reason != catalog.SourceOmissionReparsePoint {
				t.Fatalf("omissions = %+v, want reparse-point omission under %q", omissions, tc.wantDisplay)
			}

			var fileChild catalog.ScannedChild
			for _, child := range children.items {
				if child.Name == "a.txt" {
					fileChild = child
				}
			}
			parentID, _ := root.DirectoryID()
			record, err := catalog.NewFileNodeRecord(
				fileChild.FileID, parentID, fileChild.Name, fileChild.SourceReference,
				fileChild.SourceIdentity, fileChild.VersionCandidate, fileChild.ExpectedSize, fileChild.ModifiedTime,
			)
			if err != nil {
				t.Fatal(err)
			}
			stable, err := source.OpenStable(context.Background(), record)
			if err != nil {
				t.Fatalf("OpenStable(a.txt) failed: %v", err)
			}
			buf := make([]byte, stable.ExactSize())
			_, readErr := stable.ReadAt(context.Background(), buf, 0)
			closeErr := stable.Close()
			if readErr != nil || closeErr != nil || !bytes.Equal(buf, payload) {
				t.Fatalf("read = (%q, %v, %v), want %q", buf, readErr, closeErr, payload)
			}

			foundDecision := false
			for rec := range stream.Records() {
				if rec.Name() == "catalog_source_policy" && rec.Payload().Get("decision").Text() == "resolved_selected_link" {
					if rec.Payload().Get("reparse_tag").Text() != "0xa0000003" {
						t.Fatalf("resolved_selected_link lost mount-point reparse_tag: %+v", rec.Payload())
					}
					foundDecision = true
				}
			}
			if !foundDecision {
				t.Fatal("missing catalog_source_policy resolved_selected_link diagnostic trace")
			}
		})
	}
}

func TestWindowsSelectedJunctionSupportsLongPathTarget(t *testing.T) {
	base := t.TempDir()
	seg := strings.Repeat("d", 60)
	longTarget := filepath.Join(base, seg, seg, seg, seg, "long-target")
	if len(longTarget) <= 260 {
		t.Fatalf("expected longTarget > 260 chars, got %d", len(longTarget))
	}
	if err := os.MkdirAll(longTarget, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(longTarget, "payload.txt"), []byte("long-ok"), 0o600); err != nil {
		t.Fatal(err)
	}
	shortLink := filepath.Join(base, "short-to-long")
	createTestWindowsJunction(t, shortLink, linkresolve.WindowsExtendedPrefix+longTarget)

	for _, selection := range []string{shortLink, "."} {
		t.Run(selection, func(t *testing.T) {
			if selection == "." {
				t.Chdir(shortLink)
			}
			source, err := openLinkedTestSource(t, []string{selection}, diag.Sink{}, nil)
			if err != nil {
				t.Fatalf("open %q through junction to >MAX_PATH target failed: %v", selection, err)
			}
			root := source.SelectedRoots()[0]
			if root.Entry().Name() != "short-to-long" {
				t.Fatalf("root name = %q, want short-to-long", root.Entry().Name())
			}
			children := &collectingScanChildren{}
			if _, err := source.ScanDirectory(context.Background(), catalog.ScanRequest{
				Directory: root, Work: &countingScanWork{}, Children: children,
			}); err != nil {
				t.Fatalf("ScanDirectory on long target failed: %v", err)
			}
			if len(children.items) != 1 || children.items[0].Name != "payload.txt" {
				t.Fatalf("children = %+v, want payload.txt", children.items)
			}
		})
	}
}

func TestWindowsResolveSelectedLinkCloudAndSafetyPolicy(t *testing.T) {
	dirID := sourceObjectIdentity{volumeSerial: 1, fileIndexHigh: 2, fileIndexLow: 3, isDir: true}
	fileID := sourceObjectIdentity{volumeSerial: 1, fileIndexHigh: 2, fileIndexLow: 4, isDir: false}
	otherID := sourceObjectIdentity{volumeSerial: 1, fileIndexHigh: 2, fileIndexLow: 9, isDir: true}
	hydratedDirAudit := sourceEntryAudit{
		attributes:       windows.FILE_ATTRIBUTE_DIRECTORY | windows.FILE_ATTRIBUTE_REPARSE_POINT,
		reparseTag:       testReparseTagCloudRoot,
		placeholderState: cfPlaceholderStatePlaceholder | cfPlaceholderStateInSync | cfPlaceholderStateSyncRoot,
		hydratedCloud:    true,
		hasCloudState:    true,
	}
	hydratedFileAudit := sourceEntryAudit{
		attributes:       windows.FILE_ATTRIBUTE_REPARSE_POINT,
		reparseTag:       testReparseTagCloudPinned,
		placeholderState: cfPlaceholderStatePlaceholder | cfPlaceholderStateInSync,
		hydratedCloud:    true,
		hasCloudState:    true,
	}
	placeholderAudit := sourceEntryAudit{
		attributes:       windows.FILE_ATTRIBUTE_DIRECTORY | windows.FILE_ATTRIBUTE_REPARSE_POINT | windowsAttrRecallOnDataAccess,
		reparseTag:       testReparseTagCloudRoot,
		placeholderState: cfPlaceholderStatePlaceholder | cfPlaceholderStatePartial,
		hasCloudState:    true,
	}

	junctionAudit := sourceEntryAudit{
		attributes: windows.FILE_ATTRIBUTE_DIRECTORY | windows.FILE_ATTRIBUTE_REPARSE_POINT,
		reparseTag: windows.IO_REPARSE_TAG_MOUNT_POINT,
	}
	symlinkAudit := sourceEntryAudit{
		attributes: windows.FILE_ATTRIBUTE_REPARSE_POINT,
		reparseTag: windows.IO_REPARSE_TAG_SYMLINK,
	}

	// 1. Hydrated cloud directory and regular file targets remain admitted.
	got, err := resolveWindowsSelectedLinkWith(
		`C:\work\link`,
		junctionAudit,
		func(string) (sourceObjectIdentity, sourceEntryKind, sourceEntryAudit, string, error) {
			return dirID, sourceEntryDirectory, hydratedDirAudit, `C:\Users\me\OneDrive\Docs`, nil
		},
		func(string) (sourceObjectIdentity, sourceEntryKind, sourceEntryAudit, error) {
			return dirID, sourceEntryDirectory, hydratedDirAudit, nil
		},
	)
	if err != nil || got.disposition != selectedLinkResolved || got.kind != sourceEntryDirectory ||
		!got.audit.hydratedCloud || got.openPath != `C:\Users\me\OneDrive\Docs` {
		t.Fatalf("hydrated cloud dir target = (%+v, %v)", got, err)
	}
	got, err = resolveWindowsSelectedLinkWith(
		`C:\work\file-link`,
		symlinkAudit,
		func(string) (sourceObjectIdentity, sourceEntryKind, sourceEntryAudit, string, error) {
			return fileID, sourceEntryRegularFile, hydratedFileAudit, `C:\Users\me\OneDrive\report.pdf`, nil
		},
		func(string) (sourceObjectIdentity, sourceEntryKind, sourceEntryAudit, error) {
			return fileID, sourceEntryRegularFile, hydratedFileAudit, nil
		},
	)
	if err != nil || got.disposition != selectedLinkResolved || got.kind != sourceEntryRegularFile ||
		!got.audit.hydratedCloud || got.openPath != `C:\Users\me\OneDrive\report.pdf` {
		t.Fatalf("hydrated cloud file target = (%+v, %v)", got, err)
	}

	// 2. Dehydrated cloud placeholder target returns explicit rejection without recall.
	got, err = resolveWindowsSelectedLinkWith(
		`C:\work\link`,
		junctionAudit,
		func(string) (sourceObjectIdentity, sourceEntryKind, sourceEntryAudit, string, error) {
			return dirID, sourceEntryCloudPlaceholder, placeholderAudit, "", nil
		},
		func(string) (sourceObjectIdentity, sourceEntryKind, sourceEntryAudit, error) {
			t.Fatal("inspect should not run for a dehydrated placeholder")
			return sourceObjectIdentity{}, 0, sourceEntryAudit{}, nil
		},
	)
	if err != nil || got.disposition != selectedLinkRejected || got.kind != sourceEntryCloudPlaceholder || got.openPath != "" {
		t.Fatalf("dehydrated cloud target = (%+v, %v)", got, err)
	}

	// 3. Target dehydrating between follow and inspect reports cloud placeholder,
	// while a swapped foreign object reports ErrDirectoryStale.
	got, err = resolveWindowsSelectedLinkWith(
		`C:\work\link`,
		junctionAudit,
		func(string) (sourceObjectIdentity, sourceEntryKind, sourceEntryAudit, string, error) {
			return dirID, sourceEntryDirectory, hydratedDirAudit, `C:\Users\me\OneDrive\Docs`, nil
		},
		func(string) (sourceObjectIdentity, sourceEntryKind, sourceEntryAudit, error) {
			return dirID, sourceEntryCloudPlaceholder, placeholderAudit, nil
		},
	)
	if err != nil || got.disposition != selectedLinkRejected || got.kind != sourceEntryCloudPlaceholder || got.openPath != "" {
		t.Fatalf("mid-resolve dehydration = (%+v, %v)", got, err)
	}
	if _, err := resolveWindowsSelectedLinkWith(
		`C:\work\link`,
		junctionAudit,
		func(string) (sourceObjectIdentity, sourceEntryKind, sourceEntryAudit, string, error) {
			return dirID, sourceEntryDirectory, hydratedDirAudit, `C:\Users\me\OneDrive\Docs`, nil
		},
		func(string) (sourceObjectIdentity, sourceEntryKind, sourceEntryAudit, error) {
			return otherID, sourceEntryCloudPlaceholder, placeholderAudit, nil
		},
	); !errors.Is(err, catalog.ErrDirectoryStale) {
		t.Fatalf("swapped foreign cloud placeholder err = %v, want ErrDirectoryStale", err)
	}

	// 4. Unknown reparse target keeps target audit, unresolvable link falls back
	// to leaf audit (for both junctions and symlinks), and special file stays
	// classified as special_file without a misleading rejected_reparse audit.
	unknownAudit := sourceEntryAudit{attributes: windows.FILE_ATTRIBUTE_REPARSE_POINT, reparseTag: testReparseTagUnknown}
	got, err = resolveWindowsSelectedLinkWith(
		`C:\work\link`,
		junctionAudit,
		func(string) (sourceObjectIdentity, sourceEntryKind, sourceEntryAudit, string, error) {
			return dirID, sourceEntryReparsePoint, unknownAudit, "", nil
		},
		func(string) (sourceObjectIdentity, sourceEntryKind, sourceEntryAudit, error) {
			return sourceObjectIdentity{}, 0, sourceEntryAudit{}, nil
		},
	)
	if err != nil || got.disposition != selectedLinkRejected || got.kind != sourceEntryReparsePoint ||
		got.audit.reparseTag != testReparseTagUnknown {
		t.Fatalf("unknown reparse target = (%+v, %v)", got, err)
	}
	for _, leaf := range []struct {
		name  string
		audit sourceEntryAudit
	}{
		{name: "unresolvable junction", audit: junctionAudit},
		{name: "unresolvable symlink", audit: symlinkAudit},
	} {
		got, err = resolveWindowsSelectedLinkWith(
			`C:\work\loop`,
			leaf.audit,
			func(string) (sourceObjectIdentity, sourceEntryKind, sourceEntryAudit, string, error) {
				return sourceObjectIdentity{}, sourceEntryReparsePoint, sourceEntryAudit{}, "", nil
			},
			func(string) (sourceObjectIdentity, sourceEntryKind, sourceEntryAudit, error) {
				t.Fatal("inspect should not run for an unresolvable link")
				return sourceObjectIdentity{}, 0, sourceEntryAudit{}, nil
			},
		)
		if err != nil || got.disposition != selectedLinkRejected || got.kind != sourceEntryReparsePoint ||
			got.audit != leaf.audit {
			t.Fatalf("%s = (%+v, %v), want leaf audit %+v", leaf.name, got, err, leaf.audit)
		}
	}
	got, err = resolveWindowsSelectedLinkWith(
		`C:\work\link`,
		junctionAudit,
		func(string) (sourceObjectIdentity, sourceEntryKind, sourceEntryAudit, string, error) {
			return sourceObjectIdentity{}, sourceEntrySpecialFile, sourceEntryAudit{}, "", nil
		},
		func(string) (sourceObjectIdentity, sourceEntryKind, sourceEntryAudit, error) {
			t.Fatal("inspect should not run for a special file target")
			return sourceObjectIdentity{}, 0, sourceEntryAudit{}, nil
		},
	)
	if err != nil || got.disposition != selectedLinkRejected || got.kind != sourceEntrySpecialFile || got.audit.present() {
		t.Fatalf("special file target = (%+v, %v), want selectedLinkRejected/special_file without reparse audit", got, err)
	}

	// 5. Identity mismatch between follow and inspect reports ErrDirectoryStale.
	if _, err := resolveWindowsSelectedLinkWith(
		`C:\work\link`,
		junctionAudit,
		func(string) (sourceObjectIdentity, sourceEntryKind, sourceEntryAudit, string, error) {
			return dirID, sourceEntryDirectory, sourceEntryAudit{}, `C:\target`, nil
		},
		func(string) (sourceObjectIdentity, sourceEntryKind, sourceEntryAudit, error) {
			return otherID, sourceEntryDirectory, sourceEntryAudit{}, nil
		},
	); !errors.Is(err, catalog.ErrDirectoryStale) {
		t.Fatalf("identity mismatch err = %v, want ErrDirectoryStale", err)
	}

	// 6. Final-path capability errors map to SourceFailureUnsupported, while
	// permission and generic I/O errors preserve filesystem classification.
	for _, capErr := range []error{
		windows.ERROR_NOT_SUPPORTED,
		windows.ERROR_INVALID_FUNCTION,
		windows.ERROR_CALL_NOT_IMPLEMENTED,
		windows.ERROR_INVALID_PARAMETER,
	} {
		classifiedErr := classifyWindowsFinalPathError(`C:\work\link`, capErr)
		failure, ok := errors.AsType[*catalog.SourceError](classifySourceError("acquire selected root", catalog.SourceReference{}, classifiedErr))
		if !errors.Is(classifiedErr, content.ErrUnsupportedStability) || !ok || failure.Failure != catalog.SourceFailureUnsupported {
			t.Fatalf("classifyWindowsFinalPathError(%v) = (%v, %+v), want SourceFailureUnsupported", capErr, classifiedErr, failure)
		}
	}
	deniedErr := classifyWindowsFinalPathError(`C:\work\link`, windows.ERROR_ACCESS_DENIED)
	deniedFailure, deniedOK := errors.AsType[*catalog.SourceError](classifySourceError("acquire selected root", catalog.SourceReference{}, deniedErr))
	if !errors.Is(deniedErr, fs.ErrPermission) || !deniedOK || deniedFailure.Failure != catalog.SourceFailureAccessDenied {
		t.Fatalf("classifyWindowsFinalPathError(ACCESS_DENIED) = (%v, %+v), want SourceFailureAccessDenied", deniedErr, deniedFailure)
	}

	// 7. Real dangling junction, mutual junction loop, and mutual symlink loop
	// emit consistent rejected_reparse traces with the leaf link's reparse_tag.
	base := t.TempDir()
	dangling := filepath.Join(base, "dangling")
	createTestWindowsJunction(t, dangling, filepath.Join(base, "missing"))
	assertSelectionFailure(t, []string{dangling}, dangling, fs.ErrNotExist, catalog.SourceFailureMissing)

	loopA := filepath.Join(base, "loopA")
	loopB := filepath.Join(base, "loopB")
	createTestWindowsJunction(t, loopA, loopB)
	createTestWindowsJunction(t, loopB, loopA)
	assertRejectedReparseLoop(t, loopA, "0xa0000003")

	symLoopA := filepath.Join(base, "symLoopA")
	symLoopB := filepath.Join(base, "symLoopB")
	if err := os.Symlink(symLoopB, symLoopA); err == nil {
		if err := os.Symlink(symLoopA, symLoopB); err != nil {
			t.Fatal(err)
		}
		assertRejectedReparseLoop(t, symLoopA, "0xa000000c")
	}
}

func assertRejectedReparseLoop(t *testing.T, path, wantTag string) {
	t.Helper()
	stream, err := diag.NewStream(16, nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := openLinkedTestSource(t, []string{path}, stream.Sink(), nil); !errors.Is(err, content.ErrUnsupportedStability) {
		t.Fatalf("looping link %q err = %v, want ErrUnsupportedStability", path, err)
	}
	stream.Close()
	foundRejected := false
	for rec := range stream.Records() {
		if rec.Name() == "catalog_source_policy" && rec.Payload().Get("decision").Text() == "rejected_reparse" &&
			rec.Payload().Get("reparse_tag").Text() == wantTag {
			foundRejected = true
		}
	}
	if !foundRejected {
		t.Fatalf("missing catalog_source_policy rejected_reparse trace with reparse_tag=%s for %q", wantTag, path)
	}
}
