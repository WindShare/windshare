package relayv2

import (
	"context"
	"encoding/base64"
	"errors"
	"net/http"
	"testing"

	v2 "github.com/windshare/windshare/relay/protocol/v2"
)

func TestEveryRelayDialRoutesTheUpgradeByShare(t *testing.T) {
	fixture := newClientFixture(t)
	resume, err := ResumeInit(fixture.fresh)
	if err != nil {
		t.Fatal(err)
	}
	operations := []struct {
		name string
		dial func(string, DialOptions) error
	}{
		{"REGISTER", func(base string, options DialOptions) error {
			_, err := DialSender(context.Background(), SenderConfig{
				RelayBaseURL: base, Init: fixture.fresh, Descriptor: fixture.descriptor,
				SenderPrivateKey: fixture.privateKey, Dial: options,
			})
			return err
		}},
		{"RESUME", func(base string, options DialOptions) error {
			_, err := DialSender(context.Background(), SenderConfig{
				RelayBaseURL: base, Init: resume, ResumeToken: fixture.token,
				SenderPrivateKey: fixture.privateKey, Dial: options,
			})
			return err
		}},
		{"JOIN", func(base string, options DialOptions) error {
			_, err := DialReceiver(context.Background(), ReceiverConfig{
				RelayBaseURL: base, ShareID: fixture.fresh.ShareID, Dial: options,
			})
			return err
		}},
		{"STOP", func(base string, options DialOptions) error {
			return Stop(context.Background(), StopConfig{
				RelayBaseURL: base, ShareID: fixture.fresh.ShareID,
				ShareInstance: fixture.fresh.ShareInstance, PKHash: fixture.fresh.PKHash,
				StopID: v2.StopID{1}, SenderPrivateKey: fixture.privateKey, Dial: options,
			})
		}},
	}
	const base = "https://relay.example/base?token=a%20b"
	share := base64.RawURLEncoding.EncodeToString(fixture.fresh.ShareID[:])
	for _, operation := range operations {
		t.Run(operation.name, func(t *testing.T) {
			intercepted := errors.New("upgrade intercepted")
			calls := 0
			options := DialOptions{HTTPClient: &http.Client{Transport: relayUpgradeRoundTripper(func(request *http.Request) (*http.Response, error) {
				calls++
				if request.URL.Path != "/base/v2/ws" || request.URL.RawQuery != "token=a%20b&share_id="+share ||
					request.Header.Get("Upgrade") != "websocket" {
					t.Errorf("upgrade request = %s, headers = %v", request.URL, request.Header)
				}
				return nil, intercepted
			})}}
			if err := operation.dial(base, options); !errors.Is(err, intercepted) || calls != 1 {
				t.Fatalf("upgrade calls=%d error=%v", calls, err)
			}
			if err := operation.dial(base+"&share%5fid="+share, options); !errors.Is(err, v2.ErrIdentity) || !errors.Is(err, ErrProtocol) || calls != 1 {
				t.Fatalf("preconfigured route reached dial: calls=%d error=%v", calls, err)
			}
		})
	}
}

type relayUpgradeRoundTripper func(*http.Request) (*http.Response, error)

func (roundTrip relayUpgradeRoundTripper) RoundTrip(request *http.Request) (*http.Response, error) {
	return roundTrip(request)
}
