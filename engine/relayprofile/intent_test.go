package relayprofile

import (
	"errors"
	"testing"
)

func TestIntentRequiresCandidatesOnlyWhenPlanningIsExcluded(t *testing.T) {
	candidates := []Candidate{{PublicURL: "https://relay.example"}}
	for _, test := range []struct {
		name   string
		intent Intent
		want   error
	}{
		{"default planning", Intent{}, nil},
		{"empty planning", Plan(nil), nil},
		{"configured planning", Plan(candidates), nil},
		{"empty configured only", OnlyConfigured(nil), ErrCandidatesRequired},
		{"configured only", OnlyConfigured(candidates), nil},
		{"empty explicit", UseExplicit(nil), ErrCandidatesRequired},
		{"explicit", UseExplicit(candidates), nil},
	} {
		t.Run(test.name, func(t *testing.T) {
			if err := test.intent.Validate(); !errors.Is(err, test.want) {
				t.Fatalf("Validate() = %v, want %v", err, test.want)
			}
		})
	}
	for _, intent := range []Intent{
		Plan([]Candidate{{PublicURL: "invalid"}}),
		UseExplicit([]Candidate{candidates[0], candidates[0]}),
	} {
		if err := intent.Validate(); err == nil || errors.Is(err, ErrCandidatesRequired) {
			t.Fatalf("invalid candidates were misclassified: %v", err)
		}
	}
}
