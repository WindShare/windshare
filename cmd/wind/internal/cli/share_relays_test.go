package cli

import (
	"fmt"
	"github.com/windshare/windshare/connectivity/relayset"
	"reflect"
	"testing"
)

func TestShareRelayConfigurationIsPluralBoundedAndDeduplicated(t *testing.T) {
	app := testApp("")
	request, outcome := app.parseShareRequest([]string{"root", "--relay", "https://a.example", "--relay", "https://b.example", "--relay", "https://a.example"})
	if outcome != requestParseReady || !reflect.DeepEqual(request.relayURLs, []string{"https://a.example", "https://b.example"}) {
		t.Fatalf("relays=%v outcome=%v", request.relayURLs, outcome)
	}
	request, outcome = app.parseShareRequest([]string{"root"})
	if outcome != requestParseReady || !reflect.DeepEqual(request.relayURLs, []string{DefaultRelayURL}) {
		t.Fatal("default relay missing")
	}
	args := []string{"root"}
	for index := range relayset.MaximumEndpoints + 1 {
		args = append(args, "--relay", fmt.Sprintf("https://relay-%d.example", index))
	}
	if _, outcome = app.parseShareRequest(args); outcome != requestParseUsageFailure {
		t.Fatal("unbounded relay configuration accepted")
	}
}
