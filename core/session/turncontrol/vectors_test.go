package turncontrol

import (
	"bytes"
	"encoding/json"
	"flag"
	"os"
	"path/filepath"
	"testing"
	"time"
)

var updateVectors = flag.Bool("update", false, "update TURN control contract vectors")

func TestTURNControlVectors(t *testing.T) {
	header := Header{SessionID: [16]byte{1}, OwnerPathID: [16]byte{2}, Sequence: 3}
	path := [16]byte{4}
	messages := []Message{
		Available{header, []string{"primary", "secondary"}},
		Available{header, []string{}},
		Demand{header, time.Minute},
		Grant{Authority: header, PathID: path, Replaces: [16]byte{5}, Provider: "coturn", Account: "home", Profile: "primary",
			CredentialKind: Expiring, Username: "123:user", Password: "delegated-password", ValidFor: time.Hour, URLs: []string{"turn:127.0.0.1:3478?transport=udp", "turns:example.org:443?transport=tcp"}},
		Grant{Authority: header, PathID: path, Provider: "standard", Account: "home", Profile: "primary", CredentialKind: Fixed, Username: "fixed-user", Password: "fixed-password", URLs: []string{"turn:example.org"}},
		Ready{header, path, Lane{ID: 7, Epoch: 2}}, Retire{header, path, Replaced}, Unavailable{header, "primary", time.Second, "provider unavailable"}, Report{header, path, 1 << 40},
		Recover{header, path, AllocationStateMismatch}, Recover{header, path, ChannelUnavailable},
	}
	type vector struct {
		Body []byte `json:"bodyB64"`
	}
	vectors := make([]vector, 0, len(messages))
	for _, message := range messages {
		body, err := Encode(message)
		if err != nil {
			t.Fatal(err)
		}
		vectors = append(vectors, vector{body})
	}
	encoded, err := json.MarshalIndent(vectors, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	encoded = append(encoded, '\n')
	destination := filepath.Join("..", "..", "testvectors", "turn-control-v4.json")
	if *updateVectors {
		if err := os.WriteFile(destination, encoded, 0644); err != nil {
			t.Fatal(err)
		}
	}
	existing, err := os.ReadFile(destination)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(existing, encoded) {
		t.Fatal("TURN control vectors changed; run make vectors-update")
	}
}
