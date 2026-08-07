package transfer

import (
	"path/filepath"
	"testing"

	"github.com/windshare/windshare/core/catalog"
)

func TestOutputRootTargetSeparatesFilesystemRootsFromCatalogLocators(t *testing.T) {
	root := filepath.Join(t.TempDir(), "downloads")
	target, err := NewFilesystemOutputRootTarget(root)
	if err != nil {
		t.Fatal(err)
	}
	if target.Kind() != OutputFilesystemRootTarget || target.RootPath() != filepath.Clean(root) || target.IsZero() {
		t.Fatalf("target=%+v", target)
	}
	if _, err := NewFilesystemOutputRootTarget("relative/downloads"); err == nil {
		t.Fatal("relative filesystem root accepted")
	}
	if _, err := NewPathOutputLocator(root); err == nil {
		t.Fatal("absolute filesystem root accepted as catalog locator")
	}
	cleaned, err := NewFilesystemOutputRootTarget(filepath.Join(root, "nested", ".."))
	if err != nil || cleaned.Identity() != target.Identity() {
		t.Fatalf("cleaned=%+v err=%v; root target identity changed after lexical clean", cleaned, err)
	}

	share := transferID[catalog.ShareInstance](1)
	syntheticRoot := transferID[catalog.DirectoryID](2)
	rules, err := NewSelectionRules(true, nil)
	if err != nil {
		t.Fatal(err)
	}
	backend := NativeFilesystemOutputBackendID
	intent, err := NewTransferIntent(share, syntheticRoot, rules, target, backend, OutputNativeTree)
	if err != nil || intent.OutputTarget().Kind() != OutputFilesystemRootTarget || intent.IsZero() {
		t.Fatalf("intent=%+v err=%v", intent, err)
	}
	if len(intent.CanonicalBytes()) == 0 || intent.Digest().IsZero() {
		t.Fatalf("intent canonical identity missing: bytes=%d digest=%x", len(intent.CanonicalBytes()), intent.Digest())
	}
}

func TestOpaqueOutputTargetRequiresOpaqueIdentity(t *testing.T) {
	raw := make([]byte, OutputRootIdentityBytes)
	if _, err := NewOpaqueOutputTarget(raw); err == nil {
		t.Fatal("zero opaque output identity accepted")
	}
	raw[0] = 1
	target, err := NewOpaqueOutputTarget(raw)
	if err != nil || target.Kind() != OutputOpaqueTarget || target.RootPath() != "" || target.Identity().IsZero() {
		t.Fatalf("target=%+v err=%v", target, err)
	}
}
