package engine_test

import (
	"bytes"
	"crypto/ed25519"
	"encoding/base64"
	"errors"
	"reflect"
	"testing"

	"github.com/windshare/windshare/core/link"
	"github.com/windshare/windshare/engine"
)

func TestResolveReceiveLinkUsesOneCapabilityForCompleteAndSeparateInputs(t *testing.T) {
	capability, bare, key := receiveLinkFixture(t)
	full := bare + "#" + key
	originLink := capability.Relays[0] + "/app/" + capability.ShareID
	for _, test := range []struct {
		name  string
		input engine.ReceiveLinkInput
	}{
		{name: "complete link", input: engine.ReceiveLinkInput{URL: full}},
		{name: "separate key", input: engine.ReceiveLinkInput{URL: bare, SeparateKey: key}},
		{name: "matching embedded key", input: engine.ReceiveLinkInput{URL: full, SeparateKey: key}},
		{name: "copied URL as key", input: engine.ReceiveLinkInput{URL: bare, SeparateKey: "  " + full + "  "}},
		{name: "complete link infers relay", input: engine.ReceiveLinkInput{URL: originLink + "#" + key}},
		{name: "separate link infers relay", input: engine.ReceiveLinkInput{URL: originLink, SeparateKey: key}},
	} {
		t.Run(test.name, func(t *testing.T) {
			resolved, err := engine.ResolveReceiveLink(test.input)
			if err != nil || !reflect.DeepEqual(resolved, capability) {
				t.Fatalf("resolved=%+v error=%v want=%+v", resolved, err, capability)
			}
		})
	}
}

func TestResolveReceiveLinkClassifiesRequiredUserActionAndRetainsCause(t *testing.T) {
	capability, bare, key := receiveLinkFixture(t)
	capability.ReadSecret[0] ^= 0xff
	conflictingKey, err := capability.KeyString()
	if err != nil {
		t.Fatal(err)
	}
	const unsupportedSuite byte = 0x7f
	unsupportedKey := base64.RawURLEncoding.EncodeToString([]byte{unsupportedSuite})
	for _, test := range []struct {
		name  string
		input engine.ReceiveLinkInput
		kind  error
		cause error
	}{
		{
			name: "missing key permits prompting", input: engine.ReceiveLinkInput{URL: bare},
			kind: engine.ErrReceiveKeyRequired, cause: link.ErrMissingFragment,
		},
		{
			name: "invalid link must not prompt", input: engine.ReceiveLinkInput{URL: "not-a-link"},
			kind: engine.ErrInvalidReceiveLink, cause: link.ErrMalformedLink,
		},
		{
			name: "key does not repair an invalid link", input: engine.ReceiveLinkInput{URL: "not-a-link", SeparateKey: key},
			kind: engine.ErrInvalidReceiveLink, cause: link.ErrMalformedLink,
		},
		{
			name: "conflicting keys require correction", input: engine.ReceiveLinkInput{URL: bare + "#" + key, SeparateKey: conflictingKey},
			kind: engine.ErrReceiveKeyConflict, cause: link.ErrKeyConflict,
		},
		{
			name: "malformed embedded key", input: engine.ReceiveLinkInput{URL: bare + "#!!!"},
			kind: engine.ErrInvalidReceiveLink, cause: link.ErrMalformedFragment,
		},
		{
			name: "explicit malformed key cannot be ignored", input: engine.ReceiveLinkInput{URL: bare + "#" + key, SeparateKey: "!!!"},
			kind: engine.ErrInvalidReceiveLink, cause: link.ErrMalformedFragment,
		},
		{
			name: "unsupported complete link", input: engine.ReceiveLinkInput{URL: bare + "#" + unsupportedKey},
			kind: engine.ErrUnsupportedReceiveSuite, cause: link.ErrUnknownSuite,
		},
		{
			name: "unsupported separate key", input: engine.ReceiveLinkInput{URL: bare, SeparateKey: unsupportedKey},
			kind: engine.ErrUnsupportedReceiveSuite, cause: link.ErrUnknownSuite,
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			resolved, err := engine.ResolveReceiveLink(test.input)
			if !errors.Is(err, test.kind) || !errors.Is(err, test.cause) {
				t.Fatalf("error=%v want kind=%v cause=%v", err, test.kind, test.cause)
			}
			if !reflect.DeepEqual(resolved, link.Link{}) {
				t.Fatalf("failed resolution exposed a capability: %+v", resolved)
			}
		})
	}
}

func receiveLinkFixture(t *testing.T) (link.Link, string, string) {
	t.Helper()
	capability, err := link.NewSenderAuthenticated(
		bytes.Repeat([]byte{1}, link.ReadSecretBytes),
		ed25519.PublicKey(bytes.Repeat([]byte{2}, ed25519.PublicKeySize)),
		[]string{"https://relay.example"},
	)
	if err != nil {
		t.Fatal(err)
	}
	bare, key, err := capability.SplitURL("https://app.example")
	if err != nil {
		t.Fatal(err)
	}
	return capability, bare, key
}
