package nodedirectory

import (
	"bytes"
	"crypto/ed25519"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
	"unicode"
	"unicode/utf8"
)

// These bytes exercise both implementations, including signatures over JSON
// formatting that a parse-and-reserialize verifier must not silently accept.
func TestBrowserDirectoryVectors(t *testing.T) {
	f := newFixture()
	type vector struct {
		Name     string          `json:"name"`
		Envelope json.RawMessage `json:"envelope"`
		Valid    bool            `json:"valid"`
	}
	var vectors []vector
	add := func(name string, payload Payload) {
		vectors = append(vectors, vector{name, f.signed(t, payload), true})
	}
	add("valid", f.payload(1, 42))
	add("older_serial", f.payload(1, 41))
	add("new_generation", f.payload(2, 1))
	draining := f.payload(2, 2)
	draining.Nodes[0].Status = Draining
	add("draining", draining)
	removed := f.payload(2, 3)
	removed.Nodes = nil
	add("removed", removed)
	reassigned := f.payload(3, 1)
	reassigned.Nodes[0].Relay = "https://replacement.example"
	add("reassigned", reassigned)
	expired := f.payload(4, 1)
	expired.IssuedAt = f.now.Add(-23 * time.Hour)
	expired.ExpiresAt = f.now.Add(-22 * time.Hour)
	add("expired_generation", expired)
	var original Envelope
	if err := json.Unmarshal(vectors[0].Envelope, &original); err != nil {
		t.Fatal(err)
	}
	mutateAs := func(name string, valid bool, change func(*Envelope)) {
		copy := original
		change(&copy)
		data, _ := json.Marshal(copy)
		vectors = append(vectors, vector{name, data, valid})
	}
	mutate := func(name string, change func(*Envelope)) { mutateAs(name, false, change) }
	mutate("unknown_root", func(e *Envelope) { e.Certificate.Root = "unknown" })
	mutate("bad_signature", func(e *Envelope) { e.Signature = encoding.EncodeToString(make([]byte, ed25519.SignatureSize)) })
	mutate("reserialized_payload", func(e *Envelope) {
		payload, _ := encoding.DecodeString(e.Payload)
		e.Payload = encoding.EncodeToString(append([]byte("\n"), payload...))
	})
	replace := func(name, old, next string, valid bool) {
		mutateAs(name, valid, func(e *Envelope) {
			payload, _ := encoding.DecodeString(e.Payload)
			payload = bytes.Replace(payload, []byte(old), []byte(next), 1)
			e.Payload = encoding.EncodeToString(payload)
			e.Signature = encoding.EncodeToString(ed25519.Sign(f.online, append([]byte(PayloadDomain), payload...)))
		})
	}
	for _, replacement := range []struct{ name, old, next string }{
		{"duplicate_field", `"serial":42`, `"serial":42,"serial":43`},
		{"case_folded_field", `"serial":42`, `"Serial":42`},
		{"fractional_counter", `"serial":42`, `"serial":42.0`},
		{"exponential_counter", `"serial":42`, `"serial":42e0`},
		{"invalid_utf8", `"jp1"`, "\"jp\xff\""},
		{"generation_forgery", `"generation":1`, `"generation":2`},
		{"invalid_calendar", `2026-11-12`, `2026-02-30`},
		// JavaScript's trim() disagrees with Unicode White_Space at exactly these
		// two code points, so each is pinned at both edges of a text field.
		{"text_leading_byte_order_mark", `"region":"jp"`, "\"region\":\"\ufeffjp\""},
		{"text_trailing_byte_order_mark", `"region":"jp"`, "\"region\":\"jp\ufeff\""},
		{"text_leading_next_line", `"region":"jp"`, "\"region\":\"\u0085jp\""},
		{"text_trailing_next_line", `"region":"jp"`, "\"region\":\"jp\u0085\""},
	} {
		replace(replacement.name, replacement.old, replacement.next, false)
	}
	// Appended so the entries above keep their bytes. Whitespace inside text is
	// valid even next to a line separator, which a multiline edge check would
	// reject, and the 128 limit counts UTF-8 bytes rather than characters.
	replace("text_inner_space_before_line_separator", `"region":"jp"`, "\"region\":\"j \u2028p\"", true)
	replace("text_128_utf8_bytes", `"region":"jp"`, `"region":"`+strings.Repeat("\u00e9", 64)+`"`, true)
	replace("text_129_utf8_bytes", `"region":"jp"`, `"region":"`+strings.Repeat("\u00e9", 63)+"\u65e5\"", false)
	for _, item := range vectors {
		_, err := Verify(item.Envelope, "test", f.roots)
		if (err == nil) != item.Valid {
			t.Fatalf("%s: %v", item.Name, err)
		}
	}
	data, err := json.MarshalIndent(struct {
		Network string              `json:"network"`
		Now     time.Time           `json:"now"`
		Roots   []map[string]string `json:"roots"`
		Entries []vector            `json:"entries"`
	}{"test", f.now, []map[string]string{{"id": "primary", "key": encoding.EncodeToString(f.roots[0].Key)}}, vectors}, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	compareNetworkVectors(t, "directory.json", data)
}

// The expected set is built from unicode.IsSpace plus U+FEFF rather than read
// back from validText, so this cross-checks validText against the predicate it
// is meant to implement; it shares Go's White_Space table with validText. The
// exported bytes pin that table for review and for the browser sweep.
func TestTextEdgeVectors(t *testing.T) {
	edges, mismatches := []rune{}, []string{}
	for r := rune(0); r <= unicode.MaxRune; r++ {
		if !utf8.ValidRune(r) {
			continue
		}
		edge := unicode.IsSpace(r) || r == '\ufeff'
		if edge {
			edges = append(edges, r)
		}
		control := strings.ContainsRune("\x00\r\n\t", r)
		char := string(r)
		if validText(char+"a") == (edge || control) || validText("a"+char) == (edge || control) || validText("a"+char+"b") == control {
			mismatches = append(mismatches, fmt.Sprintf("U+%04X (edge=%v control=%v)", r, edge, control))
		}
	}
	if len(mismatches) > 0 {
		t.Fatalf("validText disagrees at %d code points: %s", len(mismatches), strings.Join(mismatches, ", "))
	}
	data, err := json.MarshalIndent(struct {
		InvalidEdgeCodePoints []rune `json:"invalid_edge_code_points"`
	}{edges}, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	compareNetworkVectors(t, "text-edges.json", data)
}

func compareNetworkVectors(t *testing.T, name string, data []byte) {
	t.Helper()
	data = append(data, '\n')
	path := filepath.Join("..", "..", "testdata", "network", name)
	if os.Getenv("UPDATE_NETWORK_VECTORS") == "1" {
		if err := os.MkdirAll(filepath.Dir(path), 0755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, data, 0644); err != nil {
			t.Fatal(err)
		}
	}
	existing, err := os.ReadFile(path)
	if err != nil || !bytes.Equal(existing, data) {
		t.Fatal("refresh network vectors with UPDATE_NETWORK_VECTORS=1", err)
	}
}
