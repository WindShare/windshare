package relayconfig

import (
	"errors"
	"fmt"
	"slices"

	"github.com/windshare/windshare/engine/relayprofile"
	v2 "github.com/windshare/windshare/relay/protocol/v2"
)

type RelayView struct {
	URL             string `json:"url"`
	Enabled         bool   `json:"enabled"`
	ConsoleURL      string `json:"console_url,omitempty"`
	OfferedFrontend string `json:"offered_frontend,omitempty"`
}

type Preferences struct {
	CommunityNotice uint64      `json:"community_notice,omitempty"`
	CommunityNodes  bool        `json:"community_nodes"`
	Mode            string      `json:"mode"`
	Frontend        string      `json:"frontend"`
	CustomFrontend  bool        `json:"custom_frontend"`
	Relays          []RelayView `json:"relays"`
	AtCapacity      bool        `json:"at_capacity"`
}

const (
	Automatic = "automatic"
	Selected  = "selected"
)

func matches(a, b string) bool {
	left, err := v2.NormalizeRelayEndpoint(a)
	right, other := v2.NormalizeRelayEndpoint(b)
	return err == nil && other == nil && left.DialURL == right.DialURL
}

func (c Config) enabledIndex(address string) int {
	return slices.IndexFunc(c.ActiveRelays, func(url string) bool { return matches(url, address) })
}

func (c Config) profileIndex(address string) int {
	return slices.IndexFunc(c.Profiles, func(p relayprofile.Candidate) bool { return matches(p.PublicURL, address) })
}

func (c Config) Preferences(defaultFrontend string) Preferences {
	view := Preferences{CommunityNodes: c.CommunityEnabled(), Mode: Automatic, Frontend: c.FrontURL, CustomFrontend: c.FrontURL != "", Relays: []RelayView{}}
	if c.ConfiguredOnly {
		view.Mode = Selected
	}
	if view.Frontend == "" {
		view.Frontend = defaultFrontend
	}
	view.AtCapacity = len(c.ActiveRelays) == relayprofile.MaximumEndpoints
	for _, p := range c.Profiles {
		view.Relays = append(view.Relays, RelayView{URL: p.PublicURL, Enabled: c.enabledIndex(p.PublicURL) >= 0,
			ConsoleURL: p.ConsoleURL, OfferedFrontend: p.OfferedFrontend})
	}
	return view
}

// CredentialProfile updates only the secret for an existing endpoint. A manual
// credential carries no discovery metadata and cannot withdraw a published capability.
func (c Config) CredentialProfile(address, key string) relayprofile.Candidate {
	p := relayprofile.Candidate{PublicURL: address}
	if index := c.profileIndex(address); index >= 0 {
		p = c.Profiles[index]
	}
	p.AccessKey = key
	p.Role = relayprofile.Service
	return p
}

// RefreshMetadata preserves the device identity across capability rediscovery.
func (c Config) RefreshMetadata(p relayprofile.Candidate) (Config, error) {
	if i := c.profileIndex(p.PublicURL); i >= 0 {
		previous := c.Profiles[i]
		if previous.Node.NodeKey != "" && p.Node.NodeKey != previous.Node.NodeKey && previous.AccessKey != "" {
			return Config{}, fmt.Errorf("%w: enrolled relay identity changed", ErrConfig)
		}
		p.AccessKey, p.Role = previous.AccessKey, previous.Role
	}
	return c.ReplaceIdentity(p)
}

func (c Config) CommunityEnabled() bool { return c.CommunityNodes == nil || *c.CommunityNodes }

func (c Config) ReplaceIdentity(p relayprofile.Candidate) (Config, error) {
	if err := p.Validate(); err != nil {
		return Config{}, err
	}
	index := c.profileIndex(p.PublicURL)
	c.Profiles = slices.Clone(c.Profiles)
	if index < 0 {
		if len(c.Profiles) >= MaximumProfiles {
			return Config{}, fmt.Errorf("%w: limit of %d relay profiles reached", ErrConfig, MaximumProfiles)
		}
		c.Profiles = append(c.Profiles, p)
		// Newly added profiles start enabled whenever a slot is available,
		// independently of whether automatic built-in relays are used.
		if len(c.ActiveRelays) < relayprofile.MaximumEndpoints {
			c.ActiveRelays = append(slices.Clone(c.ActiveRelays), p.PublicURL)
		}
	} else {
		c.Profiles[index] = p
	}
	return c, c.Validate()
}

func (c *Config) SetMode(mode string) error {
	switch mode {
	case Automatic:
		c.ConfiguredOnly = false
	case Selected:
		c.ConfiguredOnly = true
	default:
		return errors.New("choose automatic or selected relay mode")
	}
	return nil
}

func (c *Config) SetEnabled(address string, enabled bool) error {
	index := c.profileIndex(address)
	if index < 0 {
		return errors.New("relay is not saved")
	}
	active := c.enabledIndex(address)
	switch {
	case enabled && active < 0:
		if len(c.ActiveRelays) >= relayprofile.MaximumEndpoints {
			return fmt.Errorf("select at most %d relays", relayprofile.MaximumEndpoints)
		}
		c.ActiveRelays = append(slices.Clone(c.ActiveRelays), c.Profiles[index].PublicURL)
	case !enabled && active >= 0:
		c.ActiveRelays = slices.Delete(slices.Clone(c.ActiveRelays), active, active+1)
	}
	return nil
}

func (c *Config) RemoveRelay(address string) error {
	index := c.profileIndex(address)
	if index < 0 {
		return errors.New("relay is not saved")
	}
	c.Profiles = slices.Delete(slices.Clone(c.Profiles), index, index+1)
	if active := c.enabledIndex(address); active >= 0 {
		c.ActiveRelays = slices.Delete(slices.Clone(c.ActiveRelays), active, active+1)
	}
	return nil
}

type PreferenceEdit struct {
	Action string `json:"action"`
	Value  string `json:"value"`
}

func (e PreferenceEdit) Apply(c *Config) error {
	switch e.Action {
	case "community_nodes":
		switch e.Value {
		case "on", "off":
			enabled := e.Value == "on"
			c.CommunityNodes = &enabled
			return nil
		default:
			return errors.New("choose on or off for community nodes")
		}
	case "mode":
		return c.SetMode(e.Value)
	case "enable", "disable":
		return c.SetEnabled(e.Value, e.Action == "enable")
	case "remove":
		return c.RemoveRelay(e.Value)
	case "frontend":
		if e.Value != "" {
			if err := ValidateFrontURL(e.Value); err != nil {
				return err
			}
		}
		c.FrontURL = e.Value
		return nil
	default:
		return errors.New("unknown sharing preference")
	}
}
