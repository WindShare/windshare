// Package platformsetup separates installation history, executable-scoped rule
// inspection, and rule writes. None of them asserts actual ICE reachability.
package platformsetup

type Choice uint8

const (
	Ask Choice = iota
	Configure
	Skip
)

func (v Choice) String() string { return enumName(int(v), "ask", "configure", "skip") }
func (v Choice) Valid() bool    { return v <= Skip }

type Profile uint8

const (
	UnknownProfile Profile = iota
	Domain
	Private
	Public
)

func (v Profile) String() string { return enumName(int(v), "unknown", "domain", "private", "public") }

type Protocol uint8

const (
	AnyProtocol Protocol = iota
	UDP
	TCP
)

func (v Protocol) String() string { return enumName(int(v), "any", "UDP", "TCP") }

type InboundAction uint8

const (
	UnknownAction InboundAction = iota
	Allow
	Block
)

func (v InboundAction) String() string { return enumName(int(v), "unknown", "allow", "block") }

// InspectionScope excludes all-program rules; even RulePresent is not evidence
// that the host's complete effective policy allows this executable.
const InspectionScope = "executable-inbound-rules"

type RuleStatus uint8

const (
	UnknownStatus RuleStatus = iota
	RulePresent
	RuleMissing
	RuleRestricted
	PolicyRestricted
	RuleNotRequired
)

func (v RuleStatus) String() string {
	return enumName(int(v), "unknown", "rule-present", "rule-missing", "rule-restricted", "policy-restricted", "rule-not-required")
}

type Reason uint8

const (
	IncompleteEvidence Reason = iota
	UnsupportedPlatform
	ReadFailed
	NoActiveNetwork
	FirewallDisabled
	InterfaceExempt
	InboundRulesDisabled
	ExplicitBlock
	ConditionalRule
	ExplicitAllow
	DefaultAllow
	NoMatchingRule
	LocalRulesDisabled
)

func (v Reason) String() string {
	return enumName(int(v), "incomplete-evidence", "unsupported-platform", "read-failed",
		"no-active-network", "firewall-disabled", "interface-exempt", "inbound-rules-disabled",
		"explicit-block", "conditional-rule", "explicit-allow", "default-allow", "no-matching-rule", "local-rules-disabled")
}

func enumName(index int, names ...string) string {
	if index < 0 || index >= len(names) {
		return "invalid"
	}
	return names[index]
}

// Setting preserves missing evidence instead of silently interpreting it as false.
type Setting uint8

const (
	Unspecified Setting = iota
	Yes
	No
)

type Network struct {
	Interface string
	Profile   Profile
}

type ProfilePolicy struct {
	Profile            Profile
	Enabled            Setting
	AllowInboundRules  Setting
	AllowLocalRules    Setting
	DefaultInbound     InboundAction
	DisabledInterfaces []string
}

// The adapter selects explicit executable paths. Restricted rules are retained
// for diagnosis without attempting to evaluate their arbitrary traffic selectors.
type Rule struct {
	Name         string
	Profiles     []Profile
	Protocol     Protocol
	Action       InboundAction
	Local        bool
	Conditional  bool
	Restrictions string
}

type Snapshot struct {
	Networks []Network
	Profiles []ProfilePolicy
	Rules    []Rule
}

type Finding struct {
	Network  Network
	Protocol Protocol
	Status   RuleStatus
	Reason   Reason
	Rules    []Rule
}

func (f Finding) SuggestSetup() bool { return f.Status == RuleMissing }
