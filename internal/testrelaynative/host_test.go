package testrelaynative

import (
	"path/filepath"
	"reflect"
	"testing"
)

func TestFixtureKeepsDurableIdentityAcrossRestarts(t *testing.T) {
	for _, endpoint := range []string{"", "wss://relay.example/proxy%20prefix/v2/ws"} {
		t.Run(endpoint, func(t *testing.T) {
			fixture := NewFixture(t.TempDir(), endpoint)
			initial := fixture.configuration()
			if initial["listen"] != loopbackAddress {
				t.Fatal("initial listener must be ephemeral")
			}
			ready := Readiness{Address: "127.0.0.1:12345", Endpoint: endpoint}
			if endpoint == "" {
				ready.Endpoint = "ws://127.0.0.1:12345/v2/ws"
			}
			if err := fixture.acceptReadiness(ready); err != nil {
				t.Fatal(err)
			}
			restarted := fixture.configuration()
			if restarted["database"] != fixture.Database() || restarted["identity_file"] != initial["identity_file"] || restarted["endpoint"] != ready.Endpoint {
				t.Fatal("restart changed durable node identity")
			}
			wantListen := loopbackAddress
			if endpoint == "" {
				wantListen = ready.Address
			}
			if restarted["listen"] != wantListen {
				t.Fatal("restart lost listener identity")
			}
			ready.Endpoint = "ws://127.0.0.1:54321/v2/ws"
			if fixture.acceptReadiness(ready) == nil {
				t.Fatal("accepted changed endpoint")
			}
			if !reflect.DeepEqual(fixture.configuration(), restarted) {
				t.Fatal("failed readiness changed configuration")
			}
		})
	}
}
func TestFixturesUseIndependentDatabases(t *testing.T) {
	firstDirectory, secondDirectory := t.TempDir(), t.TempDir()
	first, second := NewFixture(firstDirectory, ""), NewFixture(secondDirectory, "")
	if first.Database() == second.Database() || filepath.Dir(first.Database()) != firstDirectory || filepath.Dir(second.Database()) != secondDirectory {
		t.Fatal("fixture storage is not isolated")
	}
}
