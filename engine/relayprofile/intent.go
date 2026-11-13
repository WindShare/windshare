package relayprofile

import "slices"

type IntentMode uint8

const (
	Planned IntentMode = iota
	ConfiguredOnly
	Explicit
)

// Intent separates a user's complete explicit set from configured candidates
// supplemented by the application network. The zero value requests planning.
type Intent struct {
	mode       IntentMode
	candidates []Candidate
}

func Plan(candidates []Candidate) Intent {
	return Intent{mode: Planned, candidates: slices.Clone(candidates)}
}
func OnlyConfigured(candidates []Candidate) Intent {
	return Intent{mode: ConfiguredOnly, candidates: slices.Clone(candidates)}
}
func UseExplicit(candidates []Candidate) Intent {
	return Intent{mode: Explicit, candidates: slices.Clone(candidates)}
}
func (i Intent) Mode() IntentMode        { return i.mode }
func (i Intent) Candidates() []Candidate { return slices.Clone(i.candidates) }
