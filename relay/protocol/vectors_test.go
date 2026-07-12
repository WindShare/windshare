package protocol_test

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"flag"
	"os"
	"path/filepath"
	"testing"

	"github.com/windshare/windshare/core/session"
	"github.com/windshare/windshare/relay/protocol"
)

var updateVectors = flag.Bool("update", false, "regenerate relay golden-vector files")

const (
	vectorEnvelopeVersion = 1
	vectorsDir            = "../../testvectors"
	vectorManifestType    = byte(0x01)
	vectorForwardType     = byte(0x02)
	vectorTerminalType    = byte(0x03)
	vectorSessionIDBytes  = 8
	vectorInnerFrameSize  = 64 << 10
	vectorBlockPayload    = vectorInnerFrameSize - 18
	vectorForwardOverhead = 1 + vectorSessionIDBytes
)

type relayVectorFile struct {
	Version     int                 `json:"version"`
	Kind        string              `json:"kind"`
	Description string              `json:"description"`
	Cases       []relayEnvelopeCase `json:"cases"`
}

type relayEnvelopeCase struct {
	Name         string `json:"name"`
	Envelope     string `json:"envelope"`
	TypeByte     byte   `json:"typeByte"`
	SessionIDB64 string `json:"sessionIdB64,omitempty"`
	PayloadB64   string `json:"payloadB64"`
	FrameB64     string `json:"frameB64"`
}

func buildRelayVectorFile(t *testing.T) relayVectorFile {
	t.Helper()
	if protocol.BinTypeManifest != vectorManifestType || protocol.BinTypeForward != vectorForwardType ||
		protocol.BinTypeTerminalForward != vectorTerminalType || protocol.SessionIDBytes != vectorSessionIDBytes ||
		protocol.ForwardOverheadBytes != vectorForwardOverhead || protocol.TerminalForwardOverheadBytes != vectorForwardOverhead ||
		session.MaxFrameSize != vectorInnerFrameSize || session.MaxBlockPayload != vectorBlockPayload {
		t.Fatalf("relay envelope implementation drifted from the normative vector contract")
	}
	manifestPayload := []byte{0xde, 0xad, 0xbe, 0xef, 0x00}
	requestFrame, err := session.EncodeRequest([]uint64{1, 0x0102030405060708})
	if err != nil {
		t.Fatalf("encode ordinary inner REQUEST: %v", err)
	}
	terminalFrame, err := session.EncodeError(session.ErrCodeBlockRead, "drift")
	if err != nil {
		t.Fatalf("encode terminal inner ERROR: %v", err)
	}
	var sessionID protocol.SessionID
	copy(sessionID[:], []byte{0x00, 0x01, 0x02, 0x03, 0x04, 0x05, 0x06, 0x07})

	build := func(name, envelope string, typeByte byte, payload, frame []byte, routed bool) relayEnvelopeCase {
		c := relayEnvelopeCase{
			Name: name, Envelope: envelope, TypeByte: typeByte,
			PayloadB64: base64.StdEncoding.EncodeToString(payload),
			FrameB64:   base64.StdEncoding.EncodeToString(frame),
		}
		if routed {
			c.SessionIDB64 = base64.StdEncoding.EncodeToString(sessionID[:])
		}
		return c
	}

	return relayVectorFile{
		Version:     vectorEnvelopeVersion,
		Kind:        "relay-envelope",
		Description: "Relay binary envelopes. Binary values use standard padded base64. manifest = 0x01 ‖ sealedManifest; forward = 0x02 ‖ sessionId(8 bytes) ‖ opaque inner frame; terminal-forward = 0x03 ‖ sessionId(8 bytes) ‖ opaque final inner ERROR frame. Routed envelopes add exactly 9 bytes to the inner frame, so a 65536-byte MaxFrameSize inner frame occupies 65545 bytes on the relay WebSocket. The terminal type is a lifecycle boundary: once accepted it is delivered ahead of queued session data, then the session is tombstoned and later traffic is discarded.",
		Cases: []relayEnvelopeCase{
			build("manifest", "manifest", vectorManifestType, manifestPayload, protocol.EncodeManifestFrame(manifestPayload), false),
			build("ordinary-forward", "forward", vectorForwardType, requestFrame, protocol.EncodeForwardFrame(sessionID, requestFrame), true),
			build("terminal-forward", "terminal-forward", vectorTerminalType, terminalFrame, protocol.EncodeTerminalForwardFrame(sessionID, terminalFrame), true),
		},
	}
}

func renderVectorFile(t *testing.T, vector any) []byte {
	t.Helper()
	var out bytes.Buffer
	encoder := json.NewEncoder(&out)
	encoder.SetEscapeHTML(false)
	encoder.SetIndent("", "  ")
	if err := encoder.Encode(vector); err != nil {
		t.Fatalf("encode relay vector: %v", err)
	}
	return out.Bytes()
}

func TestRelayVectorFileUpToDate(t *testing.T) {
	path := filepath.Join(vectorsDir, "relay-envelope.json")
	want := renderVectorFile(t, buildRelayVectorFile(t))
	if *updateVectors {
		if err := os.WriteFile(path, want, 0o644); err != nil {
			t.Fatalf("write %s: %v", path, err)
		}
	}
	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s (run go test ./relay/protocol -update first): %v", path, err)
	}
	if !bytes.Equal(got, want) {
		t.Fatalf("%s is stale; regenerate it with go test ./relay/protocol -update and review the diff", path)
	}
}

func TestRelayEnvelopeVector(t *testing.T) {
	data, err := os.ReadFile(filepath.Join(vectorsDir, "relay-envelope.json"))
	if err != nil {
		t.Fatal(err)
	}
	var vector relayVectorFile
	if err := json.Unmarshal(data, &vector); err != nil {
		t.Fatalf("decode relay vector: %v", err)
	}
	if vector.Version != vectorEnvelopeVersion || vector.Kind != "relay-envelope" {
		t.Fatalf("unexpected vector envelope version=%d kind=%q", vector.Version, vector.Kind)
	}
	for _, c := range vector.Cases {
		t.Run(c.Name, func(t *testing.T) {
			frame := decodeVectorBase64(t, c.FrameB64)
			payload := decodeVectorBase64(t, c.PayloadB64)
			if got := protocol.BinType(frame); got != c.TypeByte {
				t.Fatalf("type byte = 0x%02x, want 0x%02x", got, c.TypeByte)
			}
			switch c.Envelope {
			case "manifest":
				gotPayload, err := protocol.DecodeManifestFrame(frame)
				if err != nil {
					t.Fatalf("DecodeManifestFrame: %v", err)
				}
				if !bytes.Equal(gotPayload, payload) {
					t.Fatalf("manifest payload mismatch")
				}
				if got := protocol.EncodeManifestFrame(payload); !bytes.Equal(got, frame) {
					t.Fatalf("manifest re-encode mismatch")
				}
			case "forward", "terminal-forward":
				sessionBytes := decodeVectorBase64(t, c.SessionIDB64)
				if len(sessionBytes) != protocol.SessionIDBytes {
					t.Fatalf("session ID length = %d, want %d", len(sessionBytes), protocol.SessionIDBytes)
				}
				var wantSession protocol.SessionID
				copy(wantSession[:], sessionBytes)
				var gotSession protocol.SessionID
				var gotPayload []byte
				var err error
				if c.Envelope == "forward" {
					gotSession, gotPayload, err = protocol.DecodeForwardFrame(frame)
					if reencoded := protocol.EncodeForwardFrame(wantSession, payload); !bytes.Equal(reencoded, frame) {
						t.Fatalf("forward re-encode mismatch")
					}
				} else {
					gotSession, gotPayload, err = protocol.DecodeTerminalForwardFrame(frame)
					if reencoded := protocol.EncodeTerminalForwardFrame(wantSession, payload); !bytes.Equal(reencoded, frame) {
						t.Fatalf("terminal re-encode mismatch")
					}
				}
				if err != nil {
					t.Fatalf("decode routed envelope: %v", err)
				}
				if gotSession != wantSession || !bytes.Equal(gotPayload, payload) {
					t.Fatalf("decoded routed envelope mismatch")
				}
				inner, err := session.Decode(gotPayload)
				if err != nil {
					t.Fatalf("decode inner frame: %v", err)
				}
				if c.Envelope == "forward" {
					if _, ok := inner.(*session.Request); !ok {
						t.Fatalf("ordinary envelope inner frame = %T, want REQUEST", inner)
					}
				} else {
					terminal, ok := inner.(*session.Error)
					if !ok || terminal.Code != session.ErrCodeBlockRead || terminal.Msg != "drift" {
						t.Fatalf("terminal envelope inner frame = %#v, want block-read ERROR", inner)
					}
				}
			default:
				t.Fatalf("unknown envelope %q", c.Envelope)
			}
		})
	}
}

func TestRelayEnvelopeMaximumInnerFrameBoundary(t *testing.T) {
	inner, err := session.EncodeBlock(session.Block{
		Index: 7, Last: true, Payload: make([]byte, vectorBlockPayload),
	})
	if err != nil {
		t.Fatalf("encode maximum inner BLOCK: %v", err)
	}
	if len(inner) != vectorInnerFrameSize {
		t.Fatalf("inner frame length = %d, want %d", len(inner), vectorInnerFrameSize)
	}
	var sessionID protocol.SessionID
	outer := protocol.EncodeForwardFrame(sessionID, inner)
	if len(outer) != vectorInnerFrameSize+vectorForwardOverhead {
		t.Fatalf("relay frame length = %d, want %d", len(outer), vectorInnerFrameSize+vectorForwardOverhead)
	}
}

func decodeVectorBase64(t *testing.T, encoded string) []byte {
	t.Helper()
	decoded, err := base64.StdEncoding.DecodeString(encoded)
	if err != nil {
		t.Fatalf("decode base64: %v", err)
	}
	return decoded
}
