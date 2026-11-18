package account

import (
	"encoding/json"
	"io"
	"net/http"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/windshare/windshare/core/serviceauthority"
)

func TestSignedNodeDiscoveryBindsFullWebSocketEndpointWithoutAppendingTwice(t *testing.T) {
	data, err := os.ReadFile("../../testdata/network/authority.json")
	if err != nil {
		t.Fatal(err)
	}
	var fixture struct {
		Now        int64
		NodeKey    string `json:"node_key"`
		Descriptor json.RawMessage
	}
	if err := json.Unmarshal(data, &fixture); err != nil {
		t.Fatal(err)
	}
	key, err := serviceauthority.PublicKey(fixture.NodeKey)
	if err != nil {
		t.Fatal(err)
	}
	config, _ := accountFixture(t)
	config.Now = func() time.Time { return time.Unix(fixture.Now, 0) }
	config.Client = clientFunc(func(request *http.Request) (*http.Response, error) {
		if request.URL.Path != "/.well-known/windshare" || request.Header.Get("Authorization") != "" {
			t.Fatal("discovery leaked authority or used a transfer endpoint", request.URL)
		}
		return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader(string(fixture.Descriptor)))}, nil
	})
	service := testService(t, config)
	descriptor, err := service.discover(t.Context(), "https://node.example", key)
	if err != nil || !descriptor.Anonymous {
		t.Fatal(descriptor, err)
	}
	if _, err := service.discover(t.Context(), "https://node.example/another-base", key); err == nil {
		t.Fatal("node moved its signed endpoint")
	}
	key[0] ^= 1
	if _, err := service.discover(t.Context(), "https://node.example", key); err == nil {
		t.Fatal("directory-pinned node key was ignored")
	}
}
