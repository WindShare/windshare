package relayconfig

import (
	"context"
	"encoding/json"
	"errors"
	"path/filepath"
	"strings"
	"testing"

	"github.com/windshare/windshare/engine/relayprofile"
	"github.com/windshare/windshare/engine/turn"
)

const turnAccount = `{"id":"account","provider":"turn","issuer":"static","local_budget_bytes":0,"credentials":%s}`

// encoding/json keeps the last of repeated members and matches names
// case-insensitively, so a repeated or case-variant member silently replaces an
// earlier value. For relay profiles that rebinds an access key to another relay.
func TestDecodeRejectsRepeatedMembersByName(t *testing.T) {
	for _, test := range []struct{ input, member, object string }{
		{`{"profiles":[{"url":"https://a.example","access_key":"secret-value","url":"https://b.example"}]}`, `"url"`, "profiles[0]"},
		{`{"profiles":[{"url":"https://a.example","access_key":"secret-value","URL":"https://b.example"}]}`, `"URL"`, "profiles[0]"},
		{`{"profiles":[{"url":"https://a.example"},{"url":"https://b.example","access_key":"secret-value","access_key":"two"}]}`, `"access_key"`, "profiles[1]"},
		{`{"profiles":[{"url":"https://a.example","access_key":"secret-value","acce\u017fs_key":"two"}]}`, `"acceſs_key"`, "profiles[0]"},
		{`{"profiles":[{"url":"https://a.example","access_key":"secret-value","access_\u212aey":"two"}]}`, "\"access_\u212aey\"", "profiles[0]"},
		{`{"profiles":[{"url":"https://a.example"}],"profiles":[{"url":"https://secret-value.example"}]}`, `"profiles"`, "the top-level object"},
		{`{"profiles":[{"url":"https://a.example"}],"front_url":"https://secret-value.example","Front_URL":"https://two.example"}`, `"Front_URL"`, "the top-level object"},
		// Only credentials are opaque; their typed siblings and the rest of turn are checked.
		{`{"profiles":[{"url":"https://a.example"}],"turn":{"accounts":[{"id":"secret-value","id":"two","credentials":{}}]}}`, `"id"`, "turn.accounts[0]"},
		{`{"profiles":[{"url":"https://a.example"}],"turn":{"accounts":[{"analytics":{"api_token":"secret-value","API_TOKEN":"two"}}]}}`, `"API_TOKEN"`, "turn.accounts[0].analytics"},
		{`{"profiles":[{"url":"https://a.example"}],"turn":{"profiles":[{"urls":["turn:secret-value"],"urls":[]}]}}`, `"urls"`, "turn.profiles[0]"},
		{`{"profiles":[{"url":"https://a.example"}],"turn":{"accounts":[],"Accounts":[]}}`, `"Accounts"`, "turn"},
	} {
		_, err := Decode(strings.NewReader(test.input))
		if !errors.Is(err, ErrRepeatedMember) || !errors.Is(err, ErrConfig) {
			t.Errorf("repeated member was not reported: %s err=%v", test.input, err)
			continue
		}
		message := err.Error()
		if !strings.Contains(message, "member "+test.member+" appears more than once in "+test.object) || strings.Contains(message, "secret-value") {
			t.Errorf("error does not name only the member and its object: %s", message)
		}
	}
}

func TestDecodeAcceptsDistinctMembersInAnyCase(t *testing.T) {
	for _, input := range []string{
		`{"Profiles":[{"URL":"https://a.example","Access_Key":"k"}],"FRONT_URL":"https://front.example"}`,
		`{"profiles":[{"url":"https://a.example","access_key":"k"},{"url":"https://b.example","access_key":"k"}]}`,
	} {
		if _, err := Decode(strings.NewReader(input)); err != nil {
			t.Errorf("unambiguous document was rejected: %s err=%v", input, err)
		}
	}
}

// Earlier versions accepted repetitions inside TURN credentials and wrote the
// RawMessage back verbatim, so an upgraded device must still load and update it.
func TestRepeatedMembersInsideTURNCredentialsStillLoad(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.json")
	store := FileStore{Path: path}
	saved := Config{
		Profiles: []relayprofile.Candidate{{PublicURL: "https://relay.example"}},
		TURN: turn.ManualSettings{
			Accounts: []turn.AccountSettings{{ID: "account", Provider: "turn", Issuer: turn.StaticIssuer,
				Credentials: json.RawMessage(`{"username":"user","password":"old","password":"new"}`)}},
			Profiles: []turn.ProfileSettings{{ID: "profile", Account: "account", URLs: []string{"turn:turn.example:3478"}}},
		},
	}
	if err := store.Save(saved); err != nil {
		t.Fatal(err)
	}
	if loaded, err := store.Load(); err != nil || len(loaded.TURN.Accounts) != 1 {
		t.Fatal("configuration written by an earlier version no longer loads:", err)
	}
	if _, err := store.Update(context.Background(), func(config *Config) error {
		config.FrontURL = "https://front.example"
		return nil
	}); err != nil {
		t.Fatal("read-modify-write of an earlier configuration failed:", err)
	}
	for _, credentials := range []string{`{"username":"u","password":"a","PASSWORD":"b"}`, `{"username":"u","username":"u","password":"p"}`} {
		input := `{"profiles":[{"url":"https://relay.example"}],"turn":{"Accounts":[` +
			strings.Replace(turnAccount, "%s", credentials, 1) + `],"profiles":[{"id":"p","account":"account","urls":["turn:turn.example:3478"]}]}}`
		input = strings.Replace(input, `"credentials"`, `"Credentials"`, 1)
		if _, err := Decode(strings.NewReader(input)); err != nil {
			t.Errorf("credentials were not treated as opaque: %s err=%v", input, err)
		}
	}
}

func TestDecodeBoundsNesting(t *testing.T) {
	deep := strings.Repeat("[", maxConfigDepth+1) + strings.Repeat("]", maxConfigDepth+1)
	for _, input := range []string{
		`{"profiles":[{"url":"https://a.example"}],"extra":` + deep + `}`,
		`{"profiles":[{"url":"https://a.example"}],"turn":{"accounts":[` + strings.Replace(turnAccount, "%s", deep, 1) + `]}}`,
	} {
		if _, err := Decode(strings.NewReader(input)); !errors.Is(err, ErrConfig) || errors.Is(err, ErrRepeatedMember) {
			t.Errorf("nesting beyond the limit was not rejected as invalid: err=%v", err)
		}
	}
}
