package shareconfig

import (
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"strings"
	"testing"

	"github.com/windshare/windshare/connectivity/relayset"
	"github.com/windshare/windshare/connectivity/senderrelay"
)

func TestSaveReplacesEntireSelectionAndRetainsAddressBoundCredentials(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config", "config.json")
	t.Setenv(PathEnvironment, path)
	initial := []senderrelay.Candidate{
		{PublicURL: "https://public.example"},
		{PublicURL: "https://old-private.example", RegistrationToken: "old-token"},
	}
	if err := Save(Config{FrontURL: "https://old-front.example", Relays: initial}); err != nil {
		t.Fatal(err)
	}
	private := []senderrelay.Candidate{{
		PublicURL:         "https://private.example/base?route=one&region=two",
		RegistrationToken: strings.Repeat("<>&", senderrelay.MaxRegistrationTokenBytes/3),
	}}
	for range 2 {
		if err := Save(Config{FrontURL: "https://front.example/app", Relays: private}); err != nil {
			t.Fatal(err)
		}
		loaded, err := Load()
		if err != nil || !reflect.DeepEqual(loaded.Relays, private) || loaded.FrontURL != "https://front.example/app" {
			t.Fatal("saved selection or credential changed on import/re-import:", err)
		}
		selected, err := SelectRelays(loaded.Relays, nil)
		if err != nil || !reflect.DeepEqual(selected, private) {
			t.Fatal("import retained or added an unselected provider:", err)
		}
	}
	if err := Save(Config{Relays: private}); err != nil {
		t.Fatal(err)
	}
	loaded, err := Load()
	if err != nil || loaded.FrontURL != "" || !reflect.DeepEqual(loaded.Relays, private) {
		t.Fatal("relay-only replacement retained an unrelated frontend:", err)
	}
	entries, err := os.ReadDir(filepath.Dir(path))
	if err != nil || len(entries) != 1 || entries[0].Name() != filepath.Base(path) {
		t.Fatal("import left a staged credential file:", err)
	}
	if runtime.GOOS != "windows" {
		info, err := os.Stat(path)
		if err != nil || info.Mode().Perm() != privateFileMode {
			t.Fatal("configuration is not owner-only:", err)
		}
	}
}

func TestSaveRejectsInvalidSelectionBeforeTouchingDisk(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.json")
	t.Setenv(PathEnvironment, path)
	original := []byte("previous configuration bytes")
	if err := os.WriteFile(path, original, privateFileMode); err != nil {
		t.Fatal(err)
	}
	tooMany := make([]senderrelay.Candidate, relayset.MaximumEndpoints+1)
	for _, candidates := range [][]senderrelay.Candidate{
		nil,
		tooMany,
		{{PublicURL: "invalid"}},
		{{PublicURL: "https://relay.example"}, {PublicURL: "wss://RELAY.example:443/"}},
		{{PublicURL: "https://relay.example", RegistrationToken: "invalid\nheader"}},
		{{PublicURL: "https://relay.example/" + strings.Repeat("a", maxConfigBytes)}},
	} {
		if err := Save(Config{Relays: candidates}); !errors.Is(err, ErrConfig) {
			t.Fatal("invalid selection was not rejected:", err)
		}
		after, err := os.ReadFile(path)
		if err != nil || string(after) != string(original) {
			t.Fatal("rejected import changed saved configuration:", err)
		}
	}
}

func TestImportAtByteLimitCanBeSavedAndLoaded(t *testing.T) {
	prefix, suffix := `{"relays":[{"url":"https://relay.example/`, `"}]}`
	input := prefix + strings.Repeat("a", maxConfigBytes-len(prefix)-len(suffix)) + suffix
	candidates, err := Decode(strings.NewReader(input))
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv(PathEnvironment, filepath.Join(t.TempDir(), "config.json"))
	if err := Save(candidates); err != nil {
		t.Fatal("accepted configuration could not be saved:", err)
	}
	loaded, err := Load()
	if err != nil || !reflect.DeepEqual(loaded, candidates) {
		t.Fatal("accepted configuration could not be reloaded:", err)
	}
}

func TestSaveDestinationFailuresDoNotCreateStagedCredentials(t *testing.T) {
	root := t.TempDir()
	parentFile := filepath.Join(root, "file")
	if err := os.WriteFile(parentFile, []byte("keep"), privateFileMode); err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{"", root, filepath.Join(parentFile, "config.json")} {
		t.Setenv(PathEnvironment, path)
		if err := Save(Config{Relays: []senderrelay.Candidate{{PublicURL: "https://relay.example"}}}); err == nil {
			t.Fatal("invalid destination was accepted")
		}
	}
	entries, err := os.ReadDir(root)
	if err != nil || len(entries) != 1 || entries[0].Name() != "file" {
		t.Fatal("failed import left temporary state:", err)
	}
	if after, err := os.ReadFile(parentFile); err != nil || string(after) != "keep" {
		t.Fatal("failed import modified its parent file:", err)
	}
}

func TestSaveRefusesSymlinkDestination(t *testing.T) {
	root := t.TempDir()
	target := filepath.Join(root, "target.json")
	link := filepath.Join(root, "config.json")
	if err := os.WriteFile(target, []byte("keep"), privateFileMode); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, link); err != nil {
		if runtime.GOOS == "windows" {
			t.Skip("host does not permit symlink creation:", err)
		}
		t.Fatal(err)
	}
	t.Setenv(PathEnvironment, link)
	if err := Save(Config{Relays: []senderrelay.Candidate{{PublicURL: "https://relay.example"}}}); err == nil {
		t.Fatal("import replaced a symbolic destination")
	}
	if after, err := os.ReadFile(target); err != nil || string(after) != "keep" {
		t.Fatal("import modified the symbolic destination's target:", err)
	}
}
