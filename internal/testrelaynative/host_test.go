package testrelaynative

import (
	"path/filepath"
	"slices"
	"testing"
)

func TestFixtureKeepsDurableIdentityAcrossRestarts(t *testing.T) {
	for _, endpoint := range []string{"", "wss://relay.example/proxy%20prefix/v2/ws"} {
		t.Run(endpoint, func(t *testing.T) {
			fixture := NewFixture(t.TempDir(), endpoint)
			initial := fixture.arguments()
			for _, flag := range []string{"--listen", "--stun-udp", "--stun-admin"} {
				requireArgument(t, initial, flag, loopbackAddress)
			}
			ready := Readiness{Address: "127.0.0.1:12345", Endpoint: endpoint}
			if endpoint == "" {
				ready.Endpoint = "ws://127.0.0.1:12345/v2/ws"
			}
			if err := fixture.acceptReadiness(ready); err != nil {
				t.Fatal(err)
			}
			restarted := fixture.arguments()
			requireArgument(t, restarted, "--database", fixture.Database())
			requireArgument(t, restarted, "--public-endpoint", ready.Endpoint)
			wantListen := loopbackAddress
			if endpoint == "" {
				wantListen = ready.Address
			}
			requireArgument(t, restarted, "--listen", wantListen)
			ready.Endpoint = "ws://127.0.0.1:54321/v2/ws"
			if err := fixture.acceptReadiness(ready); err == nil {
				t.Fatal("accepted changed endpoint identity")
			}
			if !slices.Equal(fixture.arguments(), restarted) {
				t.Fatal("failed readiness changed restart identity")
			}
		})
	}
}

func TestFixturesUseIndependentDatabases(t *testing.T) {
	firstDirectory, secondDirectory := t.TempDir(), t.TempDir()
	first := NewFixture(firstDirectory, "")
	second := NewFixture(secondDirectory, "")
	if first.Database() == second.Database() || filepath.Dir(first.Database()) != firstDirectory ||
		filepath.Dir(second.Database()) != secondDirectory {
		t.Fatalf("databases = %q, %q", first.Database(), second.Database())
	}
}

func requireArgument(t *testing.T, arguments []string, flag, want string) {
	t.Helper()
	index := slices.Index(arguments, flag)
	if index < 0 || index+1 == len(arguments) || arguments[index+1] != want {
		t.Fatalf("arguments %v: %s must be %q", arguments, flag, want)
	}
}
