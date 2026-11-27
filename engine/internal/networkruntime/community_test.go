package networkruntime

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"testing"
	"time"

	"github.com/windshare/windshare/connectivity/publicdial"
	"github.com/windshare/windshare/connectivity/relayplan"
	"github.com/windshare/windshare/connectivity/relayset"
	"github.com/windshare/windshare/core/diagnostics"
	"github.com/windshare/windshare/core/link"
	"github.com/windshare/windshare/core/serviceauthority"
	"github.com/windshare/windshare/engine/internal/community"
	"github.com/windshare/windshare/engine/internal/statefile"
	"github.com/windshare/windshare/engine/relayprofile"
)

type cachedCommunityPeer struct {
	host       string
	seedByte   byte
	capacityBS uint64
}

func writeCachedCommunityPool(t *testing.T, r *Runtime, peers ...cachedCommunityPeer) string {
	t.Helper()
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
	basePayload, err := enc.DecodeString(fixture.Descriptor.Descriptor)
	if err != nil {
		t.Fatal(err)
	}
	envelopes := make([]any, 0, len(peers))
	for _, peer := range peers {
		var d map[string]any
		if err := json.Unmarshal(basePayload, &d); err != nil {
			t.Fatal(err)
		}
		key := ed25519.NewKeyFromSeed(bytes.Repeat([]byte{peer.seedByte}, 32))
		pub := key.Public().(ed25519.PublicKey)
		digest := sha256.Sum256(append([]byte("windshare/node-identity/v1\x00"), pub...))
		d["node_key"] = enc.EncodeToString(pub)
		d["node_id"] = enc.EncodeToString(digest[:])
		d["relay_endpoint"] = "wss://" + peer.host + "/v2/ws"
		d["authority"], d["platform"], d["issued_at"], d["expires_at"] = nil, "native", r.now().Unix(), r.now().Add(24*time.Hour).Unix()
		if peer.capacityBS > 0 {
			d["public_capacity_bytes_per_second"] = peer.capacityBS
		}
		payload, err := json.Marshal(d)
		if err != nil {
			t.Fatal(err)
		}
		sig := ed25519.Sign(key, append([]byte(serviceauthority.GossipDomain), payload...))
		envelopes = append(envelopes, map[string]any{"version": 2, "descriptor": enc.EncodeToString(payload), "signature": enc.EncodeToString(sig)})
	}
	raw, err := json.Marshal(envelopes)
	if err != nil {
		t.Fatal(err)
	}
	if err := statefile.Write(t.Context(), filepath.Join(statefile.StateDirectory(p), "community-peers.json"), raw); err != nil {
		t.Fatal(err)
	}
	r.community = community.New(p, publicdial.Policy{}, r.now, diagnostics.Sink{})
	r.config.CommunitySettingsPath = p
	return p
}

func TestCachedCommunityPlanWithoutDirectoryDoesNotLeakTicketsOrColdHints(t *testing.T) {
	r := fixtureRuntime(t)
	const peer = "https://cached.example"
	writeCachedCommunityPool(t, r, cachedCommunityPeer{host: "cached.example", seedByte: 43})
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

func TestCommunityObservationDiagnosticsAndFirstFailureDemotion(t *testing.T) {
	r := fixtureRuntime(t)
	writeCachedCommunityPool(t, r,
		cachedCommunityPeer{host: "a-high.example", seedByte: 44, capacityBS: 12_500_000},
		cachedCommunityPeer{host: "b-default.example", seedByte: 45, capacityBS: 3_125_000},
	)
	r.directory.Close()
	r.directory = nil

	type diagnosticCandidate struct {
		Endpoint            string `json:"endpoint"`
		Observation         string `json:"observation"`
		ObservedHandshakeMS string `json:"observed_handshake_ms"`
	}
	readDiagnostics := func(sel Selection) []diagnosticCandidate {
		t.Helper()
		raw, err := sel.DiagnosticRecord().Payload().Get("initial_candidates").MarshalJSON()
		if err != nil {
			t.Fatal(err)
		}
		var out []diagnosticCandidate
		if err := json.Unmarshal(raw, &out); err != nil {
			t.Fatal(err)
		}
		return out
	}

	const highURL, defaultURL = "https://a-high.example", "https://b-default.example"
	initial, err := r.Plan(t.Context(), relayprofile.Plan(nil), "")
	if err != nil || len(initial.Plan.Network) != 2 || initial.Plan.Network[0].URL != highURL {
		t.Fatalf("initial community plan = (%+v, %v)", initial.Plan, err)
	}

	observe := r.Observer(initial, diagnostics.Sink{})
	observe(highURL, relayset.Attempt{State: relayset.AttemptStarted})
	observe(highURL, relayset.Attempt{State: relayset.AttemptSucceeded, HandshakeDuration: 145 * time.Millisecond})

	afterAdmit, err := r.Plan(t.Context(), relayprofile.Plan(nil), "")
	if err != nil {
		t.Fatal(err)
	}
	got := readDiagnostics(afterAdmit)
	if len(got) != 2 || got[0].Endpoint != highURL || got[0].Observation != "admitted" || got[0].ObservedHandshakeMS != "145" {
		t.Fatalf("admitted community diagnostics = %+v", got)
	}

	// First transport failure records Admitted=false with CooldownUntil=0; the failed high-capacity
	// peer must drop behind the unmeasured peer and report observation="failed".
	observe(highURL, relayset.Attempt{State: relayset.AttemptStarted})
	observe(highURL, relayset.Attempt{State: relayset.AttemptFailed, Failure: errors.New("dial tcp timeout")})

	afterFail, err := r.Plan(t.Context(), relayprofile.Plan(nil), "")
	if err != nil {
		t.Fatal(err)
	}
	got = readDiagnostics(afterFail)
	if len(got) != 2 || got[0].Endpoint != defaultURL || got[0].Observation != "unmeasured" ||
		got[1].Endpoint != highURL || got[1].Observation != "failed" || got[1].ObservedHandshakeMS != "" {
		t.Fatalf("post-failure community diagnostics = %+v", got)
	}
}

func TestCommunityReadsStayLockFreeAndPlanHonorsCancelOnPendingRecovery(t *testing.T) {
	r := fixtureRuntime(t)
	p := writeCachedCommunityPool(t, r, cachedCommunityPeer{host: "lockfree.example", seedByte: 46})
	r.directory.Close()
	r.directory = nil
	if err := statefile.Write(t.Context(), p, []byte(`{"profiles":[]}`)); err != nil {
		t.Fatal(err)
	}

	// Ordinary Plan, Unavailability, and ICEPool reads must not create config.json.lock.
	if selection, err := r.Plan(t.Context(), relayprofile.Plan(nil), ""); err != nil || len(selection.Relays) != 1 {
		t.Fatalf("lock-free Plan=(%+v, %v)", selection, err)
	}
	if failure := r.Unavailability(); failure != nil {
		t.Fatalf("lock-free Unavailability=%v", failure)
	}
	_ = r.ICEPool()
	if _, err := os.Stat(p + ".lock"); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("ordinary community reads created lock file: err=%v", err)
	}

	// Placing an unwritable barrier at config.json.lock simulates read-only lock failure.
	if err := os.Mkdir(p+".lock", 0o700); err != nil {
		t.Fatal(err)
	}
	if _, allowed := r.communitySettings(t.Context()); !allowed {
		t.Fatal("read-only lock barrier disabled communitySettings")
	}
	if selection, err := r.Plan(t.Context(), relayprofile.Plan(nil), ""); err != nil || len(selection.Relays) != 1 {
		t.Fatalf("read-only Plan=(%+v, %v)", selection, err)
	}
	if failure := r.Unavailability(); failure != nil {
		t.Fatalf("read-only Unavailability=%v", failure)
	}
	_ = r.ICEPool()
	if err := os.Remove(p + ".lock"); err != nil {
		t.Fatal(err)
	}

	// Under external lock contention with no commit.json, reads still proceed without blocking.
	locked := make(chan struct{})
	release := make(chan struct{})
	done := make(chan error, 1)
	go func() {
		done <- statefile.WithLock(t.Context(), p, func() error {
			close(locked)
			<-release
			return nil
		})
	}()
	<-locked
	if selection, err := r.Plan(t.Context(), relayprofile.Plan(nil), ""); err != nil || len(selection.Relays) != 1 {
		close(release)
		t.Fatalf("contended lock-free Plan=(%+v, %v)", selection, err)
	}
	if failure := r.Unavailability(); failure != nil {
		close(release)
		t.Fatalf("contended lock-free Unavailability=%v", failure)
	}
	_ = r.ICEPool()

	// When commit.json recovery is pending under lock contention, Plan honors caller ctx cancellation.
	commitPayload := `[{"name":"settings","data":"eyJwcm9maWxlcyI6W119"},{"name":"community-eligibility.json","data":"e30="}]`
	if err := statefile.Write(t.Context(), filepath.Join(statefile.StateDirectory(p), "commit.json"), []byte(commitPayload)); err != nil {
		close(release)
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Millisecond)
	defer cancel()
	if _, err := r.Plan(ctx, relayprofile.Plan(nil), ""); !errors.Is(err, context.DeadlineExceeded) {
		close(release)
		t.Fatalf("contended recovery Plan err=%v, want context.DeadlineExceeded", err)
	}
	close(release)
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	if selection, err := r.Plan(t.Context(), relayprofile.Plan(nil), ""); err != nil || len(selection.Relays) != 1 {
		t.Fatalf("post-recovery Plan=(%+v, %v)", selection, err)
	}
}
