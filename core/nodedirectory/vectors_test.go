package nodedirectory

import (
	"bytes"
	"crypto/ed25519"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"
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
	mutate := func(name string, change func(*Envelope)) {
		copy := original
		change(&copy)
		data, _ := json.Marshal(copy)
		vectors = append(vectors, vector{name, data, false})
	}
	mutate("unknown_root", func(e *Envelope) { e.Certificate.Root = "unknown" })
	mutate("bad_signature", func(e *Envelope) { e.Signature = encoding.EncodeToString(make([]byte, ed25519.SignatureSize)) })
	mutate("reserialized_payload", func(e *Envelope) {
		payload, _ := encoding.DecodeString(e.Payload)
		e.Payload = encoding.EncodeToString(append([]byte("\n"), payload...))
	})
	for _, replacement := range []struct{ name, old, next string }{
		{"duplicate_field", `"serial":42`, `"serial":42,"serial":43`},
		{"case_folded_field", `"serial":42`, `"Serial":42`},
		{"fractional_counter", `"serial":42`, `"serial":42.0`},
		{"exponential_counter", `"serial":42`, `"serial":42e0`},
		{"invalid_utf8", `"jp1"`, "\"jp\xff\""},
		{"generation_forgery", `"generation":1`, `"generation":2`},
		{"invalid_calendar", `2026-11-12`, `2026-02-30`},
	} {
		mutate(replacement.name, func(e *Envelope) {
			payload, _ := encoding.DecodeString(e.Payload)
			payload = bytes.Replace(payload, []byte(replacement.old), []byte(replacement.next), 1)
			e.Payload = encoding.EncodeToString(payload)
			e.Signature = encoding.EncodeToString(ed25519.Sign(f.online, append([]byte(PayloadDomain), payload...)))
		})
	}
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
	data = append(data, '\n')
	path := filepath.Join("..", "..", "testdata", "network", "directory.json")
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
