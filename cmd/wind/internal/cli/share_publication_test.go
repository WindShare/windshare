package cli

import (
	"bytes"
	"crypto/ed25519"
	"errors"
	"io"
	"net/url"
	"reflect"
	"strings"
	"testing"

	"github.com/windshare/windshare/core/link"
)

func TestCapabilityPublicationBuildsExactInvariantPayload(t *testing.T) {
	capability := testShareCapability(t)
	full, err := capability.URL("https://windshare.example/app")
	if err != nil {
		t.Fatal(err)
	}
	bare, key, err := capability.SplitURL("https://windshare.example/app")
	if err != nil {
		t.Fatal(err)
	}
	for _, test := range []struct {
		name  string
		split bool
		want  string
	}{
		{name: "complete", want: "Link: " + full + "\n"},
		{name: "split", split: true, want: "Bare link: " + bare + "\nKey: " + key + "\n"},
	} {
		t.Run(test.name, func(t *testing.T) {
			for _, presentation := range []string{"tty-default", "tty-verbose", "redirected-default", "redirected-verbose"} {
				payload, err := buildShareCapabilityPayload(capability, shareLinkPresentation{frontURL: "https://windshare.example/app", splitKey: test.split})
				if err != nil {
					t.Fatalf("%s: build payload: %v", presentation, err)
				}
				if got := string(payload); got != test.want {
					t.Fatalf("%s: payload = %q, want %q", presentation, got, test.want)
				}
			}
		})
	}
}

func TestPublishedLinksPreserveCapabilityAndResolveRelays(t *testing.T) {
	for _, test := range []struct {
		name       string
		relays     []string
		wantRelays []string
		explicit   bool
	}{
		{"same origin", []string{"wss://windshare.example"}, []string{"https://windshare.example"}, false},
		{"other origin", []string{"https://relay-a.example"}, []string{"https://relay-a.example"}, true},
		{"multiple relays", []string{"https://windshare.example", "https://relay-b.example"}, []string{"https://windshare.example", "https://relay-b.example"}, true},
	} {
		t.Run(test.name, func(t *testing.T) {
			capability := testShareCapability(t)
			capability.Relays = test.relays
			for _, split := range []bool{false, true} {
				for _, trace := range []bool{false, true} {
					payload, err := buildShareCapabilityPayload(capability, shareLinkPresentation{
						frontURL: "https://windshare.example/app", splitKey: split, browserTrace: trace,
					})
					if err != nil {
						t.Fatal(err)
					}
					lines := strings.Split(strings.TrimSpace(string(payload)), "\n")
					linkText := strings.TrimPrefix(strings.TrimPrefix(lines[0], "Bare link: "), "Link: ")
					page, err := url.Parse(linkText)
					if err != nil {
						t.Fatal(err)
					}
					if page.Query().Has("r") != test.explicit {
						t.Fatalf("unexpected relay hint: %s", linkText)
					}
					wantTrace := ""
					if trace {
						wantTrace = browserTraceQueryEnabled
					}
					if page.Query().Get(browserTraceQueryParameter) != wantTrace {
						t.Fatal("browser tracing did not preserve the requested setting")
					}
					args := []string{linkText}
					if split {
						if page.Fragment != "" {
							t.Fatal("split-key URL includes credentials")
						}
						args = append(args, "--key", strings.TrimPrefix(lines[1], "Key: "))
					}
					app, _, stderr := newSemanticTestApp(strings.NewReader(""))
					request, outcome := app.parseGetRequest(args)
					want := capability
					want.Relays = test.wantRelays
					if outcome != requestParseReady || !reflect.DeepEqual(request.link, want) {
						t.Fatalf("published link cannot be received: outcome=%d relays=%v stderr=%s", outcome, request.link.Relays, stderr.String())
					}
				}
			}
		})
	}
}

func TestBrowserTraceRequestIsIndependentOfSenderTrace(t *testing.T) {
	app := &App{Stderr: io.Discard}
	t.Cleanup(app.closeTerminalOutput)
	request, outcome := app.parseShareRequest([]string{"root", "--browser-trace", "--split-key"})
	if outcome != requestParseReady || !request.link.browserTrace || !request.link.splitKey {
		t.Fatalf("unexpected request: %+v outcome: %v", request, outcome)
	}
	if request.observation.traceEnabled() {
		t.Fatal("browser trace enabled sender trace")
	}
	request, outcome = app.parseShareRequest([]string{"root"})
	if outcome != requestParseReady || request.link.browserTrace {
		t.Fatal("ordinary links enable browser tracing")
	}
}

func TestCapabilityPublicationChecksOneCompleteWrite(t *testing.T) {
	payload := []byte("Bare link: https://example.invalid/share\nKey: secret\n")
	writer := &sharePublicationWriter{}
	if err := publishShareCapability(writer, payload); err != nil {
		t.Fatal(err)
	}
	if writer.calls != 1 || !bytes.Equal(writer.payload, payload) {
		t.Fatalf("writes = %d payload = %q", writer.calls, writer.payload)
	}

	for _, test := range []struct {
		name   string
		writer io.Writer
	}{
		{name: "nil", writer: nil},
		{name: "short", writer: shareShortWriter{}},
		{name: "failed", writer: shareFailedWriter{}},
	} {
		t.Run(test.name, func(t *testing.T) {
			if err := publishShareCapability(test.writer, payload); err == nil {
				t.Fatal("publication failure was accepted")
			}
		})
	}
}

func testShareCapability(t *testing.T) link.Link {
	t.Helper()
	seed := bytes.Repeat([]byte{0x5a}, ed25519.SeedSize)
	privateKey := ed25519.NewKeyFromSeed(seed)
	capability, err := link.NewSenderAuthenticated(
		bytes.Repeat([]byte{0xa5}, link.ReadSecretBytes),
		privateKey.Public().(ed25519.PublicKey),
		[]string{"wss://relay.example/ws/v2?forbidden=discarded"},
	)
	if err != nil {
		t.Fatal(err)
	}
	return capability
}

type sharePublicationWriter struct {
	calls   int
	payload []byte
}

func (writer *sharePublicationWriter) Write(payload []byte) (int, error) {
	writer.calls++
	writer.payload = append([]byte(nil), payload...)
	return len(payload), nil
}

type shareShortWriter struct{}

func (shareShortWriter) Write(payload []byte) (int, error) { return len(payload) - 1, nil }

type shareFailedWriter struct{}

func (shareFailedWriter) Write([]byte) (int, error) { return 0, errors.New("stdout provider canary") }
