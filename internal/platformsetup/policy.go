// Package platformsetup separates a user's setup choice from current host policy.
// Its reports describe inbound policy, never the outcome of an ICE connection.
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
func (v Profile) Valid() bool    { return v <= Public }

type Protocol uint8

const (
	AnyProtocol Protocol = iota
	UDP
	TCP
)

func (v Protocol) String() string { return enumName(int(v), "any", "UDP", "TCP") }
func (v Protocol) Valid() bool    { return v == UDP || v == TCP }

type Verdict uint8

const (
	Unknown Verdict = iota
	Allowed
	Blocked
)

func (v Verdict) String() string { return enumName(int(v), "unknown", "allowed", "blocked") }
func (v Verdict) Valid() bool    { return v <= Blocked }

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
	DefaultBlock
	LocalRulesDisabled
)

func (v Reason) String() string {
	return enumName(int(v), "incomplete-evidence", "unsupported-platform", "policy-read-failed",
		"no-active-network", "firewall-disabled", "interface-exempt", "inbound-rules-disabled",
		"explicit-block", "conditional-rule", "explicit-allow", "default-allow", "default-block", "local-rules-disabled")
}
func (v Reason) Valid() bool { return v <= LocalRulesDisabled }

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
	Interface      string
	Profile        Profile
	LocalAddresses []string
}

type ProfilePolicy struct {
	Profile            Profile
	Enabled            Setting
	AllowInboundRules  Setting
	AllowLocalRules    Setting
	DefaultInbound     Verdict
	DisabledInterfaces []string
}

// Rules are already scoped to this executable by the platform adapter. Conditional
// rules retain their restrictions because peer addresses and ICE ports are not known.
type Rule struct {
	Name            string
	Profiles        []Profile
	Protocol        Protocol
	Action          Verdict
	Local           bool
	Conditional     bool
	Restrictions    string
	Interfaces      []string
	LocalAddresses  []string
	OverridesBlocks bool
}

type Snapshot struct {
	Networks []Network
	Profiles []ProfilePolicy
	Rules    []Rule
}

type Finding struct {
	Network  Network
	Protocol Protocol
	Verdict  Verdict
	Reason   Reason
	Rules    []Rule
}

func (f Finding) Repairable() bool { return f.Verdict == Blocked && f.Reason == DefaultBlock }
