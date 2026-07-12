package protocol_test

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/windshare/windshare/relay/protocol"
)

type relaySignalingVectorFile struct {
	Version     int                  `json:"version"`
	Kind        string               `json:"kind"`
	Description string               `json:"description"`
	Cases       []relaySignalingCase `json:"cases"`
}

type relaySignalingCase struct {
	Name      string `json:"name"`
	Wire      string `json:"wire"`
	Accepted  bool   `json:"accepted"`
	Canonical string `json:"canonical,omitempty"`
}

func buildRelaySignalingVectorFile() relaySignalingVectorFile {
	accepted := func(name, wire, canonical string) relaySignalingCase {
		return relaySignalingCase{Name: name, Wire: wire, Accepted: true, Canonical: canonical}
	}
	acceptedOpaque := func(name, wire string) relaySignalingCase {
		return relaySignalingCase{Name: name, Wire: wire, Accepted: true}
	}
	rejected := func(name, wire string) relaySignalingCase {
		return relaySignalingCase{Name: name, Wire: wire}
	}
	smile := string(rune(0x1f600))
	replacement := string(rune(0xfffd))
	lineSeparator := string(rune(0x2028))
	paragraphSeparator := string(rune(0x2029))
	nestedJSON := func(depth int) string {
		return strings.Repeat("[", depth) + "null" + strings.Repeat("]", depth)
	}
	payloadAtDepthLimit := nestedJSON(protocol.MaxSignalingJSONDepth - 1)
	payloadAboveDepthLimit := nestedJSON(protocol.MaxSignalingJSONDepth)

	return relaySignalingVectorFile{
		Version:     vectorEnvelopeVersion,
		Kind:        "relay-signaling",
		Description: "Shared Go/browser signaling-object contract. accepted=true requires both runtimes to decode the exact-key JSON object. canonical, when present, is the shared re-encoding for inputs whose opaque payload has one cross-runtime lexical form; its absence deliberately makes no byte-canonical claim for parser-sensitive payload JSON. Protocol strings must be non-empty Unicode scalar text; optional strings are either absent or non-empty; payload remains opaque valid JSON. All JSON structure, including ignored fields, is bounded to 64 nested objects/arrays so runtime parser recursion limits are not protocol behavior. accepted=false pins hostile spellings that both runtimes must reject before routing.",
		Cases: []relaySignalingCase{
			accepted("keepalive", `{"type":"keepalive"}`, `{"type":"keepalive"}`),
			accepted("join", `{"type":"join","shareId":"AAAAAAAAAAAA"}`, `{"type":"join","shareId":"AAAAAAAAAAAA"}`),
			accepted("manifest", `{"type":"manifest","sessionId":"AQIDBAUGBwg"}`, `{"type":"manifest","sessionId":"AQIDBAUGBwg"}`),
			accepted("not-found", `{"type":"not_found"}`, `{"type":"not_found"}`),
			accepted("signal-offer", `{"type":"signal","sessionId":"AQIDBAUGBwg","kind":"offer","payload":{"type":"offer","sdp":"v=0"}}`, `{"type":"signal","sessionId":"AQIDBAUGBwg","kind":"offer","payload":{"type":"offer","sdp":"v=0"}}`),
			accepted("bye", `{"type":"bye","sessionId":"AQIDBAUGBwg"}`, `{"type":"bye","sessionId":"AQIDBAUGBwg"}`),
			accepted("connection-error", `{"type":"error","code":"rate_limited","message":"slow down"}`, `{"type":"error","code":"rate_limited","message":"slow down"}`),
			accepted("session-error", `{"type":"error","code":"sender_gone","message":"offline","sessionId":"AQIDBAUGBwg"}`, `{"type":"error","code":"sender_gone","message":"offline","sessionId":"AQIDBAUGBwg"}`),
			accepted("serializer-sensitive-protocol-strings", `{"type":"error","code":"<>&","message":"\u2028\u2029"}`, `{"type":"error","code":"<>&","message":"`+lineSeparator+paragraphSeparator+`"}`),
			accepted("unknown-field-is-ignored", `{"type":"join","shareId":"AAAAAAAAAAAA","future":true}`, `{"type":"join","shareId":"AAAAAAAAAAAA"}`),
			accepted("case-alias-is-only-unknown", `{"type":"join","shareId":"AAAAAAAAAAAA","ShareID":"bad!"}`, `{"type":"join","shareId":"AAAAAAAAAAAA"}`),
			accepted("paired-surrogate-is-scalar", `{"type":"signal","sessionId":"AQIDBAUGBwg","kind":"\ud83d\ude00","payload":null}`, `{"type":"signal","sessionId":"AQIDBAUGBwg","kind":"`+smile+`","payload":null}`),
			accepted("replacement-character-is-scalar", `{"type":"error","code":"x","message":"\ufffd"}`, `{"type":"error","code":"x","message":"`+replacement+`"}`),
			accepted("serializer-sensitive-payload-string", `{"type":"signal","sessionId":"AQIDBAUGBwg","kind":"candidate","payload":{"html":"<>&","separator":"\u2028"}}`, `{"type":"signal","sessionId":"AQIDBAUGBwg","kind":"candidate","payload":{"html":"<>&","separator":"`+lineSeparator+`"}}`),
			acceptedOpaque("payload-integer-key-order", `{"type":"signal","sessionId":"AQIDBAUGBwg","kind":"candidate","payload":{"2":"two","1":"one"}}`),
			acceptedOpaque("payload-duplicate-keys", `{"type":"signal","sessionId":"AQIDBAUGBwg","kind":"candidate","payload":{"candidate":"first","candidate":"last"}}`),
			acceptedOpaque("payload-number-lexemes", `{"type":"signal","sessionId":"AQIDBAUGBwg","kind":"candidate","payload":{"minusZero":-0,"exponent":1e2}}`),
			accepted("payload-at-depth-limit", `{"type":"signal","sessionId":"AQIDBAUGBwg","kind":"offer","payload":`+payloadAtDepthLimit+`}`, `{"type":"signal","sessionId":"AQIDBAUGBwg","kind":"offer","payload":`+payloadAtDepthLimit+`}`),
			accepted("ignored-field-at-depth-limit", `{"type":"keepalive","future":`+payloadAtDepthLimit+`}`, `{"type":"keepalive"}`),
			rejected("top-level-array", `[]`),
			rejected("type-case-alias", `{"Type":"join","shareId":"AAAAAAAAAAAA"}`),
			rejected("share-id-case-alias", `{"type":"join","ShareId":"AAAAAAAAAAAA"}`),
			rejected("lone-high-surrogate", `{"type":"signal","sessionId":"AQIDBAUGBwg","kind":"\ud800","payload":{}}`),
			rejected("lone-low-surrogate", `{"type":"signal","sessionId":"AQIDBAUGBwg","kind":"\udc00","payload":{}}`),
			rejected("broken-surrogate-pair", `{"type":"signal","sessionId":"AQIDBAUGBwg","kind":"\ud800\u0041","payload":{}}`),
			rejected("null-kind", `{"type":"signal","sessionId":"AQIDBAUGBwg","kind":null,"payload":{}}`),
			rejected("missing-payload", `{"type":"signal","sessionId":"AQIDBAUGBwg","kind":"offer"}`),
			rejected("null-error-message", `{"type":"error","code":"x","message":null}`),
			rejected("empty-error-message", `{"type":"error","code":"x","message":""}`),
			rejected("non-scalar-error-message", `{"type":"error","code":"x","message":"\ud800"}`),
			rejected("null-optional-session", `{"type":"error","code":"x","sessionId":null}`),
			rejected("empty-optional-session", `{"type":"error","code":"x","sessionId":""}`),
			rejected("unknown-type", `{"type":"teleport"}`),
			rejected("payload-above-depth-limit", `{"type":"signal","sessionId":"AQIDBAUGBwg","kind":"offer","payload":`+payloadAboveDepthLimit+`}`),
			rejected("ignored-field-above-depth-limit", `{"type":"keepalive","future":`+payloadAboveDepthLimit+`}`),
		},
	}
}

func TestRelaySignalingVectorFileUpToDate(t *testing.T) {
	path := filepath.Join(vectorsDir, "relay-signaling.json")
	want := renderVectorFile(t, buildRelaySignalingVectorFile())
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

func TestRelaySignalingVector(t *testing.T) {
	data, err := os.ReadFile(filepath.Join(vectorsDir, "relay-signaling.json"))
	if err != nil {
		t.Fatal(err)
	}
	var vector relaySignalingVectorFile
	if err := json.Unmarshal(data, &vector); err != nil {
		t.Fatalf("decode relay signaling vector: %v", err)
	}
	if vector.Version != vectorEnvelopeVersion || vector.Kind != "relay-signaling" {
		t.Fatalf("unexpected vector envelope version=%d kind=%q", vector.Version, vector.Kind)
	}
	for _, testCase := range vector.Cases {
		t.Run(testCase.Name, func(t *testing.T) {
			message, err := protocol.Decode([]byte(testCase.Wire))
			if !testCase.Accepted {
				if err == nil {
					t.Fatalf("Decode accepted hostile signaling as %#v", message)
				}
				return
			}
			if err != nil {
				t.Fatalf("Decode accepted vector: %v", err)
			}
			canonical, err := protocol.Encode(message)
			if err != nil {
				t.Fatalf("Encode accepted vector: %v", err)
			}
			if testCase.Canonical != "" && string(canonical) != testCase.Canonical {
				t.Fatalf("canonical signaling = %s, want %s", canonical, testCase.Canonical)
			}
		})
	}
}
