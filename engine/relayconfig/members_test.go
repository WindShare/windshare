package relayconfig

import (
	"context"
	"encoding/json"
	"errors"
	"path/filepath"
	"reflect"
	"runtime"
	"strconv"
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

// The escape is decoded before the name is matched, as encoding/json does.
func TestEscapedCredentialsNameIsStillOpaque(t *testing.T) {
	input := `{"profiles":[{"url":"https://relay.example"}],"turn":{"accounts":[` +
		strings.Replace(turnAccount, `"credentials":%s`, `"cr\u0065dentials":{"username":"u","password":"a","password":"b"}`, 1) +
		`],"profiles":[{"id":"p","account":"account","urls":["turn:turn.example:3478"]}]}}`
	if _, err := Decode(strings.NewReader(input)); err != nil {
		t.Fatal("escaped credentials name was not treated as opaque:", err)
	}
}

// Only schema field names reach the message, so input text can neither add a
// line to it nor make it long. Names outside the schema are rejected by
// Decode anyway and get the generic error.
func TestRepeatedMemberMessageIsBoundedAndSingleLine(t *testing.T) {
	forged := "x\r\nconfig import: Saved sharing configuration with 1 relay candidate(s)."
	long := strings.Repeat("k", maxConfigBytes/2)
	for _, input := range []string{
		`{"profiles":[{"url":"https://a.example"}],` + strconv.Quote(forged) + `:{"a":1,"a":2}}`,
		`{"profiles":[{"url":"https://a.example"}],` + strconv.Quote(forged) + `:1,` + strconv.Quote(forged) + `:2}`,
		`{"profiles":[{"url":"https://a.example","sk-SECRET123":{"a":1,"a":2}}]}`,
		`{"profiles":[{"url":"https://a.example"}],"` + long + `":1,"` + long[1:] + `":2}`,
		`{"profiles":[{"` + long[:1000] + `":{"url":1,"url":2}}]}`,
	} {
		_, err := Decode(strings.NewReader(input))
		if !errors.Is(err, ErrConfig) || errors.Is(err, ErrRepeatedMember) {
			t.Errorf("member outside the schema was not a generic configuration error: %.80q err=%v", input, err)
		}
	}
	// A schema name spelled with look-alikes stays readable and ASCII.
	_, err := Decode(strings.NewReader(`{"Turn":{"ACCOUNTS":[],"accounts":[]},"profiles":[]}`))
	_, err2 := Decode(strings.NewReader("{\"profiles\":[{\"url\":\"https://a.example\",\"access_\u212aey\":\"k\",\"ACCESS_KEY\":\"k\"}]}"))
	for _, err := range []error{err, err2} {
		message := err.Error()
		if !errors.Is(err, ErrConfig) || strings.ContainsAny(message, "\r\n\u212a") || len(message) > 200 {
			t.Errorf("message is not a short ASCII line: %q", message)
		}
	}
	if want := `member "accounts" appears more than once in turn (also written as "ACCOUNTS"`; !strings.Contains(err.Error(), want) {
		t.Errorf("path does not use schema names: %q", err)
	}
	if want := `member "ACCESS_KEY" appears more than once in profiles[0] (also written as "access_\u212aey"`; !strings.Contains(err2.Error(), want) {
		t.Errorf("look-alike is not escaped: %q", err2)
	}
}

// amplifyingConfiguration nests long member names around a 64 KiB-scale array,
// inside TURN credentials when opaque is set. The scan used to build every
// member's full path, allocating hundreds of megabytes for such input.
func amplifyingConfiguration(opaque bool) string {
	levels := 60
	key := `"` + strings.Repeat("n", 480) + `"`
	prefix := `{"profiles":[{"url":"https://a.example"}],"active_relays":`
	suffix := "}"
	if opaque {
		prefix = `{"profiles":[{"url":"https://a.example"}],"turn":{"accounts":[` +
			`{"id":"a","provider":"turn","issuer":"static","local_budget_bytes":0,"credentials":`
		suffix = "}]}}"
		levels = maxConfigDepth - 6 // within the nesting limit
	}
	prefix += strings.Repeat("{"+key+":", levels) + "[0"
	suffix = "]" + strings.Repeat("}", levels) + suffix
	elements := (maxConfigBytes - len(prefix) - len(suffix)) / 2
	return prefix + strings.Repeat(",0", elements) + suffix
}

func TestScanCostIsBoundedOnAmplifyingInput(t *testing.T) {
	for _, opaque := range []bool{false, true} {
		input := amplifyingConfiguration(opaque)
		if len(input) > maxConfigBytes {
			t.Fatalf("input is %d bytes, beyond the size limit", len(input))
		}
		var before, after runtime.MemStats
		runtime.ReadMemStats(&before)
		_, err := Decode(strings.NewReader(input))
		runtime.ReadMemStats(&after)
		if !errors.Is(err, ErrConfig) {
			t.Fatal("amplifying input was accepted:", err)
		}
		// The input is copied a few times at most; full paths cost ~570 MB.
		if allocated := after.TotalAlloc - before.TotalAlloc; allocated > 16*maxConfigBytes*8 {
			t.Errorf("opaque=%v: decoding %d bytes allocated %d bytes", opaque, len(input), allocated)
		}
	}
}

// Only turn.accounts[].credentials may escape the repeated-member check; a
// new opaque field (json.RawMessage, a map, an interface or any type with its
// own UnmarshalJSON) needs its own decision about repetitions.
func TestOnlyTURNCredentialsAreOpaque(t *testing.T) {
	var opaque []string
	var walk func(string, *schemaNode)
	walk = func(path string, node *schemaNode) {
		switch {
		case node.opaque:
			opaque = append(opaque, path)
		case node.element != nil:
			walk(path+"[]", node.element)
		}
		for _, field := range node.fields {
			walk(strings.TrimPrefix(path+"."+field.name, "."), field.node)
		}
	}
	walk("", documentSchema())
	if !reflect.DeepEqual(opaque, []string{"turn.accounts[].credentials"}) {
		t.Fatalf("opaque members: %v", opaque)
	}
}
