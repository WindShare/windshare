package resumestate

import (
	"bytes"
	"strings"
	"testing"

	"github.com/windshare/windshare/core/transfer"
)

func TestControlCertificationAndOutputRootAccessors(t *testing.T) {
	for _, value := range []CertificationID{CertificationLinuxExt4ProcessRestart, CertificationWindowsNTFSProcessRestart} {
		got, err := NewCertificationID(string(value))
		if err != nil || got != value {
			t.Fatalf("certification %q = %q, %v", value, got, err)
		}
	}
	if _, err := NewCertificationID("unsupported"); err == nil {
		t.Fatal("unsupported certification was accepted")
	}
	root := testRootBindingFor(t, CertificationLinuxExt4ProcessRestart, 5)
	if root.Certification() != CertificationLinuxExt4ProcessRestart || root.IsZero() || len(root.Bytes()) != OutputRootBindingBytes || root.String() == "" {
		t.Fatalf("root accessors = %+v", root)
	}
	bytesCopy := root.Bytes()
	bytesCopy[0] ^= 1
	if bytes.Equal(bytesCopy, root.Bytes()) {
		t.Fatal("root bytes must be defensive copy")
	}
	control, err := NewControl(ControlSpec{
		Backend:       testBackend(t),
		OutputRoot:    root,
		Certification: root.Certification(),
		Durability:    transfer.DurabilityProcessRestart,
		Generation:    1,
	})
	if err != nil {
		t.Fatal(err)
	}
	if control.OutputRoot() != root || control.Certification() != root.Certification() || control.Generation() != 1 {
		t.Fatalf("control accessors = %+v", control)
	}
}

func TestGeneratedAndDecodedFixedIdentities(t *testing.T) {
	raw := bytes.Repeat([]byte{0xA5}, OutputObjectIDBytes)
	id, err := OutputObjectIDFromBytes(raw)
	if err != nil || id.IsZero() || id.String() != strings.Repeat("a5", OutputObjectIDBytes) || !bytes.Equal(id.Bytes(), raw) {
		t.Fatalf("object identity = %q, %v", id, err)
	}
	if _, err := OutputObjectIDFromBytes(nil); err == nil {
		t.Fatal("short object identity was accepted")
	}
	if _, err := OutputObjectIDFromBytes(make([]byte, OutputObjectIDBytes)); err == nil {
		t.Fatal("zero object identity was accepted")
	}
	nonce, err := GenerateBootstrapNonce(bytes.NewReader(raw))
	if err != nil || nonce.IsZero() || len(nonce.Bytes()) != BootstrapNonceBytes {
		t.Fatalf("bootstrap nonce = %q, %v", nonce, err)
	}
	update, err := GenerateUpdateNonce(bytes.NewReader(raw))
	if err != nil || update.IsZero() || len(update.Bytes()) != UpdateNonceBytes || update.String() == "" {
		t.Fatalf("update nonce = %q, %v", update, err)
	}
	if _, err := GenerateUpdateNonce(nil); err == nil {
		t.Fatal("nil update entropy source was accepted")
	}
	if generated, err := NewOutputObjectID(); err != nil || generated.IsZero() {
		t.Fatalf("crypto output object allocation = %q, %v", generated, err)
	}
	if generated, err := NewBootstrapNonce(); err != nil || generated.IsZero() {
		t.Fatalf("crypto bootstrap allocation = %q, %v", generated, err)
	}
	if generated, err := NewUpdateNonce(); err != nil || generated.IsZero() {
		t.Fatalf("crypto update allocation = %q, %v", generated, err)
	}
}
