package relayconfig

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/windshare/windshare/core/networkendpoint"
	"github.com/windshare/windshare/engine/relayprofile"
)

func TestSavedWebSocketMountDoesNotBlockOrLendCredentialsToAnOverride(t *testing.T) {
	document := `{"profiles":[{"url":"https://relay.example/v2/ws","access_key":"nested"},{"url":"https://relay.example","access_key":"root"}],"active_relays":["https://relay.example/v2/ws"]}`
	config, err := Decode(strings.NewReader(document))
	if err != nil {
		t.Fatal(err)
	}
	for _, test := range []struct {
		override []string
		key      string
	}{{nil, "nested"}, {[]string{"https://relay.example"}, "root"}} {
		selected, err := SelectRelays(config, test.override)
		if err != nil || len(selected) != 1 || selected[0].AccessKey != test.key {
			t.Fatalf("selection %v: %v, %v", test.override, selected, err)
		}
	}
}

func TestRelayAddressDiagnosticsPreserveCauseAndConfigurationField(t *testing.T) {
	const host = "relay.123"
	for _, test := range []struct {
		name, document, field string
	}{
		{name: "explicit override"},
		{name: "inactive profile", document: `{"profiles":[{"url":"https://good.example"},{"url":"https://relay.123?token=secret-canary","access_key":"secret-canary"}],"active_relays":["https://good.example"]}`, field: "profiles[1].url"},
		{name: "active selection", document: `{"profiles":[{"url":"https://good.example"}],"active_relays":["https://relay.123"]}`, field: "active_relays[0]"},
	} {
		t.Run(test.name, func(t *testing.T) {
			var err error
			if test.document == "" {
				_, err = SelectRelays(Config{}, []string{"https://" + host})
			} else {
				_, err = Decode(strings.NewReader(test.document))
			}
			failure, ok := errors.AsType[*networkendpoint.NumericHostError](err)
			if !ok || failure.Host != host || !errors.Is(err, ErrRelayURL) || !errors.Is(err, networkendpoint.ErrInvalid) {
				t.Fatalf("address rejection lost its domain cause: %v", err)
			}
			if errors.Is(err, ErrConfig) != (test.document != "") {
				t.Fatalf("configuration category does not match the input source: %v", err)
			}
			for _, detail := range []string{host, test.field, "canonical IP address"} {
				if !strings.Contains(err.Error(), detail) {
					t.Errorf("repair detail %q missing: %v", detail, err)
				}
			}
			if strings.Contains(err.Error(), "secret-canary") || strings.Contains(err.Error(), "TURN") {
				t.Fatalf("address diagnostic includes unrelated configuration: %v", err)
			}
		})
	}
}

func TestSavedRelayAddressDiagnosticsIdentifyTheRepairFile(t *testing.T) {
	directory := t.TempDir()
	t.Chdir(directory)
	store := FileStore{Path: "config.json"}
	input := []byte(`{"profiles":[{"url":"https://relay.0x10"}]}`)
	if err := os.WriteFile(store.Path, input, 0o600); err != nil {
		t.Fatal(err)
	}
	_, loadErr := store.Load()
	_, updateErr := store.Update(context.Background(), func(*Config) error {
		t.Fatal("invalid settings reached the edit callback")
		return nil
	})
	for _, err := range []error{loadErr, updateErr} {
		if !errors.Is(err, ErrRelayURL) || !errors.Is(err, ErrConfig) {
			t.Fatalf("saved address lost its error categories: %v", err)
		}
		for _, detail := range []string{fmt.Sprintf("%+q", filepath.Join(directory, store.Path)), "profiles[0].url", "relay.0x10"} {
			if !strings.Contains(err.Error(), detail) {
				t.Errorf("repair detail %q missing: %v", detail, err)
			}
		}
	}
	after, err := os.ReadFile(store.Path)
	if err != nil || string(after) != string(input) {
		t.Fatal("rejection changed the saved configuration:", err)
	}
	if err := store.Save(Config{Profiles: []relayprofile.Candidate{{PublicURL: "https://relay.example"}}}); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Load(); err != nil {
		t.Fatal("corrected replacement did not restore loading:", err)
	}
}
