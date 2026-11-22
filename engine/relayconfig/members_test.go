package relayconfig

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/windshare/windshare/engine/relayprofile"
)

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
