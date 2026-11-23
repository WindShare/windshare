package relayconfig

import (
	"errors"
	"fmt"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/windshare/windshare/engine/relayprofile"
)

func TestTURNConfigurationSurvivesSaveAndShareSelection(t *testing.T) {
	store := FileStore{Path: filepath.Join(t.TempDir(), "config.json")}
	services := []TURNService{
		{ID: "office", Kind: TURNCoturn, URLs: []string{"turn:office.example"}, SharedSecret: "secret", TTLSeconds: 3600},
		{ID: "managed", Kind: TURNCloudflare, KeyID: "Provider_key-1", APIToken: "token"},
		{ID: "backup", Kind: TURNStatic, Role: RoleFallback, URLs: []string{"turns:backup.example:443"}, Username: "user", Password: "password"},
	}
	relay := relayprofile.Candidate{PublicURL: "https://private.example", AccessKey: "device-key", ManagedTurn: true}
	config := Config{TURNServices: services, Profiles: []relayprofile.Candidate{relay}, ActiveRelays: []string{relay.PublicURL}}
	if err := store.Save(config); err != nil {
		t.Fatal(err)
	}
	loaded, err := store.Load()
	if err != nil || !reflect.DeepEqual(loaded, config) {
		t.Fatalf("TURN settings changed on disk: %v", err)
	}
	intent, err := ShareIntent(loaded, nil)
	if err != nil || !reflect.DeepEqual(intent.Candidates(), config.Profiles) {
		t.Fatalf("share lost the managed profile or credential: %v", err)
	}
	for i, want := range []string{RoleParallel, RoleFallback, RoleFallback} {
		if got := loaded.TURNServices[i].EffectiveRole(); got != want {
			t.Fatalf("role for %s = %s, want %s", services[i].ID, got, want)
		}
	}
}

func TestCloudflareKeyIDIsOneBoundedPathSegment(t *testing.T) {
	service := TURNService{ID: "cloud", Kind: TURNCloudflare, APIToken: "token"}
	for _, id := range []string{"short", "Provider_key-9", strings.Repeat("a", 32)} {
		service.KeyID = id
		if err := ValidateTURNServices([]TURNService{service}); err != nil {
			t.Fatalf("valid key ID %q: %v", id, err)
		}
	}
	for _, id := range []string{"", ".", "..", "a/b", "a%2Fb", "x?y", "x#y", `a\b`, "a b", "x\ny", "x\x00y", strings.Repeat("a", 33)} {
		service.KeyID = id
		if !errors.Is(ValidateTURNServices([]TURNService{service}), ErrConfig) {
			t.Fatalf("accepted invalid key ID %q", id)
		}
	}
}

func TestTURNValidationEnforcesServiceLimitsAndProviderFields(t *testing.T) {
	cloud := TURNService{ID: "cloud", Kind: TURNCloudflare, KeyID: "key", APIToken: "token"}
	for _, change := range []func(*TURNService){
		func(s *TURNService) { s.ID = "" },
		func(s *TURNService) { s.Role = "other" },
		func(s *TURNService) { s.Kind = "other" },
		func(s *TURNService) { s.URLs = []string{"stun:wrong.example"} },
		func(s *TURNService) { s.TTLSeconds = MinimumTURNTTL - 1 },
		func(s *TURNService) { s.TTLSeconds = MaximumTURNTTL + 1 },
		func(s *TURNService) { s.Username = "unrelated" },
		func(s *TURNService) { s.APIToken = "" },
	} {
		invalid := cloud
		change(&invalid)
		if !errors.Is(ValidateTURNServices([]TURNService{invalid}), ErrConfig) {
			t.Fatal("accepted invalid TURN service fields")
		}
	}
	if !errors.Is(ValidateTURNServices([]TURNService{cloud, cloud}), ErrConfig) {
		t.Fatal("accepted duplicate service IDs")
	}
	var services []TURNService
	for i := range MaxTURNServices + 1 {
		s := cloud
		s.ID = fmt.Sprint(i)
		services = append(services, s)
	}
	if ValidateTURNServices(services[:MaxTURNServices]) != nil || !errors.Is(ValidateTURNServices(services), ErrConfig) {
		t.Fatal("incorrect service count limit")
	}
}
