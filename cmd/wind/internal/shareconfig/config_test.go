package shareconfig

import (
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"strings"
	"testing"
)

func TestDecodeRejectsIncompleteAmbiguousAndOversizedConfiguration(t *testing.T) {
	for _, input := range []string{
		"", "null", `{}`, `{"relays":[]}`, `{"relay":[]}`,
		`{"relays":[{"url":"https://relay.example","registration_token":"secret","typo":1}]}`,
		`{"relays":[{"url":"https://relay.example"}]} {}`,
		`{"relays":[{"url":"https://relay.example"},{"url":"wss://RELAY.example:443/"}]}`,
		`{"relays":[{"url":"https://relay.example?share_id=existing"}]}`,
		`{"relays":[{"url":"https://relay.example","registration_token":"secret\nheader"}]}`,
		`{"relays":[{"url":"https://relay.example"}]}` + strings.Repeat(" ", maxConfigBytes),
	} {
		if _, err := Decode(strings.NewReader(input)); !errors.Is(err, ErrConfig) {
			t.Fatalf("invalid configuration result=%v", err)
		}
	}
}

func TestSavedCandidatesAndExplicitSelectionKeepAuthorityBoundToAddress(t *testing.T) {
	saved, err := Decode(strings.NewReader(`{"relays":[
		{"url":"https://one.example?route=one","registration_token":"one-token"},
		{"url":"https://two.example","registration_token":"two-token"}
	]}`))
	if err != nil {
		t.Fatal(err)
	}
	selected, err := SelectRelays(saved.Relays, nil)
	if err != nil || !reflect.DeepEqual(selected, saved.Relays) {
		t.Fatal("saved selection was not preserved")
	}
	selected[0].RegistrationToken = "changed"
	if saved.Relays[0].RegistrationToken != "one-token" {
		t.Fatal("selection borrowed mutable caller storage")
	}
	selected, err = SelectRelays(saved.Relays, []string{"wss://TWO.example:443/", "https://other.example", "https://one.example?route=other"})
	if err != nil || len(selected) != 3 || selected[0].RegistrationToken != "two-token" ||
		selected[1].RegistrationToken != "" || selected[2].RegistrationToken != "" {
		t.Fatal("override moved registration authority across addresses")
	}
	selected, err = SelectRelays(nil, nil)
	if !errors.Is(err, ErrConfig) || len(selected) != 0 {
		t.Fatal("unconfigured relay silently fell back")
	}
	selected, err = SelectRelays(saved.Relays, []string{"https://two.example", "wss://two.example:443/"})
	if err != nil || len(selected) != 1 || selected[0].RegistrationToken != "two-token" {
		t.Fatal("equivalent explicit candidates were not deduplicated")
	}
	if _, err := SelectRelays(saved.Relays, []string{"invalid"}); err == nil {
		t.Fatal("invalid override accepted")
	}
}

func TestLoadDefaultsAndExplicitFileFailures(t *testing.T) {
	directory := t.TempDir()
	t.Setenv("APPDATA", directory)
	t.Setenv("XDG_CONFIG_HOME", directory)
	if runtime.GOOS == "darwin" {
		t.Setenv("HOME", directory)
	}
	t.Setenv(PathEnvironment, "")
	if err := os.Unsetenv(PathEnvironment); err != nil {
		t.Fatal(err)
	}
	if candidates, err := Load(); err != nil || len(candidates.Relays) != 0 {
		t.Fatalf("missing default=%v", err)
	}
	base, err := os.UserConfigDir()
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(base, "windshare", "config.json")
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(`{"relays":[{"url":"https://saved.example","registration_token":"saved-token"}]}`), 0o600); err != nil {
		t.Fatal(err)
	}
	for range 2 {
		candidates, err := Load()
		if err != nil || len(candidates.Relays) != 1 || candidates.Relays[0].RegistrationToken != "saved-token" {
			t.Fatal("saved configuration was not reusable")
		}
	}
	t.Setenv(PathEnvironment, filepath.Join(directory, "missing.json"))
	if _, err := Load(); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("explicit missing config silently fell back: %v", err)
	}
	t.Setenv(PathEnvironment, path)
	if err := os.WriteFile(path, []byte("corrupted"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := Load(); !errors.Is(err, ErrConfig) {
		t.Fatalf("corrupt config silently fell back: %v", err)
	}
}
