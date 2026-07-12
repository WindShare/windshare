package protocol

import (
	"bytes"
	"testing"
)

func TestManifestFrameRoundTrip(t *testing.T) {
	sealed := []byte{0xde, 0xad, 0xbe, 0xef}
	f := EncodeManifestFrame(sealed)
	if f[0] != BinTypeManifest || len(f) != len(sealed)+ManifestOverheadBytes {
		t.Fatalf("frame layout mismatch: % x", f)
	}
	got, err := DecodeManifestFrame(f)
	if err != nil || !bytes.Equal(got, sealed) {
		t.Fatalf("round trip failed: %v % x", err, got)
	}
	// 空清单帧(仅前缀)合法解出空字节——大小下限由 signaling 语义层把关。
	if got, err := DecodeManifestFrame([]byte{BinTypeManifest}); err != nil || len(got) != 0 {
		t.Fatalf("empty manifest frame: %v", err)
	}
}

func TestManifestFrameRejects(t *testing.T) {
	if _, err := DecodeManifestFrame(nil); err == nil {
		t.Error("empty frame should be rejected")
	}
	if _, err := DecodeManifestFrame([]byte{BinTypeForward, 1, 2}); err == nil {
		t.Error("wrong type prefix should be rejected")
	}
}

func TestForwardFrameRoundTrip(t *testing.T) {
	id := SessionID{9, 8, 7, 6, 5, 4, 3, 2}
	inner := []byte("opaque-data-plane-frame")
	f := EncodeForwardFrame(id, inner)
	if f[0] != BinTypeForward || len(f) != len(inner)+ForwardOverheadBytes {
		t.Fatalf("frame layout mismatch: % x", f)
	}
	gotID, gotInner, err := DecodeForwardFrame(f)
	if err != nil || gotID != id || !bytes.Equal(gotInner, inner) {
		t.Fatalf("round trip failed: %v %v % x", err, gotID, gotInner)
	}
}

func TestTerminalForwardFrameRoundTrip(t *testing.T) {
	id := SessionID{1, 2, 3, 4, 5, 6, 7, 8}
	inner := []byte("opaque-terminal")
	frame := EncodeTerminalForwardFrame(id, inner)
	if frame[0] != BinTypeTerminalForward || len(frame) != len(inner)+TerminalForwardOverheadBytes {
		t.Fatalf("terminal frame layout = % x", frame)
	}
	gotID, gotInner, err := DecodeTerminalForwardFrame(frame)
	if err != nil || gotID != id || !bytes.Equal(gotInner, inner) {
		t.Fatalf("terminal round trip = %v %v % x", err, gotID, gotInner)
	}
	if _, _, err := DecodeForwardFrame(frame); err == nil {
		t.Fatal("ordinary decoder accepted terminal envelope")
	}
}

func TestForwardFrameRejects(t *testing.T) {
	if _, _, err := DecodeForwardFrame([]byte{BinTypeForward, 1, 2}); err == nil {
		t.Error("truncated header should be rejected")
	}
	if _, _, err := DecodeForwardFrame(EncodeManifestFrame([]byte("x"))); err == nil {
		t.Error("wrong type prefix should be rejected")
	}
}

func TestSessionIDStringRoundTrip(t *testing.T) {
	id := SessionID{0xff, 0, 0x7f, 1, 2, 3, 4, 5}
	s := id.String()
	got, err := ParseSessionID(s)
	if err != nil || got != id {
		t.Fatalf("round trip failed: %v %v", err, got)
	}
	if _, err := ParseSessionID("too-short"); err == nil {
		t.Error("invalid length should be rejected")
	}
	if _, err := ParseSessionID(nonCanonicalBase64URLAlias(s)); err == nil {
		t.Error("non-canonical trailing bits should be rejected")
	}
}

func TestBinType(t *testing.T) {
	if BinType(nil) != 0 {
		t.Error("empty frame should return 0")
	}
	if BinType([]byte{BinTypeManifest}) != BinTypeManifest {
		t.Error("prefix was read incorrectly")
	}
}
