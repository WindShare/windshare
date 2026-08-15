package destinationauthority

import (
	"bytes"
	"errors"
	"path/filepath"
	"testing"

	"github.com/windshare/windshare/core/osfs/internal/outputcap"
)

func TestBindingSeparatesDisplayPathFromDestinationAuthority(t *testing.T) {
	id, err := outputcap.DestinationAuthorityIDFromBytes(bytes.Repeat([]byte{0x31}, 32))
	if err != nil {
		t.Fatal(err)
	}
	supported := outputcap.SupportedCapability()
	facts, err := outputcap.NewDestinationCapabilities(supported, supported, supported, supported)
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Clean(filepath.Join(filepath.VolumeName(t.TempDir())+string(filepath.Separator), "downloads"))
	if !filepath.IsAbs(path) {
		path = filepath.Clean(t.TempDir())
	}
	binding, err := NewBinding(id, facts, path)
	mode, modeErr := binding.ExecutionMode()
	if err != nil || modeErr != nil || !binding.Valid() || binding.ID() != id ||
		mode != outputcap.ExecutionResumable || binding.DisplayPath() != path ||
		!bytes.Equal(binding.AuthorityRef().Bytes(), id.Bytes()) {
		t.Fatalf("binding = %+v, %v/%v", binding, err, modeErr)
	}
	liveUnsupported, _ := outputcap.UnsupportedCapability(outputcap.CapabilityReasonUnverifiableRangeRecovery)
	liveFacts, _ := outputcap.NewDestinationCapabilities(supported, supported, liveUnsupported, supported)
	liveBinding, err := NewBinding(id, liveFacts, path)
	liveMode, liveModeErr := liveBinding.ExecutionMode()
	if err != nil || liveModeErr != nil || liveMode != outputcap.ExecutionLiveOnly {
		t.Fatalf("live binding = %+v, %v/%v", liveBinding, err, liveModeErr)
	}
	unsafe, _ := outputcap.UnsupportedCapability(outputcap.CapabilityReasonUnsafePublication)
	unsafeFacts, _ := outputcap.NewDestinationCapabilities(unsafe, supported, supported, supported)
	unsafeBinding, err := NewBinding(id, unsafeFacts, path)
	if err != nil {
		t.Fatalf("binding must retain independently reported facts: %v", err)
	}
	if _, err := unsafeBinding.ExecutionMode(); !errors.Is(err, outputcap.ErrOrdinaryOutputUnsupported) {
		t.Fatalf("unsafe mode error = %v", err)
	}
}
