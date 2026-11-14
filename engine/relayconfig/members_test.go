package relayconfig

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
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
		{`{"profiles":[{"url":"https://a.example","access_key":"secret-value","acce\u017fs_key":"two"}]}`, `"acce\u017fs_key"`, "profiles[0]"},
		{`{"profiles":[{"url":"https://a.example","access_key":"secret-value","access_\u212aey":"two"}]}`, `"access_\u212aey"`, "profiles[0]"},
		{`{"profiles":[{"url":"https://a.example"}],"profiles":[{"url":"https://secret-value.example"}]}`, `"profiles"`, "the top-level object"},
		{`{"profiles":[{"url":"https://a.example"}],"front_url":"https://secret-value.example","Front_URL":"https://two.example"}`, `"Front_URL"`, "the top-level object"},
		// Typed TURN fields use the same rule as relay profiles and credentials.
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

func TestDecodeRejectsRepeatedTURNCredentials(t *testing.T) {
	for _, credentials := range []string{
		`{"username":"u","password":"a","password":"b"}`,
		`{"username":"u","password":"a","PASSWORD":"b"}`,
		`{"username":"u","username":"u","password":"p"}`,
	} {
		for _, name := range []string{"credentials", "Credentials", `cr\u0065dentials`} {
			account := strings.Replace(turnAccount, "%s", credentials, 1)
			account = strings.Replace(account, "credentials", name, 1)
			input := `{"profiles":[],"turn":{"accounts":[` + account + `],"profiles":[{"id":"p","account":"account","urls":["turn:turn.example:3478"]}]}}`
			if _, err := Decode(strings.NewReader(input)); !errors.Is(err, ErrRepeatedMember) ||
				!errors.Is(err, ErrConfig) || !strings.Contains(err.Error(), "turn.accounts[0].credentials") {
				t.Fatalf("ambiguous credentials accepted or lost their location: %v", err)
			}
		}
	}
}

func TestSaveAndUpdateRejectAmbiguousCredentialsWithoutReplacingSettings(t *testing.T) {
	store := FileStore{Path: filepath.Join(t.TempDir(), "config.json")}
	saved := Config{
		Profiles: []relayprofile.Candidate{{PublicURL: "https://relay.example"}},
		TURN: turn.ManualSettings{
			Accounts: []turn.AccountSettings{{ID: "account", Provider: "turn", Issuer: turn.StaticIssuer,
				Credentials: json.RawMessage(`{"username":"user","password":"password"}`)}},
			Profiles: []turn.ProfileSettings{{ID: "profile", Account: "account", URLs: []string{"turn:turn.example:3478"}}},
		},
	}
	if err := store.Save(saved); err != nil {
		t.Fatal(err)
	}
	before, err := os.ReadFile(store.Path)
	if err != nil {
		t.Fatal(err)
	}
	repeated := json.RawMessage(`{"username":"user","password":"old","PASSWORD":"new"}`)
	saved.TURN.Accounts[0].Credentials = repeated
	if err := store.Save(saved); !errors.Is(err, ErrRepeatedMember) || !errors.Is(err, ErrConfig) {
		t.Fatal("programmatic save bypassed member validation:", err)
	}
	if _, err := store.Update(context.Background(), func(config *Config) error {
		config.TURN.Accounts[0].Credentials = repeated
		return nil
	}); !errors.Is(err, ErrRepeatedMember) {
		t.Fatal("programmatic update bypassed member validation:", err)
	}
	after, err := os.ReadFile(store.Path)
	if err != nil || string(before) != string(after) {
		t.Fatal("rejected settings changed the saved file:", err)
	}
}

func TestRepeatedSavedMembersIdentifyTheRepairFile(t *testing.T) {
	directory := t.TempDir()
	t.Chdir(directory)
	path := "config.json"
	input := `{"profiles":[{"url":"https://relay.example","url":"https://relay.example"}]}`
	if err := os.WriteFile(path, []byte(input), 0o600); err != nil {
		t.Fatal(err)
	}
	store := FileStore{Path: path}
	_, loadErr := store.Load()
	_, updateErr := store.Update(context.Background(), func(*Config) error {
		t.Fatal("an ambiguous file reached the edit callback")
		return nil
	})
	for _, err := range []error{loadErr, updateErr} {
		if !errors.Is(err, ErrRepeatedMember) || !strings.Contains(err.Error(), fmt.Sprintf("%+q", filepath.Join(directory, path))) {
			t.Fatalf("repair diagnostic lost the actual file or error category: %v", err)
		}
	}
	// Complete replacement remains available without interpreting the damaged file.
	if err := store.Save(Config{Profiles: []relayprofile.Candidate{{PublicURL: "https://relay.example"}}}); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Load(); err != nil {
		t.Fatal("clean replacement did not restore normal loading:", err)
	}
}
