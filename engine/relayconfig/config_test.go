package relayconfig

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
		"", "null", `{}`, `{"relay":[]}`,
		`{"profiles":[{"url":"https://relay.example","access_key":"secret","typo":1}]}`,
		`{"profiles":[{"url":"https://relay.example"}]} {}`,
		`{"profiles":[{"url":"https://relay.example"},{"url":"wss://RELAY.example:443/"}]}`,
		`{"profiles":[{"url":"https://relay.example?share_id=existing"}]}`,
		`{"profiles":[{"url":"https://relay.example","access_key":"secret\nheader"}]}`,
		`{"profiles":[{"url":"https://relay.example"}]}` + strings.Repeat(" ", maxConfigBytes),
	} {
		if _, err := Decode(strings.NewReader(input)); !errors.Is(err, ErrConfig) {
			t.Fatalf("invalid configuration result=%v", err)
		}
	}
}

func TestSavedCandidatesAndExplicitSelectionKeepAuthorityBoundToAddress(t *testing.T) {
	saved, err := Decode(strings.NewReader(`{"profiles":[
		{"url":"https://one.example?route=one","access_key":"one-token"},
		{"url":"https://two.example","access_key":"two-token"}
	]}`))
	if err != nil {
		t.Fatal(err)
	}
	saved.ActiveRelays = []string{"https://one.example?route=one", "https://two.example"}
	selected, err := SelectRelays(saved, nil)
	if err != nil || !reflect.DeepEqual(selected, saved.Profiles) {
		t.Fatal("saved selection was not preserved")
	}
	selected[0].AccessKey = "changed"
	if saved.Profiles[0].AccessKey != "one-token" {
		t.Fatal("selection borrowed mutable caller storage")
	}
	selected, err = SelectRelays(saved, []string{"wss://TWO.example:443/", "https://other.example", "https://one.example?route=other"})
	if err != nil || len(selected) != 3 || selected[0].AccessKey != "two-token" ||
		selected[1].AccessKey != "" || selected[2].AccessKey != "" {
		t.Fatal("override moved registration authority across addresses")
	}
	selected, err = SelectRelays(Config{}, nil)
	if !errors.Is(err, ErrConfig) || len(selected) != 0 {
		t.Fatal("unconfigured relay silently fell back")
	}
	selected, err = SelectRelays(saved, []string{"https://two.example", "wss://two.example:443/"})
	if err != nil || len(selected) != 1 || selected[0].AccessKey != "two-token" {
		t.Fatal("equivalent explicit candidates were not deduplicated")
	}
	if _, err := SelectRelays(saved, []string{"invalid"}); err == nil {
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
	if candidates, err := Load(); err != nil || len(candidates.Profiles) != 0 {
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
	if err := os.WriteFile(path, []byte(`{"profiles":[{"url":"https://saved.example","access_key":"saved-token"}]}`), 0o600); err != nil {
		t.Fatal(err)
	}
	for range 2 {
		candidates, err := Load()
		if err != nil || len(candidates.Profiles) != 1 || candidates.Profiles[0].AccessKey != "saved-token" {
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
