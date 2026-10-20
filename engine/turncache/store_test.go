package turncache

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/windshare/windshare/connectivity/senderrelay"
	"github.com/windshare/windshare/engine/relayconfig"
	"github.com/windshare/windshare/engine/turnprovider"
)

func TestCacheIsScopedToAuthorityAndRejectsOlderObservations(t *testing.T) {
	store := FileStore{Settings: relayconfig.FileStore{Path: filepath.Join(t.TempDir(), "config.json")}}
	candidate := senderrelay.Candidate{PublicURL: "https://relay.example", AccessKey: "first-key"}
	at := time.Unix(100, 0)
	enabled := turnprovider.Capability{ServiceID: "cf-service", Enabled: true}
	if cached, err := store.Load(candidate); err != nil || cached != nil {
		t.Fatal(cached, err)
	}
	if err := store.Save(t.Context(), candidate, enabled, at); err != nil {
		t.Fatal(err)
	}
	equivalent := candidate
	equivalent.PublicURL = "wss://RELAY.example:443/"
	if cached, err := store.Load(equivalent); err != nil || cached == nil || *cached != enabled {
		t.Fatal(cached, err)
	}
	replacement := candidate
	replacement.AccessKey = "replacement"
	if cached, err := store.Load(replacement); err != nil || cached != nil {
		t.Fatal("cache moved authority", cached, err)
	}
	disabled := enabled
	disabled.Enabled = false
	if err := store.Save(t.Context(), candidate, disabled, at.Add(time.Minute)); err != nil {
		t.Fatal(err)
	}
	if err := store.Save(t.Context(), candidate, enabled, at); err != nil {
		t.Fatal(err)
	}
	if cached, err := store.Load(candidate); err != nil || cached == nil || cached.Enabled {
		t.Fatal("late response resurrected withdrawal", cached, err)
	}
	if _, err := os.Stat(store.Settings.Path); !os.IsNotExist(err) {
		t.Fatal("cache wrote settings", err)
	}
}

func TestCorruptCacheCanBeRebuiltWithoutChangingSettings(t *testing.T) {
	settings := relayconfig.FileStore{Path: filepath.Join(t.TempDir(), "config.json")}
	candidate := senderrelay.Candidate{PublicURL: "https://relay.example", AccessKey: "key"}
	config := relayconfig.Config{Profiles: []senderrelay.Candidate{candidate}}
	if err := settings.Save(config); err != nil {
		t.Fatal(err)
	}
	before, _ := os.ReadFile(settings.Path)
	store := FileStore{Settings: settings}
	capability := turnprovider.Capability{ServiceID: "cf-service", Enabled: true}
	if err := store.Save(t.Context(), candidate, capability, time.Now()); err != nil {
		t.Fatal(err)
	}
	path, _, _ := store.location(candidate)
	if err := os.WriteFile(path, []byte("corrupt"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Load(candidate); err == nil {
		t.Fatal("corrupt cache accepted")
	}
	if saved, err := settings.Load(); err != nil || saved.SettingsRevision() != config.SettingsRevision() {
		t.Fatal("optional cache blocked settings", err)
	}
	if err := store.Save(t.Context(), candidate, capability, time.Now()); err != nil {
		t.Fatal(err)
	}
	if cached, err := store.Load(candidate); err != nil || cached == nil || *cached != capability {
		t.Fatal(cached, err)
	}
	after, _ := os.ReadFile(settings.Path)
	if string(before) != string(after) {
		t.Fatal("cache repair rewrote settings")
	}
}
