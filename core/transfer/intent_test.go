package transfer

import (
	"bytes"
	"errors"
	"path/filepath"
	"testing"

	"github.com/windshare/windshare/core/catalog"
)

func TestTransferIntentDraftFreezesTargetAndExcludesRunIdentity(t *testing.T) {
	share := transferID[catalog.ShareInstance](21)
	root := transferID[catalog.DirectoryID](22)
	file := transferID[catalog.FileID](23)
	rules, err := NewSelectionRules(false, []SelectionOverride{{FileID: file, Selected: true}})
	if err != nil {
		t.Fatal(err)
	}
	draft, err := NewTransferIntentDraft(share, root, rules)
	if err != nil {
		t.Fatal(err)
	}
	backend, _ := NewOutputBackendID("windshare/test")
	if _, err := draft.Freeze(backend, OutputNativeTree); !errors.Is(err, ErrTransferIntentOutputUnset) {
		t.Fatalf("unconfirmed draft freeze error = %v", err)
	}
	rootPath := filepath.Join(t.TempDir(), "out")
	draft, err = draft.ConfirmPath(rootPath)
	if err != nil || !draft.HasOutputTarget() {
		t.Fatalf("confirmed draft=%+v err=%v", draft, err)
	}
	intent, err := draft.Freeze(backend, OutputNativeTree)
	if err != nil || intent.IsZero() {
		t.Fatalf("intent=%+v err=%v", intent, err)
	}
	canonical := intent.CanonicalBytes()
	canonical[0] ^= 0xff
	if bytes.Equal(canonical, intent.CanonicalBytes()) {
		t.Fatal("intent canonical bytes were not defensively copied")
	}
	jobID, err := NewTransferJobID()
	if err != nil || jobID.IsZero() {
		t.Fatalf("job id=%x err=%v", jobID, err)
	}
	if intent.Digest().IsZero() || len(intent.Digest().Bytes()) != TransferIntentDigestBytes {
		t.Fatalf("intent digest=%x", intent.Digest())
	}
}

func TestTransferIntentCanonicalRulesAndTargetsAreDeterministic(t *testing.T) {
	share := transferID[catalog.ShareInstance](31)
	root := transferID[catalog.DirectoryID](32)
	first := transferID[catalog.FileID](33)
	second := transferID[catalog.DirectoryID](34)
	rulesA, err := NewSelectionRules(false, []SelectionOverride{
		{DirectoryID: second, Selected: true}, {FileID: first, Selected: true},
	})
	if err != nil {
		t.Fatal(err)
	}
	rulesB, err := NewSelectionRules(false, []SelectionOverride{
		{FileID: first, Selected: true}, {DirectoryID: second, Selected: true},
	})
	if err != nil {
		t.Fatal(err)
	}
	rootTarget, err := NewFilesystemOutputRootTarget(filepath.Join(t.TempDir(), "root"))
	if err != nil {
		t.Fatal(err)
	}
	backend, _ := NewOutputBackendID("windshare/test")
	left, err := NewTransferIntent(share, root, rulesA, rootTarget, backend, OutputNativeTree)
	if err != nil {
		t.Fatal(err)
	}
	right, err := NewTransferIntent(share, root, rulesB, rootTarget, backend, OutputNativeTree)
	if err != nil {
		t.Fatal(err)
	}
	if !left.EqualCanonical(right) || left.Digest() != right.Digest() {
		t.Fatal("equivalent rule maps produced different intent identity")
	}
	otherTarget, err := NewOpaqueOutputTarget(bytes.Repeat([]byte{7}, OutputRootIdentityBytes))
	if err != nil {
		t.Fatal(err)
	}
	other, err := NewTransferIntent(share, root, rulesA, otherTarget, backend, OutputNativeTree)
	if err != nil {
		t.Fatal(err)
	}
	if left.EqualCanonical(other) || left.Digest() == other.Digest() {
		t.Fatal("different output targets shared intent identity")
	}
}

func TestTransferIntentEqualCanonicalRejectsMalformedValues(t *testing.T) {
	share := transferID[catalog.ShareInstance](35)
	root := transferID[catalog.DirectoryID](36)
	rules, err := NewSelectionRules(true, nil)
	if err != nil {
		t.Fatal(err)
	}
	target, err := NewOpaqueOutputTarget(bytes.Repeat([]byte{9}, OutputRootIdentityBytes))
	if err != nil {
		t.Fatal(err)
	}
	backend, _ := NewOutputBackendID("windshare/test")
	valid, err := NewTransferIntent(share, root, rules, target, backend, OutputNativeTree)
	if err != nil {
		t.Fatal(err)
	}
	malformed := valid
	malformed.encoded = append([]byte(nil), valid.encoded...)
	malformed.encoded[len(malformed.encoded)-1] ^= 1
	if malformed.EqualCanonical(valid) || valid.EqualCanonical(malformed) || malformed.EqualCanonical(malformed) {
		t.Fatal("malformed intents were treated as canonically equal")
	}
	if (TransferIntent{}).EqualCanonical(TransferIntent{}) {
		t.Fatal("zero intents were treated as canonically equal")
	}
}

func TestTransferIntentOpaqueTargetSupportsStreamFormats(t *testing.T) {
	share := transferID[catalog.ShareInstance](41)
	root := transferID[catalog.DirectoryID](42)
	rules, err := NewSelectionRules(true, nil)
	if err != nil {
		t.Fatal(err)
	}
	target, err := NewOpaqueOutputTarget(bytes.Repeat([]byte{0x5a}, OutputRootIdentityBytes))
	if err != nil {
		t.Fatal(err)
	}
	if target.Kind() != OutputOpaqueTarget {
		t.Fatalf("opaque target kind = %v", target.Kind())
	}
	zipIntent, err := NewTransferIntent(
		share, root, rules, target, NativeFilesystemOutputBackendID, OutputZIPStream,
	)
	if err != nil {
		t.Fatalf("zip intent = %+v, err=%v", zipIntent, err)
	}
	singleIntent, err := NewTransferIntent(
		share, root, rules, target, NativeFilesystemOutputBackendID, OutputSingleFileStream,
	)
	if err != nil {
		t.Fatalf("single-file intent = %+v, err=%v", singleIntent, err)
	}
	if zipIntent.OutputTarget().Identity() != target.Identity() ||
		zipIntent.Format() != OutputZIPStream || singleIntent.Format() != OutputSingleFileStream ||
		bytes.Equal(zipIntent.CanonicalBytes(), singleIntent.CanonicalBytes()) ||
		zipIntent.Digest() == singleIntent.Digest() {
		t.Fatal("opaque target or stream format was omitted from intent identity")
	}
}

func TestTransferIntentConstructionRejectsIncompleteAuthorityAndFreezesPaths(t *testing.T) {
	share := transferID[catalog.ShareInstance](51)
	root := transferID[catalog.DirectoryID](52)
	rules, err := NewSelectionRules(true, nil)
	if err != nil {
		t.Fatal(err)
	}
	target, err := NewOpaqueOutputTarget(bytes.Repeat([]byte{0x6b}, OutputRootIdentityBytes))
	if err != nil {
		t.Fatal(err)
	}
	backend, err := NewOutputBackendID("windshare/test")
	if err != nil {
		t.Fatal(err)
	}

	invalid := []struct {
		name      string
		share     catalog.ShareInstance
		root      catalog.DirectoryID
		rules     SelectionRules
		target    OutputTarget
		backend   OutputBackendID
		format    OutputMode
		wantError error
	}{
		{name: "share", root: root, rules: rules, target: target, backend: backend, format: OutputNativeTree, wantError: ErrInvalidTransferIntent},
		{name: "root", share: share, rules: rules, target: target, backend: backend, format: OutputNativeTree, wantError: ErrInvalidTransferIntent},
		{name: "rules", share: share, root: root, target: target, backend: backend, format: OutputNativeTree, wantError: ErrInvalidTransferIntent},
		{name: "target", share: share, root: root, rules: rules, backend: backend, format: OutputNativeTree, wantError: ErrInvalidTransferIntent},
		{name: "backend", share: share, root: root, rules: rules, target: target, format: OutputNativeTree, wantError: ErrInvalidOutputBinding},
		{name: "format", share: share, root: root, rules: rules, target: target, backend: backend, wantError: ErrInvalidTransferIntent},
	}
	for _, test := range invalid {
		t.Run(test.name, func(t *testing.T) {
			_, err := NewTransferIntent(
				test.share, test.root, test.rules, test.target, test.backend, test.format,
			)
			if !errors.Is(err, test.wantError) {
				t.Fatalf("construction error = %v, want %v", err, test.wantError)
			}
		})
	}

	if _, err := NewFilesystemTransferIntent(
		share, root, rules, "relative/output", backend, OutputNativeTree,
	); !errors.Is(err, ErrInvalidOutputBinding) {
		t.Fatalf("relative filesystem root error = %v", err)
	}
	relative := filepath.Join("relative", "downloads")
	absolute, err := filepath.Abs(relative)
	if err != nil {
		t.Fatal(err)
	}
	intent, err := NewPathTransferIntent(share, root, rules, relative, backend, OutputNativeTree)
	if err != nil {
		t.Fatal(err)
	}
	if intent.ShareInstance() != share || intent.SyntheticRoot() != root ||
		intent.SelectionMode() != SelectionByNodeID || intent.OutputTarget().RootPath() != filepath.Clean(absolute) ||
		intent.BackendID() != backend || intent.Format() != OutputNativeTree {
		t.Fatalf("path intent did not freeze its complete authority: %+v", intent)
	}
	encoded := intent.Bytes()
	encoded[0] ^= 0xff
	if bytes.Equal(encoded, intent.Bytes()) {
		t.Fatal("intent Bytes exposed mutable canonical storage")
	}
}

func TestTransferIntentDraftRetainsAuthorityThroughNamedFreeze(t *testing.T) {
	share := transferID[catalog.ShareInstance](61)
	root := transferID[catalog.DirectoryID](62)
	rules, err := NewSelectionRules(false, nil)
	if err != nil {
		t.Fatal(err)
	}
	for _, invalid := range []struct {
		name  string
		share catalog.ShareInstance
		root  catalog.DirectoryID
		rules SelectionRules
	}{
		{name: "share", root: root, rules: rules},
		{name: "root", share: share, rules: rules},
		{name: "rules", share: share, root: root},
	} {
		t.Run(invalid.name, func(t *testing.T) {
			if _, err := NewTransferIntentDraft(invalid.share, invalid.root, invalid.rules); !errors.Is(err, ErrInvalidTransferIntent) {
				t.Fatalf("draft construction error = %v", err)
			}
		})
	}

	draft, err := NewTransferIntentDraft(share, root, rules)
	if err != nil {
		t.Fatal(err)
	}
	if draft.ShareInstance() != share || draft.SyntheticRoot() != root ||
		draft.SelectionRules().Mode() != rules.Mode() || draft.HasOutputTarget() {
		t.Fatalf("draft authority = %+v", draft)
	}
	if _, err := draft.ConfirmOutput(OutputTarget{}); !errors.Is(err, ErrInvalidTransferIntent) {
		t.Fatalf("zero output confirmation error = %v", err)
	}
	if _, err := draft.ConfirmFilesystemRoot("relative/output"); !errors.Is(err, ErrInvalidOutputBinding) {
		t.Fatalf("relative output confirmation error = %v", err)
	}
	target, err := NewOpaqueOutputTarget(bytes.Repeat([]byte{0x7c}, OutputRootIdentityBytes))
	if err != nil {
		t.Fatal(err)
	}
	draft, err = draft.ConfirmOutput(target)
	if err != nil {
		t.Fatal(err)
	}
	backend, err := NewOutputBackendID("windshare/test")
	if err != nil {
		t.Fatal(err)
	}
	intent, err := NewTransferIntentFromDraft(draft, backend, OutputSingleFileStream)
	if err != nil {
		t.Fatal(err)
	}
	if intent.ShareInstance() != share || intent.SyntheticRoot() != root ||
		intent.OutputTarget() != target || intent.BackendID() != backend || intent.Format() != OutputSingleFileStream {
		t.Fatalf("frozen draft lost authority: %+v", intent)
	}

	malformed := intent
	malformed.backend = ""
	if malformed.EqualCanonical(intent) {
		t.Fatal("canonical equality accepted an invalid backend binding")
	}
}
