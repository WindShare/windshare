package turn

// An issuer owns one immutable signing identity. Configuration binding retains
// it when parsed issuer settings match, independently of budgets and analytics.
// Renewal material may only cross snapshots that retain this same owner.
type credentialIssuer struct {
	source     credentialSource
	definition any
}

type issuerFactory func(Dependencies, *credentialIssuer) (*credentialIssuer, error)

func planIssuer[Definition comparable](definition Definition, create func(Dependencies) (credentialSource, error)) issuerFactory {
	return func(dependencies Dependencies, previous *credentialIssuer) (*credentialIssuer, error) {
		if previous != nil {
			if prior, ok := previous.definition.(Definition); ok && prior == definition {
				return previous, nil
			}
		}
		source, err := create(dependencies)
		if err != nil {
			return nil, err
		}
		return &credentialIssuer{source: source, definition: definition}, nil
	}
}
