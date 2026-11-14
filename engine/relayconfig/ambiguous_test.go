package relayconfig

import (
	"errors"
	"strings"
	"testing"
)

// encoding/json keeps the last of repeated members and matches names
// case-insensitively, so a repeated or case-variant member silently replaces an
// earlier value. For relay profiles that rebinds an access key to another relay.
func TestDecodeRejectsRepeatedMembers(t *testing.T) {
	for _, input := range []string{
		`{"profiles":[{"url":"https://a.example","access_key":"k","url":"https://b.example"}]}`,
		`{"profiles":[{"url":"https://a.example","access_key":"k","URL":"https://b.example"}]}`,
		`{"profiles":[{"url":"https://a.example","access_key":"one","access_key":"two"}]}`,
		`{"profiles":[{"url":"https://a.example"}],"profiles":[{"url":"https://b.example"}]}`,
		`{"profiles":[{"url":"https://a.example"}],"front_url":"https://one.example","Front_URL":"https://two.example"}`,
	} {
		if _, err := Decode(strings.NewReader(input)); !errors.Is(err, ErrConfig) {
			t.Errorf("repeated members were accepted: %s err=%v", input, err)
		}
	}
	config, err := Decode(strings.NewReader(`{"profiles":[{"url":"https://a.example","access_key":"k"},{"url":"https://b.example","access_key":"k"}]}`))
	if err != nil || len(config.Profiles) != 2 {
		t.Fatalf("members repeated across sibling objects were rejected: %v", err)
	}
}
