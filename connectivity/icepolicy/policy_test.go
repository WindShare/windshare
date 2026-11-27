package icepolicy

import (
	"encoding/json"
	"fmt"
	"os"
	"reflect"
	"slices"
	"testing"
	"time"

	"github.com/windshare/windshare/core/networkendpoint"
	"github.com/windshare/windshare/core/nodedirectory"
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
	facts.RecordProfile(profile, true, time.Now())
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
	if got := SelectAttemptProfile(DefaultPool(), selection(), GenerationFacts{}).URLs(); len(got) != 2 || got[0] == got[1] {
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
		store.RecordProfile(profile, true, time.Now())
		store.RecordProfile(profile, false, time.Now())
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
	store.SetGeneration("other")
	store.RecordProfile(profile, true, time.Now())
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

func TestPublicSTUNCandidateCatalogRemainsUnreviewed(t *testing.T) {
	data, err := os.ReadFile("../../deploy/network/public-stun-candidates.json")
	if err != nil {
		t.Fatal(err)
	}
	var entries []Endpoint
	if err := json.Unmarshal(data, &entries); err != nil {
		t.Fatal(err)
	}
	if len(entries) == 0 {
		t.Fatal("empty candidate catalog")
	}
	pool, err := ParseLocalConfiguration(data)
	if err != nil {
		t.Fatal(err)
	}
	seenURLs := make(map[string]bool, len(entries))
	for i, entry := range entries {
		canonical, err := networkendpoint.NormalizeSTUN(entry.URL)
		if err != nil || canonical != entry.URL {
			t.Fatalf("non-canonical STUN URL %q: %v", entry.URL, err)
		}
		if seenURLs[entry.URL] {
			t.Fatalf("duplicate STUN URL %q", entry.URL)
		}
		seenURLs[entry.URL] = true
		if i > 0 && entries[i-1].URL >= entry.URL {
			t.Fatalf("catalog not sorted at %q >= %q", entries[i-1].URL, entry.URL)
		}
		if entry.Trust != "unreviewed" || entry.Enabled {
			t.Fatalf("candidate %q must remain unreviewed and disabled", entry.ID)
		}
		if entry.Region != "" || entry.FailureDomain != "" || entry.Provider != "" {
			t.Fatalf("candidate %q carries unmeasured facts", entry.ID)
		}
	}
	if got := SelectAttemptProfile(pool, selection(), GenerationFacts{}).URLs(); len(got) != 0 {
		t.Fatalf("unreviewed catalog produced active URLs: %v", got)
	}
}

func TestDirectoryPoolPrioritizesDirectoryOverFallbacks(t *testing.T) {
	payload := nodedirectory.Payload{
		Nodes: []nodedirectory.Node{
			{Alias: "cn1", STUN: []string{"stun:cn1.example:3478"}, Roles: []string{"relay", "stun"}, Region: "cn-wuhan", Provider: "aliyun", Operator: "official", Status: nodedirectory.Active},
			{Alias: "us1", STUN: []string{"stun:us1.example:3478"}, Roles: []string{"relay", "stun"}, Region: "us-los-angeles", Provider: "quickpacket", Operator: "official", Status: nodedirectory.Draining},
			{Alias: "relay-only", STUN: []string{"stun:ignored.example:3478"}, Roles: []string{"relay"}, Region: "de-frankfurt", Provider: "other", Operator: "official", Status: nodedirectory.Active},
		},
		PublicSTUN: []nodedirectory.STUNEndpoint{
			{URL: CloudflareSTUNServer, Region: "global", Provider: "cloudflare", FailureDomain: "directory-cloudflare"},
			{URL: "stun:cn-public.example:3478", Region: "cn", Provider: "tencent", FailureDomain: "tencent-stun"},
		},
	}
	pool := DirectoryPool(payload)
	byURL := make(map[string]Endpoint, len(pool.endpoints))
	for _, endpoint := range pool.endpoints {
		byURL[endpoint.URL] = endpoint
	}
	if _, included := byURL["stun:us1.example:3478"]; !included {
		t.Fatal("relay drain removed healthy STUN")
	}
	if _, excluded := byURL["stun:ignored.example:3478"]; excluded {
		t.Fatal("non-stun node entered pool")
	}
	if got := byURL["stun:cn1.example:3478"].Priority; got != DirectoryNodeSTUNPriority {
		t.Fatalf("directory node priority = %d, want %d", got, DirectoryNodeSTUNPriority)
	}
	if got := byURL["stun:us1.example:3478"].Priority; got != DirectoryDrainingSTUNPriority {
		t.Fatalf("draining node priority = %d, want %d", got, DirectoryDrainingSTUNPriority)
	}
	if cf := byURL[CloudflareSTUNServer]; cf.Priority != DirectoryPublicSTUNPriority || cf.FailureDomain != "directory-cloudflare" || cf.Region != "global" {
		t.Fatalf("overlapping directory public_stun did not override fallback: %+v", cf)
	}
	if got := byURL[ExistingDefaultSTUNServer].Priority; got != FallbackSTUNPriority {
		t.Fatalf("fallback priority = %d, want %d", got, FallbackSTUNPriority)
	}
	for i := range 16 {
		request := selection()
		request.WaveID = fmt.Sprintf("wave-%d", i)
		primary := SelectAttemptProfile(pool, request, GenerationFacts{})
		urls := primary.URLs()
		if len(urls) != 2 || !((urls[0] == "stun:cn1.example:3478" && urls[1] == "stun:us1.example:3478") ||
			(urls[0] == "stun:us1.example:3478" && urls[1] == "stun:cn1.example:3478")) {
			t.Fatalf("wave %d cold-start primary = %v, want {cn1, us1}", i, urls)
		}
		request.Sequence++
		request.UsedEndpointIDs = primary.EndpointIDs()
		request.UsedFailureDomains = primary.FailureDomains()
		backup := SelectAttemptProfile(pool, request, GenerationFacts{})
		backupURLs := backup.URLs()
		if len(backupURLs) != 2 || !((backupURLs[0] == CloudflareSTUNServer && backupURLs[1] == "stun:cn-public.example:3478") ||
			(backupURLs[0] == "stun:cn-public.example:3478" && backupURLs[1] == CloudflareSTUNServer)) {
			t.Fatalf("wave %d backup = %v, want directory public_stun tier", i, backupURLs)
		}
	}
	// Runtime profile success still overrides static directory priority.
	facts := NewFactStore("network")
	request := selection()
	primary := SelectAttemptProfile(pool, request, GenerationFacts{})
	request.Sequence++
	request.UsedEndpointIDs = primary.EndpointIDs()
	request.UsedFailureDomains = primary.FailureDomains()
	backup := SelectAttemptProfile(pool, request, GenerationFacts{})
	facts.RecordProfile(backup, true, request.Now)
	promoted := SelectAttemptProfile(pool, selection(), facts.Snapshot())
	if !reflect.DeepEqual(promoted.URLs(), backup.URLs()) {
		t.Fatalf("runtime-successful backup was not promoted: got %v, want %v", promoted.URLs(), backup.URLs())
	}
}

func TestDirectoryPoolRanksActiveAheadOfCanaryAndDraining(t *testing.T) {
	threeNode := DirectoryPool(nodedirectory.Payload{
		Nodes: []nodedirectory.Node{
			{Alias: "cn1", STUN: []string{"stun:cn1.example:3478"}, Roles: []string{"relay", "stun"}, Region: "cn-wuhan", Provider: "aliyun", Operator: "official", Status: nodedirectory.Active},
			{Alias: "us1", STUN: []string{"stun:us1.example:3478"}, Roles: []string{"relay", "stun"}, Region: "us-los-angeles", Provider: "quickpacket", Operator: "official", Status: nodedirectory.Draining},
			{Alias: "eu1", STUN: []string{"stun:eu1.example:3478"}, Roles: []string{"relay", "stun"}, Region: "eu-frankfurt", Provider: "gtt", Operator: "official", Status: nodedirectory.Active},
		},
	})
	for i := range 16 {
		request := selection()
		request.WaveID = fmt.Sprintf("three-%d", i)
		primary := SelectAttemptProfile(threeNode, request, GenerationFacts{})
		urls := primary.URLs()
		if len(urls) != 2 || !((urls[0] == "stun:cn1.example:3478" && urls[1] == "stun:eu1.example:3478") ||
			(urls[0] == "stun:eu1.example:3478" && urls[1] == "stun:cn1.example:3478")) {
			t.Fatalf("wave %d primary = %v, want active pair {cn1, eu1}", i, urls)
		}
		request.Sequence++
		request.UsedEndpointIDs = primary.EndpointIDs()
		request.UsedFailureDomains = primary.FailureDomains()
		backup := SelectAttemptProfile(threeNode, request, GenerationFacts{})
		if got := backup.URLs(); len(got) != 2 || got[0] != "stun:us1.example:3478" {
			t.Fatalf("wave %d backup = %v, want draining us1 first before embedded fallback", i, got)
		}
	}

	mixed := DirectoryPool(nodedirectory.Payload{
		Nodes: []nodedirectory.Node{
			{Alias: "cn1", STUN: []string{"stun:cn1.example:3478"}, Roles: []string{"relay", "stun"}, Region: "cn-wuhan", Provider: "aliyun", Operator: "official", Status: nodedirectory.Active},
			{Alias: "jp1", STUN: []string{"stun:jp1.example:3478"}, Roles: []string{"relay", "stun"}, Region: "jp-tokyo", Provider: "vultr", Operator: "official", Status: nodedirectory.Canary},
			{Alias: "us1", STUN: []string{"stun:us1.example:3478"}, Roles: []string{"relay", "stun"}, Region: "us-los-angeles", Provider: "quickpacket", Operator: "official", Status: nodedirectory.Draining},
		},
		PublicSTUN: []nodedirectory.STUNEndpoint{
			{URL: "stun:cn-public.example:3478", Region: "cn", Provider: "tencent", FailureDomain: "tencent-stun"},
		},
	})
	byURL := make(map[string]Endpoint, len(mixed.endpoints))
	for _, endpoint := range mixed.endpoints {
		byURL[endpoint.URL] = endpoint
	}
	if byURL["stun:cn1.example:3478"].Priority != DirectoryNodeSTUNPriority ||
		byURL["stun:jp1.example:3478"].Priority != DirectoryCanarySTUNPriority ||
		byURL["stun:us1.example:3478"].Priority != DirectoryDrainingSTUNPriority ||
		byURL["stun:cn-public.example:3478"].Priority != DirectoryPublicSTUNPriority {
		t.Fatalf("unexpected priority tiers: %+v", byURL)
	}
	for i := range 16 {
		request := selection()
		request.WaveID = fmt.Sprintf("mixed-%d", i)
		primary := SelectAttemptProfile(mixed, request, GenerationFacts{})
		if got := primary.URLs(); !reflect.DeepEqual(got, []string{"stun:cn1.example:3478", "stun:jp1.example:3478"}) {
			t.Fatalf("wave %d primary = %v, want [cn1, jp1]", i, got)
		}
		request.Sequence++
		request.UsedEndpointIDs = primary.EndpointIDs()
		request.UsedFailureDomains = primary.FailureDomains()
		backup := SelectAttemptProfile(mixed, request, GenerationFacts{})
		if got := backup.URLs(); !reflect.DeepEqual(got, []string{"stun:us1.example:3478", "stun:cn-public.example:3478"}) {
			t.Fatalf("wave %d backup = %v, want [us1, cn-public]", i, got)
		}
	}
}

func TestCommunityTrustCannotDisplaceHigherTrustInEitherSlot(t *testing.T) {
	entry := func(id, trust, domain string, priority int) Endpoint {
		return Endpoint{ID: id, URL: "stun:" + id + ".example:3478", Family: "any", Trust: trust, Region: domain, FailureDomain: domain, Provider: domain, Enabled: true, Priority: priority}
	}
	pool, err := NewICEEndpointPool([]Endpoint{entry("official", "reviewed", "same", 1),
		entry("local", "local", "same", 2), entry("community", "community", "different", 1000)})
	if err != nil {
		t.Fatal(err)
	}
	req := selection()
	facts := GenerationFacts{NetworkGenerationID: req.NetworkGenerationID, Profiles: []ProfileFact{{EndpointIDs: []string{"community"}, ServerReflexiveProduced: true, ObservedAt: req.Now}}}
	if got := SelectAttemptProfile(pool, req, facts).EndpointIDs(); !slices.Equal(got, []string{"official", "local"}) {
		t.Fatal(got)
	}
	facts.Endpoints = []EndpointFact{{EndpointID: "local", FailedUntil: req.Now.Add(time.Minute)}}
	if got := SelectAttemptProfile(pool, req, facts).EndpointIDs(); !slices.Equal(got, []string{"official", "community"}) {
		t.Fatal(got)
	}
}

func TestWithCommunityFiltersInvalidAndCollidingEntriesIndividually(t *testing.T) {
	base := DefaultPool()
	baseCount := len(base.endpoints)
	valid1 := Endpoint{ID: "community:stun:198.51.100.1:3478", URL: "stun:198.51.100.1:3478", Family: "ipv4", Trust: "community", FailureDomain: "198.51.100.0/24", Enabled: true}
	valid2 := Endpoint{ID: "community:stun:[2001:db8::1]:3478", URL: "stun:[2001:db8::1]:3478", Family: "ipv6", Trust: "community", FailureDomain: "2001:db8::/48", Enabled: true}

	merged := base.WithCommunity([]Endpoint{
		valid1,
		{ID: "", URL: "stun:198.51.100.2:3478", Family: "ipv4", Trust: "community", Enabled: true},
		{ID: "shipped-google-stun", URL: "stun:198.51.100.3:3478", Family: "ipv4", Trust: "community", Enabled: true},
		{ID: valid1.ID, URL: "stun:198.51.100.4:3478", Family: "ipv4", Trust: "community", Enabled: true},
		{ID: "community:dup-base-url", URL: CloudflareSTUNServer, Family: "any", Trust: "community", Enabled: true},
		{ID: "community:dup-base-canonical-url", URL: "stun:STUN.CLOUDFLARE.COM:3478", Family: "any", Trust: "community", Enabled: true},
		{ID: "community:dup-batch-url", URL: valid1.URL, Family: "ipv4", Trust: "community", Enabled: true},
		{ID: "community:bad-url", URL: "turn:198.51.100.5:3478", Family: "ipv4", Trust: "community", Enabled: true},
		{ID: "community:bad-family", URL: "stun:198.51.100.6:3478", Family: "", Trust: "community", Enabled: true},
		{ID: "community:bad-priority", URL: "stun:198.51.100.7:3478", Family: "ipv4", Trust: "community", Priority: -1, Enabled: true},
		{ID: "community:wrong-trust", URL: "stun:198.51.100.8:3478", Family: "ipv4", Trust: "reviewed", Enabled: true},
		valid2,
	})

	if len(base.endpoints) != baseCount {
		t.Fatalf("base pool mutated: got %d entries, want %d", len(base.endpoints), baseCount)
	}
	if _, err := NewICEEndpointPool(merged.endpoints); err != nil {
		t.Fatalf("merged pool violates NewICEEndpointPool invariants: %v", err)
	}
	added := merged.endpoints[baseCount:]
	if len(added) != 2 || added[0].ID != valid1.ID || added[1].ID != valid2.ID {
		t.Fatalf("unexpected community entries appended: %+v", added)
	}

	// Empty or all-invalid batches leave the base pool unchanged.
	if got := base.WithCommunity(nil); len(got.endpoints) != baseCount {
		t.Fatalf("nil entries changed pool: %+v", got.endpoints)
	}
	if got := base.WithCommunity([]Endpoint{{ID: "", URL: "stun:198.51.100.9:3478", Family: "ipv4", Trust: "community", Enabled: true}}); len(got.endpoints) != baseCount {
		t.Fatalf("all-invalid entries changed pool: %+v", got.endpoints)
	}

	// Invalid and colliding entries do not consume remaining catalog capacity.
	nearFullEntries := make([]Endpoint, 0, MaximumCatalogEndpoints-1)
	for i := range MaximumCatalogEndpoints - 1 {
		nearFullEntries = append(nearFullEntries, Endpoint{
			ID: fmt.Sprintf("base-%d", i), URL: fmt.Sprintf("stun:node%d.example:3478", i),
			Family: "any", Trust: "reviewed", Enabled: true,
		})
	}
	nearFull, err := NewICEEndpointPool(nearFullEntries)
	if err != nil {
		t.Fatal(err)
	}
	fits := Endpoint{ID: "community:fits", URL: "stun:198.51.100.10:3478", Family: "ipv4", Trust: "community", Enabled: true}
	overflow := Endpoint{ID: "community:overflow", URL: "stun:198.51.100.11:3478", Family: "ipv4", Trust: "community", Enabled: true}
	capped := nearFull.WithCommunity([]Endpoint{
		{ID: "", URL: "stun:198.51.100.12:3478", Family: "ipv4", Trust: "community", Enabled: true},
		{ID: "base-0", URL: "stun:198.51.100.13:3478", Family: "ipv4", Trust: "community", Enabled: true},
		{ID: "community:dup-url", URL: "stun:node0.example:3478", Family: "ipv4", Trust: "community", Enabled: true},
		fits,
		overflow,
	})
	if len(capped.endpoints) != MaximumCatalogEndpoints || capped.endpoints[MaximumCatalogEndpoints-1].ID != fits.ID {
		t.Fatalf("catalog cap handling failed: len=%d last=%+v", len(capped.endpoints), capped.endpoints[len(capped.endpoints)-1])
	}
	if full := capped.WithCommunity([]Endpoint{overflow}); len(full.endpoints) != MaximumCatalogEndpoints {
		t.Fatalf("already-full pool exceeded cap: len=%d", len(full.endpoints))
	}
}
