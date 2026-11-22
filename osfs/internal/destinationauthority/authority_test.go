package destinationauthority

import (
	"bytes"
	"errors"
	"path/filepath"
	"testing"

	"github.com/windshare/windshare/osfs/internal/outputcap"
)

func TestBindingSeparatesDisplayPathFromDestinationAuthority(t *testing.T) {
	id, err := outputcap.DestinationBindingIDFromBytes(bytes.Repeat([]byte{0x31}, 32))
	if err != nil {
		t.Fatal(err)
	}
	supported := outputcap.SupportedCapability()
	facts, err := outputcap.NewDestinationCapabilities(supported, supported)
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Clean(filepath.Join(filepath.VolumeName(t.TempDir())+string(filepath.Separator), "downloads"))
	if !filepath.IsAbs(path) {
		path = filepath.Clean(t.TempDir())
	}
	binding, err := NewBinding(id, facts, path)
	if err != nil || !binding.Valid() || binding.ID() != id ||
		binding.Capabilities().ValidateSupport() != nil || binding.DisplayPath() != path ||
		!bytes.Equal(binding.AuthorityRef().Bytes(), id.Bytes()) {
		t.Fatalf("binding = %+v, %v", binding, err)
	}
	unverifiable, _ := outputcap.UnsupportedCapability(outputcap.CapabilityReasonUnverifiableOperationRecovery)
	unverifiableFacts, _ := outputcap.NewDestinationCapabilities(supported, unverifiable)
	unverifiableBinding, err := NewBinding(id, unverifiableFacts, path)
	if err != nil {
		t.Fatalf("binding must retain independently reported facts: %v", err)
	}
	if err := unverifiableBinding.Capabilities().ValidateSupport(); !errors.Is(err, outputcap.ErrRecoverableOutputUnsupported) {
		t.Fatalf("unverifiable recovery error = %v", err)
	}
	unsafe, _ := outputcap.UnsupportedCapability(outputcap.CapabilityReasonUnsafePublication)
	unsafeFacts, _ := outputcap.NewDestinationCapabilities(unsafe, supported)
	unsafeBinding, err := NewBinding(id, unsafeFacts, path)
	if err != nil {
		t.Fatalf("binding must retain independently reported facts: %v", err)
	}
	if err := unsafeBinding.Capabilities().ValidateSupport(); !errors.Is(err, outputcap.ErrOrdinaryOutputUnsupported) {
		t.Fatalf("unsafe publication error = %v", err)
	}
}
