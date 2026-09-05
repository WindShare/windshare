package icepolicy

import (
	"encoding/json"
	"fmt"
	"os"
	"reflect"
	"testing"
	"time"
)

func testPool(t *testing.T) ICEEndpointPool {
	t.Helper()
	var entries []Endpoint
	for i := range 6 {
		entries = append(entries, Endpoint{ID: fmt.Sprint(i), URL: fmt.Sprintf("stun:node%d.test:3478", i), Region: fmt.Sprint(i % 2), FailureDomain: fmt.Sprint(i), Family: "any", Trust: "reviewed", Priority: 6 - i, Enabled: true})
	}
	pool, err := NewICEEndpointPool(entries)
	if err != nil {
		t.Fatal(err)
	}
	return pool
}
func selection() SelectionRequest {
	return SelectionRequest{NetworkGenerationID: "network", WaveID: "wave", Sequence: 1, Now: time.Unix(100, 0), IPv4: true, IPv6: true}
}
func TestProfilesBoundTrustDiversityAndGeneration(t *testing.T) {
	pool := testPool(t)
	request := selection()
	profile := SelectAttemptProfile(pool, request, GenerationFacts{})
	if got := profile.EndpointIDs(); !reflect.DeepEqual(got, []string{"0", "1"}) {
		t.Fatal(got)
	}
	if profile.NetworkGenerationID() != "network" || profile.ID() == "" {
		t.Fatal(profile)
	}
	urls := profile.URLs()
	urls[0] = "mutated"
	if profile.URLs()[0] == "mutated" {
		t.Fatal("mutable profile")
	}
	request.UsedEndpointIDs = profile.EndpointIDs()
	request.UsedFailureDomains = profile.FailureDomains()
	request.Sequence++
	backup := SelectAttemptProfile(pool, request, GenerationFacts{})
	if !reflect.DeepEqual(backup.EndpointIDs(), []string{"2", "3"}) {
		t.Fatal(backup.EndpointIDs())
	}
	request.UsedEndpointIDs = append(request.UsedEndpointIDs, backup.EndpointIDs()...)
	if len(SelectAttemptProfile(pool, request, GenerationFacts{}).URLs()) != 0 {
		t.Fatal("wave exceeded four endpoints")
	}
	facts := NewFactStore("network")
	if facts.RecordEndpointFailure(profile, "unattributed", request.Now) {
		t.Fatal("guessed endpoint")
	}
	if !facts.RecordEndpointFailure(profile, "0", request.Now) {
		t.Fatal("missing fact")
	}
	request = selection()
	if got := SelectAttemptProfile(pool, request, facts.Snapshot()).EndpointIDs(); got[0] != "1" {
		t.Fatal(got)
	}
	facts.RecordProfile(profile, true, time.Millisecond)
	if len(facts.Snapshot().Profiles) != 1 {
		t.Fatal("missing profile fact")
	}
	facts.SetGeneration("next")
	if len(facts.Snapshot().Endpoints) != 0 || len(facts.Snapshot().Profiles) != 0 || facts.RecordEndpointFailure(profile, "0", request.Now) {
		t.Fatal("stale generation")
	}
	if got := SelectAttemptProfile(pool, request, facts.Snapshot()).EndpointIDs(); got[0] != "0" {
		t.Fatal(got)
	}
	request.IPv4 = false
	request.IPv6 = false
	if len(SelectAttemptProfile(pool, request, GenerationFacts{}).URLs()) != 0 {
		t.Fatal("incompatible")
	}
}
func TestCatalogConfiguration(t *testing.T) {
	base := Endpoint{ID: "node", URL: "stun:node.test:3478", Family: "any", Trust: "local", Enabled: true}
	for _, url := range []string{"turn:node:3478", "stun:node", "stun:user@node:3478", "stun:node:0", "stun:node:65536", "stun:node:abc", "stun://node:3478"} {
		entry := base
		entry.URL = url
		if _, err := NewICEEndpointPool([]Endpoint{entry}); err == nil {
			t.Fatal(url)
		}
	}
	for _, change := range []func(*Endpoint){func(e *Endpoint) { e.ID = "" }, func(e *Endpoint) { e.Family = "other" }, func(e *Endpoint) { e.Trust = "unreviewed" }, func(e *Endpoint) { e.Priority = -1 }} {
		entry := base
		change(&entry)
		if _, err := NewICEEndpointPool([]Endpoint{entry}); err == nil {
			t.Fatal(entry)
		}
	}
	if _, err := NewICEEndpointPool([]Endpoint{base, base}); err == nil {
		t.Fatal("duplicate")
	}
	if _, err := NewICEEndpointPool(make([]Endpoint, MaximumCatalogEndpoints+1)); err == nil {
		t.Fatal("unbounded")
	}
	if _, err := ParseLocalConfiguration([]byte("{")); err == nil {
		t.Fatal("malformed")
	}
	data, _ := json.Marshal([]Endpoint{base})
	pool, err := ParseLocalConfiguration(data)
	if err != nil {
		t.Fatal(err)
	}
	if len(SelectAttemptProfile(pool, selection(), GenerationFacts{}).URLs()) != 1 {
		t.Fatal("local configuration")
	}
	if got := SelectAttemptProfile(DefaultPool(), selection(), GenerationFacts{}).URLs(); len(got) != 1 || got[0] != ExistingDefaultSTUNServer {
		t.Fatal(got)
	}
	base.Enabled = false
	pool, _ = NewICEEndpointPool([]Endpoint{base})
	if len(SelectAttemptProfile(pool, selection(), GenerationFacts{}).URLs()) != 0 {
		t.Fatal("disabled endpoint")
	}
}
func TestSharedCandidateVectorsAndLateReservations(t *testing.T) {
	data, err := os.ReadFile("../../testdata/ice-policy/candidates.json")
	if err != nil {
		t.Fatal(err)
	}
	var vectors []struct{ Candidate, Class, Reason string }
	if err = json.Unmarshal(data, &vectors); err != nil {
		t.Fatal(err)
	}
	budget := NewCandidateBudget(0)
	for _, vector := range vectors {
		decision := budget.Accept(vector.Candidate)
		if decision.Reason != vector.Reason || decision.Class != vector.Class || decision.Accepted != (vector.Reason == "accepted") {
			t.Fatalf("%s: %+v", vector.Candidate, decision)
		}
	}
	flood := NewCandidateBudget(12)
	for i := range 4 {
		if !flood.Accept(fmt.Sprintf("candidate:%d 1 udp 100 192.168.1.2 %d typ host", i, 5000+i)).Accepted {
			t.Fatal("initial LAN")
		}
	}
	if decision := flood.Accept("candidate:5 1 udp 100 192.168.1.2 5005 typ host"); decision.Reason != "reserved" {
		t.Fatal(decision)
	}
	for _, candidate := range []string{"candidate:6 1 udp 100 2001:db8::1 5000 typ host", "candidate:7 1 udp 100 192.0.2.1 5000 typ srflx", "candidate:8 1 tcp 100 192.0.2.1 5000 typ host tcptype passive"} {
		if !flood.Accept(candidate).Accepted {
			t.Fatal("late candidate starved")
		}
	}
	small := NewCandidateBudget(1)
	if !small.Accept(vectors[0].Candidate).Accepted || small.Accept(vectors[2].Candidate).Reason != "budget" {
		t.Fatal("local hard cap")
	}
}
func TestFactStoreBoundedAndSnapshotsDetached(t *testing.T) {
	store := NewFactStore("network")
	store.SetGeneration("network")
	pool := testPool(t)
	request := selection()
	for i := range MaximumProfileFacts + 1 {
		request.Sequence = uint64(i)
		profile := SelectAttemptProfile(pool, request, GenerationFacts{})
		store.RecordProfile(profile, true, time.Millisecond)
		store.RecordProfile(profile, false, 2*time.Millisecond)
	}
	snapshot := store.Snapshot()
	if len(snapshot.Profiles) != MaximumProfileFacts {
		t.Fatal(len(snapshot.Profiles))
	}
	snapshot.Profiles[0].ProfileID = "mutated"
	if store.Snapshot().Profiles[0].ProfileID == "mutated" {
		t.Fatal("snapshot mutable")
	}
	profile := SelectAttemptProfile(pool, request, GenerationFacts{})
	store.RecordProfile(profile, true, -1)
	store.SetGeneration("other")
	store.RecordProfile(profile, true, 1)
	if len(store.Snapshot().Profiles) != 0 {
		t.Fatal("stale profile")
	}
}

func TestOrthogonalFamilyAndMappedReservations(t *testing.T) {
	budget := NewCandidateBudget(12)
	for i := range 4 {
		if !budget.Accept(fmt.Sprintf("candidate:1 1 udp 100 192.168.1.2 %d typ host", 5000+i)).Accepted {
			t.Fatal("initial host")
		}
	}
	if !budget.Accept("candidate:2 1 udp 100 fd00::1 5000 typ host").Accepted {
		t.Fatal("IPv6 LAN crowded out by IPv4 LAN")
	}
	if decision := budget.AcceptMapped("candidate:3 1 udp 100 192.0.2.55 6000 typ srflx"); !decision.Accepted || decision.Class != "mapped" {
		t.Fatal(decision)
	}
	if decision := budget.Accept("candidate:4 1 udp 200 192.0.2.55 6000 typ srflx"); decision.Reason != "duplicate" {
		t.Fatal("origin changed path identity", decision)
	}
}

func TestSharedProfileIdentityVector(t *testing.T) {
	data, err := os.ReadFile("../../testdata/ice-policy/selection.json")
	if err != nil {
		t.Fatal(err)
	}
	var vector struct {
		NetworkGenerationID, WaveID, ProfileID string
		Sequence                               uint64
		EndpointIDs                            []string
	}
	if err = json.Unmarshal(data, &vector); err != nil {
		t.Fatal(err)
	}
	request := selection()
	request.NetworkGenerationID = vector.NetworkGenerationID
	request.WaveID = vector.WaveID
	request.Sequence = vector.Sequence
	profile := SelectAttemptProfile(testPool(t), request, GenerationFacts{})
	if profile.ID() != vector.ProfileID || !reflect.DeepEqual(profile.EndpointIDs(), vector.EndpointIDs) {
		t.Fatal(profile.ID(), profile.EndpointIDs())
	}
}
