package relay

import (
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"testing"
)

const relayEndpointVectorShareID = "AAAAAAAAAAAA"

type relayEndpointVectorFile struct {
	Version     int                      `json:"version"`
	Kind        string                   `json:"kind"`
	Cases       []relayEndpointCase      `json:"cases"`
	ASCIIMatrix relayEndpointASCIIMatrix `json:"asciiMatrix"`
}

type relayEndpointASCIIMatrix struct {
	First    byte                        `json:"first"`
	Last     byte                        `json:"last"`
	Path     relayEndpointASCIIComponent `json:"path"`
	Query    relayEndpointASCIIComponent `json:"query"`
	Userinfo relayEndpointASCIIComponent `json:"userinfo"`
}

type relayEndpointASCIIComponent struct {
	Skip         string            `json:"skip"`
	Alphanumeric bool              `json:"alphanumeric"`
	Literal      string            `json:"literal"`
	Escaped      map[string]string `json:"escaped"`
}

type relayEndpointCase struct {
	Name         string `json:"name"`
	RelayURL     string `json:"relayUrl"`
	Accepted     bool   `json:"accepted"`
	WebSocketURL string `json:"webSocketUrl"`
}

func TestRelayWebSocketURLSharedVectors(t *testing.T) {
	data, err := os.ReadFile("../../testvectors/relay-endpoint.json")
	if err != nil {
		t.Fatal(err)
	}
	var vector relayEndpointVectorFile
	if err := json.Unmarshal(data, &vector); err != nil {
		t.Fatalf("decode relay endpoint vectors: %v", err)
	}
	if vector.Version != 1 || vector.Kind != "relay-endpoint" {
		t.Fatalf("unexpected endpoint vector version=%d kind=%q", vector.Version, vector.Kind)
	}
	for _, testCase := range vector.Cases {
		t.Run(testCase.Name, func(t *testing.T) {
			endpoint, err := relayWebSocketURL(testCase.RelayURL, relayEndpointVectorShareID)
			if !testCase.Accepted {
				if err == nil {
					t.Fatalf("accepted hostile relay URL as %s", endpoint)
				}
				return
			}
			if err != nil {
				t.Fatalf("relayWebSocketURL: %v", err)
			}
			if got := endpoint.String(); got != testCase.WebSocketURL {
				t.Fatalf("endpoint = %q, want %q", got, testCase.WebSocketURL)
			}
		})
	}
	testRelayEndpointASCIIMatrix(t, vector.ASCIIMatrix)
}

func testRelayEndpointASCIIMatrix(t *testing.T, matrix relayEndpointASCIIMatrix) {
	t.Helper()
	components := []struct {
		name      string
		contract  relayEndpointASCIIComponent
		buildURLs func(raw, encoded string) (string, string)
	}{
		{
			name: "path", contract: matrix.Path,
			buildURLs: func(raw, encoded string) (string, string) {
				return "https://relay.example/matrix/x" + raw + "y/tail",
					"wss://relay.example/matrix/x" + encoded + "y/tail/v1/ws/" + relayEndpointVectorShareID
			},
		},
		{
			name: "query", contract: matrix.Query,
			buildURLs: func(raw, encoded string) (string, string) {
				return "https://relay.example/base?q=x" + raw + "y",
					"wss://relay.example/base/v1/ws/" + relayEndpointVectorShareID + "?q=x" + encoded + "y"
			},
		},
		{
			name: "userinfo", contract: matrix.Userinfo,
			buildURLs: func(raw, encoded string) (string, string) {
				return "https://user:px" + raw + "y@relay.example/base",
					"wss://user:px" + encoded + "y@relay.example/base/v1/ws/" + relayEndpointVectorShareID
			},
		},
	}
	for _, component := range components {
		t.Run(component.name, func(t *testing.T) {
			for value := matrix.First; value <= matrix.Last; value++ {
				character := string([]byte{value})
				if strings.Contains(component.contract.Skip, character) {
					continue
				}
				encoded, accepted := matrixCharacterEncoding(component.contract, value)
				rawURL, wantURL := component.buildURLs(character, encoded)
				t.Run(fmt.Sprintf("%02x", value), func(t *testing.T) {
					got, err := relayWebSocketURL(rawURL, relayEndpointVectorShareID)
					if !accepted {
						if err == nil {
							t.Fatalf("accepted raw %s byte 0x%02x as %s", component.name, value, got)
						}
						return
					}
					if err != nil {
						t.Fatalf("rejected raw %s byte 0x%02x: %v", component.name, value, err)
					}
					if got.String() != wantURL {
						t.Fatalf("endpoint = %q, want %q", got, wantURL)
					}
				})
				if value == matrix.Last {
					break
				}
			}
		})
	}
}

func matrixCharacterEncoding(contract relayEndpointASCIIComponent, value byte) (string, bool) {
	character := string([]byte{value})
	if encoded, ok := contract.Escaped[character]; ok {
		return encoded, true
	}
	if contract.Alphanumeric && (isASCIIAlpha(value) || value >= '0' && value <= '9') {
		return character, true
	}
	return character, strings.Contains(contract.Literal, character)
}
