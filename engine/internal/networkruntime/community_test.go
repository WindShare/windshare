package networkruntime

import (
	"bytes"
	"crypto/ed25519"
	"encoding/base64"
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"testing"
	"time"

	"github.com/windshare/windshare/connectivity/publicdial"
	"github.com/windshare/windshare/connectivity/relayplan"
	"github.com/windshare/windshare/core/diagnostics"
	"github.com/windshare/windshare/core/link"
	"github.com/windshare/windshare/core/serviceauthority"
	"github.com/windshare/windshare/engine/internal/community"
	"github.com/windshare/windshare/engine/internal/statefile"
	"github.com/windshare/windshare/engine/relayprofile"
)

func TestCachedCommunityPlanWithoutDirectoryDoesNotLeakTicketsOrColdHints(t *testing.T) {
	r := fixtureRuntime(t)
	p := filepath.Join(t.TempDir(), "config.json")
	data, err := os.ReadFile("../../../testdata/network/authority.json")
	if err != nil {
		t.Fatal(err)
	}
	var fixture struct{ Descriptor struct{ Descriptor string } }
	if err := json.Unmarshal(data, &fixture); err != nil {
		t.Fatal(err)
	}
	enc := base64.RawURLEncoding
	payload, err := enc.DecodeString(fixture.Descriptor.Descriptor)
	if err != nil {
		t.Fatal(err)
	}
	var d map[string]any
	if err := json.Unmarshal(payload, &d); err != nil {
		t.Fatal(err)
	}
	const peer = "https://cached.example"
	d["relay_endpoint"] = "wss://cached.example/v2/ws"
	d["authority"], d["platform"], d["issued_at"], d["expires_at"] = nil, "native", r.now().Unix(), r.now().Add(24*time.Hour).Unix()
	payload, err = json.Marshal(d)
	if err != nil {
		t.Fatal(err)
	}
	sig := ed25519.Sign(ed25519.NewKeyFromSeed(bytes.Repeat([]byte{43}, 32)), append([]byte(serviceauthority.GossipDomain), payload...))
	raw, err := json.Marshal([]any{map[string]any{"version": 2, "descriptor": enc.EncodeToString(payload), "signature": enc.EncodeToString(sig)}})
	if err != nil {
		t.Fatal(err)
	}
	if err := statefile.Write(t.Context(), filepath.Join(statefile.StateDirectory(p), "community-peers.json"), raw); err != nil {
		t.Fatal(err)
	}
	r.community = community.New(p, publicdial.Policy{}, r.now, diagnostics.Sink{})
	r.config.CommunitySettingsPath = p
	// A missing trust store has the same discovery boundary as an expired snapshot.
	r.directory.Close()
	r.directory = nil
	selection, err := r.Plan(t.Context(), relayprofile.Plan(nil), "")
	if err != nil || r.Unavailability() != nil || len(selection.Relays) != 1 {
		t.Fatal(selection, err, r.Unavailability())
	}
	reg := selection.Relays[0]
	if selection.Plan.Network[0].Source != relayplan.Community || reg.Authorization != nil || reg.Community == nil || reg.Community.Origin != r.config.LinkHost {
		t.Fatal(reg, selection.Plan)
	}
	capability := link.Link{}
	selection.PublishRelayHints(&capability, []string{peer})
	if len(capability.Relays) != 0 {
		t.Fatal("unverified community hint", capability)
	}
	reg.Community.Ready.Store(true)
	selection.PublishRelayHints(&capability, []string{peer})
	if !slices.Equal(capability.Relays, []string{peer}) {
		t.Fatal(capability)
	}
	if _, err := r.Plan(t.Context(), relayprofile.Plan(nil).WithoutCommunity(), ""); err == nil {
		t.Fatal("opt-out included community candidate")
	}
}
