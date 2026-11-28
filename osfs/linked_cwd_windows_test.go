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

func TestWindowsDotTraversalThroughJunctionSharesTarget(t *testing.T) {
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
		{"junction chain dot", jchain, ".", "my-chain", jchain},
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
				if rec.Name() == "catalog_source_policy" && rec.Payload().Get("decision").Text() == "resolved_linked_cwd" {
					if rec.Payload().Get("reparse_tag").Text() != "0xa0000003" {
						t.Fatalf("resolved_linked_cwd lost mount-point reparse_tag: %+v", rec.Payload())
					}
					foundDecision = true
				}
			}
			if !foundDecision {
				t.Fatal("missing catalog_source_policy resolved_linked_cwd diagnostic trace")
			}
		})
	}
}

func TestWindowsNamedJunctionSelectionsStayRefusedIncludingDotSuffixes(t *testing.T) {
	base := t.TempDir()
	target := filepath.Join(base, "target")
	if err := os.MkdirAll(filepath.Join(target, "inner"), 0o700); err != nil {
		t.Fatal(err)
	}
	jlink := filepath.Join(base, "my-junction")
	createTestWindowsJunction(t, jlink, target)

	t.Chdir(jlink)
	for _, selection := range []string{
		jlink,
		jlink + `\`,
		jlink + "/",
		jlink + `\.`,
		jlink + `\.\`,
		jlink + "/.",
		jlink + "/./",
		`inner\..`,
		"inner/..",
	} {
		t.Run(selection, func(t *testing.T) {
			assertUnsupportedSelection(t, []string{selection}, selection)
		})
	}
}

func TestWindowsLinkedWorkingDirectorySupportsLongPathTarget(t *testing.T) {
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

	t.Chdir(shortLink)
	source, err := openLinkedTestSource(t, []string{"."}, diag.Sink{}, nil)
	if err != nil {
		t.Fatalf("open '.' through junction to >MAX_PATH target failed: %v", err)
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
}

func TestWindowsResolveLinkedDirectoryCloudAndSafetyPolicy(t *testing.T) {
	dirID := sourceObjectIdentity{volumeSerial: 1, fileIndexHigh: 2, fileIndexLow: 3, isDir: true}
	otherID := sourceObjectIdentity{volumeSerial: 1, fileIndexHigh: 2, fileIndexLow: 9, isDir: true}
	hydratedAudit := sourceEntryAudit{
		attributes:       windows.FILE_ATTRIBUTE_DIRECTORY | windows.FILE_ATTRIBUTE_REPARSE_POINT,
		reparseTag:       testReparseTagCloudRoot,
		placeholderState: cfPlaceholderStatePlaceholder | cfPlaceholderStateInSync | cfPlaceholderStateSyncRoot,
		hydratedCloud:    true,
		hasCloudState:    true,
	}
	placeholderAudit := sourceEntryAudit{
		attributes:       windows.FILE_ATTRIBUTE_DIRECTORY | windows.FILE_ATTRIBUTE_REPARSE_POINT | windowsAttrRecallOnDataAccess,
		reparseTag:       testReparseTagCloudRoot,
		placeholderState: cfPlaceholderStatePlaceholder | cfPlaceholderStatePartial,
		hasCloudState:    true,
	}

	// 1. Hydrated cloud directory target remains admitted.
	got, err := resolveWindowsLinkedDirectoryWith(
		`C:\work\link`,
		func(string) (sourceObjectIdentity, sourceEntryKind, sourceEntryAudit, string, error) {
			return dirID, sourceEntryDirectory, hydratedAudit, `C:\Users\me\OneDrive\Docs`, nil
		},
		func(string) (sourceObjectIdentity, sourceEntryKind, sourceEntryAudit, error) {
			return dirID, sourceEntryDirectory, hydratedAudit, nil
		},
	)
	if err != nil || got.disposition != linkedDirectoryResolved || got.kind != sourceEntryDirectory ||
		!got.audit.hydratedCloud || got.openPath != `C:\Users\me\OneDrive\Docs` {
		t.Fatalf("hydrated cloud target = (%+v, %v)", got, err)
	}

	// 2. Dehydrated cloud placeholder target returns explicit rejection without recall.
	got, err = resolveWindowsLinkedDirectoryWith(
		`C:\work\link`,
		func(string) (sourceObjectIdentity, sourceEntryKind, sourceEntryAudit, string, error) {
			return dirID, sourceEntryCloudPlaceholder, placeholderAudit, "", nil
		},
		func(string) (sourceObjectIdentity, sourceEntryKind, sourceEntryAudit, error) {
			t.Fatal("inspect should not run for a dehydrated placeholder")
			return sourceObjectIdentity{}, 0, sourceEntryAudit{}, nil
		},
	)
	if err != nil || got.disposition != linkedDirectoryRejected || got.kind != sourceEntryCloudPlaceholder || got.openPath != "" {
		t.Fatalf("dehydrated cloud target = (%+v, %v)", got, err)
	}

	// 3. Target dehydrating between follow and inspect reports cloud placeholder,
	// while a swapped foreign cloud placeholder fails closed with mismatch.
	got, err = resolveWindowsLinkedDirectoryWith(
		`C:\work\link`,
		func(string) (sourceObjectIdentity, sourceEntryKind, sourceEntryAudit, string, error) {
			return dirID, sourceEntryDirectory, hydratedAudit, `C:\Users\me\OneDrive\Docs`, nil
		},
		func(string) (sourceObjectIdentity, sourceEntryKind, sourceEntryAudit, error) {
			return dirID, sourceEntryCloudPlaceholder, placeholderAudit, nil
		},
	)
	if err != nil || got.disposition != linkedDirectoryRejected || got.kind != sourceEntryCloudPlaceholder || got.openPath != "" {
		t.Fatalf("mid-resolve dehydration = (%+v, %v)", got, err)
	}
	if _, err := resolveWindowsLinkedDirectoryWith(
		`C:\work\link`,
		func(string) (sourceObjectIdentity, sourceEntryKind, sourceEntryAudit, string, error) {
			return dirID, sourceEntryDirectory, hydratedAudit, `C:\Users\me\OneDrive\Docs`, nil
		},
		func(string) (sourceObjectIdentity, sourceEntryKind, sourceEntryAudit, error) {
			return otherID, sourceEntryCloudPlaceholder, placeholderAudit, nil
		},
	); !errors.Is(err, content.ErrUnsupportedStability) {
		t.Fatalf("swapped foreign cloud placeholder err = %v, want ErrUnsupportedStability", err)
	}

	// 4. Unknown reparse target stays rejected as reparse point.
	unknownAudit := sourceEntryAudit{attributes: windows.FILE_ATTRIBUTE_REPARSE_POINT, reparseTag: testReparseTagUnknown}
	got, err = resolveWindowsLinkedDirectoryWith(
		`C:\work\link`,
		func(string) (sourceObjectIdentity, sourceEntryKind, sourceEntryAudit, string, error) {
			return dirID, sourceEntryReparsePoint, unknownAudit, "", nil
		},
		func(string) (sourceObjectIdentity, sourceEntryKind, sourceEntryAudit, error) {
			return sourceObjectIdentity{}, 0, sourceEntryAudit{}, nil
		},
	)
	if err != nil || got.disposition != linkedDirectoryRejected || got.kind != sourceEntryReparsePoint {
		t.Fatalf("unknown reparse target = (%+v, %v)", got, err)
	}

	// 5. Regular file target and identity mismatch fail closed.
	if _, err := resolveWindowsLinkedDirectoryWith(
		`C:\work\link`,
		func(string) (sourceObjectIdentity, sourceEntryKind, sourceEntryAudit, string, error) {
			return sourceObjectIdentity{volumeSerial: 1, fileIndexLow: 4}, sourceEntryRegularFile, sourceEntryAudit{}, "", nil
		},
		func(string) (sourceObjectIdentity, sourceEntryKind, sourceEntryAudit, error) {
			return sourceObjectIdentity{}, 0, sourceEntryAudit{}, nil
		},
	); !errors.Is(err, content.ErrUnsupportedStability) {
		t.Fatalf("regular file target err = %v, want ErrUnsupportedStability", err)
	}
	if _, err := resolveWindowsLinkedDirectoryWith(
		`C:\work\link`,
		func(string) (sourceObjectIdentity, sourceEntryKind, sourceEntryAudit, string, error) {
			return dirID, sourceEntryDirectory, sourceEntryAudit{}, `C:\target`, nil
		},
		func(string) (sourceObjectIdentity, sourceEntryKind, sourceEntryAudit, error) {
			return otherID, sourceEntryDirectory, sourceEntryAudit{}, nil
		},
	); !errors.Is(err, content.ErrUnsupportedStability) {
		t.Fatalf("identity mismatch err = %v, want ErrUnsupportedStability", err)
	}

	// 6. Real dangling junction and mutual junction loop (including rejected_reparse audit).
	base := t.TempDir()
	dangling := filepath.Join(base, "dangling")
	createTestWindowsJunction(t, dangling, filepath.Join(base, "missing"))
	if _, err := resolveLinkedWorkingDirectory(".", dangling); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("dangling junction err = %v, want fs.ErrNotExist", err)
	}
	loopA := filepath.Join(base, "loopA")
	loopB := filepath.Join(base, "loopB")
	createTestWindowsJunction(t, loopA, loopB)
	createTestWindowsJunction(t, loopB, loopA)
	if _, err := resolveLinkedWorkingDirectory(".", loopA); !errors.Is(err, content.ErrUnsupportedStability) {
		t.Fatalf("junction loop err = %v, want ErrUnsupportedStability", err)
	}

	// Verify openSelectedRoot preserves rejected_reparse policy audit when a
	// dot traversal lands on a Windows junction whose resolution fails closed.
	realDir := filepath.Join(base, "real-dir")
	realSub := filepath.Join(realDir, "sub")
	if err := os.MkdirAll(realSub, 0o700); err != nil {
		t.Fatal(err)
	}
	retargeted := filepath.Join(base, "retargeted")
	loopPartner := filepath.Join(base, "retargeted-loop")
	createTestWindowsJunction(t, retargeted, realDir)
	t.Chdir(filepath.Join(retargeted, "sub"))
	if err := os.Remove(retargeted); err != nil {
		t.Fatal(err)
	}
	createTestWindowsJunction(t, retargeted, loopPartner)
	createTestWindowsJunction(t, loopPartner, retargeted)
	stream, err := diag.NewStream(16, nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := openLinkedTestSource(t, []string{".."}, stream.Sink(), nil); !errors.Is(err, content.ErrUnsupportedStability) {
		t.Fatalf("dot traversal to looping junction err = %v, want ErrUnsupportedStability", err)
	}
	stream.Close()
	foundRejected := false
	for rec := range stream.Records() {
		if rec.Name() == "catalog_source_policy" && rec.Payload().Get("decision").Text() == "rejected_reparse" &&
			rec.Payload().Get("reparse_tag").Text() == "0xa0000003" {
			foundRejected = true
		}
	}
	if !foundRejected {
		t.Fatal("missing catalog_source_policy rejected_reparse diagnostic trace for dot-traversal junction loop")
	}
}

func TestWindowsParentTraversalFromRealParentThroughNestedJunctionKeepsLogicalParent(t *testing.T) {
	base := t.TempDir()
	realParent := filepath.Join(base, "real-parent")
	elsewhere := filepath.Join(base, "elsewhere", "child")
	if err := os.MkdirAll(realParent, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(elsewhere, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(realParent, "logical.txt"), []byte("logical"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(base, "elsewhere", "physical.txt"), []byte("physical"), 0o600); err != nil {
		t.Fatal(err)
	}
	jump := filepath.Join(realParent, "jump")
	createTestWindowsJunction(t, jump, elsewhere)

	// Win32 resolves ".." lexically against GetCurrentDirectoryW, so real-parent
	// matches the directory Windows opens from real-parent\jump.
	t.Chdir(jump)
	source, err := openLinkedTestSource(t, []string{".."}, diag.Sink{}, nil)
	if err != nil {
		t.Fatalf("sharing .. from realParent\\jump failed: %v", err)
	}
	root := source.SelectedRoots()[0]
	if root.Entry().Name() != "real-parent" {
		t.Fatalf("root name = %q, want real-parent", root.Entry().Name())
	}
	children := &collectingScanChildren{}
	if _, err := source.ScanDirectory(context.Background(), catalog.ScanRequest{
		Directory: root, Work: &countingScanWork{}, Children: children,
	}); err != nil {
		t.Fatal(err)
	}
	if len(children.items) != 1 || children.items[0].Name != "logical.txt" {
		t.Fatalf("children = %+v, want only logical.txt from logical parent", children.items)
	}
}
