package relayprofile

import "testing"

func TestProfilesPreserveSelectionOrderAndRejectAmbiguousAuthority(t *testing.T) {
	values := []Candidate{{PublicURL: "https://one.example", AccessKey: "first"}, {PublicURL: "https://two.example", AccessKey: "second"}}
	urls, err := PublicURLs(values)
	if err != nil || len(urls) != 2 || urls[0] != values[0].PublicURL || urls[1] != values[1].PublicURL {
		t.Fatalf("selected relays=%v err=%v", urls, err)
	}
	for _, value := range values {
		if err := value.Validate(); err != nil {
			t.Fatal(err)
		}
	}
	for _, invalid := range []Candidate{{PublicURL: "not a relay"}, {PublicURL: "https://one.example", AccessKey: "key with space"}} {
		if err := invalid.Validate(); err == nil {
			t.Fatalf("invalid profile accepted: %q", invalid.PublicURL)
		}
	}
	if _, err := PublicURLs([]Candidate{{PublicURL: "https://same.example"}, {PublicURL: "https://same.example", AccessKey: "other"}}); err == nil {
		t.Fatal("duplicate relay authority accepted")
	}
}
