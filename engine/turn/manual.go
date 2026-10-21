package turn

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"slices"
	"time"

	"github.com/windshare/windshare/connectivity/iceconfig"
	"github.com/windshare/windshare/engine/turn/internal/provider"
)

const DelegationNotice = "TURN credentials are shared with authenticated recipients. They can reuse them until provider expiry or confirmed revocation; fixed passwords have no WindShare-enforced expiry. Issuer secrets remain on this device."

const (
	StaticIssuer     = "static"
	CloudflareIssuer = "cloudflare"
	CoturnIssuer     = "coturn"
)

// ManualSettings is the persisted, device-owned part of TURN settings.
type ManualSettings struct {
	Accounts []AccountSettings `json:"accounts"`
	Profiles []ProfileSettings `json:"profiles"`
}

type AccountSettings struct {
	ID               string               `json:"id"`
	Provider         string               `json:"provider"`
	Issuer           string               `json:"issuer"`
	LocalBudgetBytes uint64               `json:"local_budget_bytes"`
	Credentials      json.RawMessage      `json:"credentials"`
	Analytics        *CloudflareAnalytics `json:"analytics,omitempty"`
}
type CloudflareAnalytics struct {
	AccountID string `json:"account_id"`
	APIToken  string `json:"api_token"`
}
type ProfileSettings struct {
	ID      string   `json:"id"`
	Account string   `json:"account"`
	URLs    []string `json:"urls"`
}
type StaticCredentials struct {
	Username  string     `json:"username"`
	Password  string     `json:"password"`
	ExpiresAt *time.Time `json:"expires_at,omitempty"`
}
type CloudflareCredentials struct {
	KeyID           string `json:"key_id"`
	APIToken        string `json:"api_token"`
	LifetimeSeconds uint32 `json:"lifetime_seconds,omitempty"`
}
type CoturnCredentials struct {
	SharedSecret    string `json:"shared_secret"`
	LifetimeSeconds uint32 `json:"lifetime_seconds,omitempty"`
}

func (settings ManualSettings) Clone() ManualSettings {
	settings.Accounts = slices.Clone(settings.Accounts)
	for index := range settings.Accounts {
		account := &settings.Accounts[index]
		account.Credentials = slices.Clone(account.Credentials)
		if account.Analytics != nil {
			value := *account.Analytics
			account.Analytics = &value
		}
	}
	settings.Profiles = slices.Clone(settings.Profiles)
	for index := range settings.Profiles {
		settings.Profiles[index].URLs = slices.Clone(settings.Profiles[index].URLs)
	}
	return settings
}

type sourceFactory func(Dependencies) (credentialSource, error)
type manualPlan struct {
	definition accountConfig
	sources    []sourceFactory
	analytics  []*provider.CloudflareUsageConfig
}

func (settings ManualSettings) Validate() error {
	_, err := settings.prepare()
	return err
}

// Preparation parses secrets and checks relationships without creating clients,
// account owners or leases. Binding dependencies is a separate runtime step.
func (settings ManualSettings) prepare() (manualPlan, error) {
	result := manualPlan{}
	if len(settings.Accounts) > MaximumAccounts || len(settings.Profiles) > MaximumProfiles {
		return result, ErrConfiguration
	}
	issuers := make(map[string]string, len(settings.Accounts))
	for _, setting := range settings.Accounts {
		factory, analytics, err := setting.prepareIssuer()
		if err != nil {
			return manualPlan{}, errors.Join(ErrConfiguration, err)
		}
		result.definition.Accounts = append(result.definition.Accounts, account{
			ID: setting.ID, Provider: setting.Provider, LocalBudgetBytes: setting.LocalBudgetBytes})
		result.sources = append(result.sources, factory)
		result.analytics = append(result.analytics, analytics)
		issuers[setting.ID] = setting.Issuer
	}
	for _, setting := range settings.Profiles {
		if len(setting.URLs) == 0 && issuers[setting.Account] != CloudflareIssuer {
			return manualPlan{}, ErrConfiguration
		}
		result.definition.Profiles = append(result.definition.Profiles, profile{
			ID: setting.ID, AccountID: setting.Account, URLs: setting.URLs})
	}
	if _, err := prepareDefinitions(result.definition); err != nil {
		return manualPlan{}, err
	}
	return result, nil
}

func (plan manualPlan) bind(dependencies Dependencies) (accountConfig, error) {
	result := cloneConfig(plan.definition)
	for index := range result.Accounts {
		source, err := plan.sources[index](dependencies)
		if err != nil {
			return accountConfig{}, errors.Join(ErrConfiguration, err)
		}
		result.Accounts[index].Source = source
		if analytics := plan.analytics[index]; analytics != nil {
			config := *analytics
			config.Client = dependencies.Client
			reporter, err := provider.NewCloudflareUsage(config)
			if err != nil {
				return accountConfig{}, errors.Join(ErrConfiguration, err)
			}
			result.Accounts[index].Usage = reporter
		}
	}
	return result, nil
}

func (setting AccountSettings) prepareIssuer() (sourceFactory, *provider.CloudflareUsageConfig, error) {
	if setting.Analytics != nil && setting.Issuer != CloudflareIssuer {
		return nil, nil, ErrConfiguration
	}
	switch setting.Issuer {
	case StaticIssuer:
		var value StaticCredentials
		if err := decode(setting.Credentials, &value); err != nil {
			return nil, nil, err
		}
		credential, err := iceconfig.Fixed(value.Username, value.Password)
		if value.ExpiresAt != nil {
			credential, err = iceconfig.Expiring(value.Username, value.Password, *value.ExpiresAt)
		}
		if err != nil {
			return nil, nil, err
		}
		return func(dependencies Dependencies) (credentialSource, error) {
			return provider.NewStatic(credential, dependencies.Now)
		}, nil, nil
	case CloudflareIssuer:
		var value CloudflareCredentials
		if err := decode(setting.Credentials, &value); err != nil {
			return nil, nil, err
		}
		config := provider.CloudflareConfig{KeyID: value.KeyID, APIToken: value.APIToken, Lifetime: time.Duration(value.LifetimeSeconds) * time.Second}
		if err := config.Validate(); err != nil {
			return nil, nil, err
		}
		var analytics *provider.CloudflareUsageConfig
		if setting.Analytics != nil {
			analytics = &provider.CloudflareUsageConfig{AccountID: setting.Analytics.AccountID, KeyID: value.KeyID, APIToken: setting.Analytics.APIToken}
			if err := analytics.Validate(); err != nil {
				return nil, nil, err
			}
		}
		return func(dependencies Dependencies) (credentialSource, error) {
			bound := config
			bound.Client, bound.Now = dependencies.Client, dependencies.Now
			return provider.NewCloudflare(bound)
		}, analytics, nil
	case CoturnIssuer:
		var value CoturnCredentials
		if err := decode(setting.Credentials, &value); err != nil {
			return nil, nil, err
		}
		config := provider.CoturnConfig{SharedSecret: value.SharedSecret, Lifetime: time.Duration(value.LifetimeSeconds) * time.Second}
		if err := config.Validate(); err != nil {
			return nil, nil, err
		}
		return func(dependencies Dependencies) (credentialSource, error) {
			bound := config
			bound.Now = dependencies.Now
			return provider.NewCoturn(bound)
		}, nil, nil
	default:
		return nil, nil, ErrConfiguration
	}
}

func decode(body []byte, target any) error {
	if len(body) == 0 || bytes.Equal(bytes.TrimSpace(body), []byte("null")) {
		return ErrConfiguration
	}
	decoder := json.NewDecoder(bytes.NewReader(body))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(target); err != nil {
		return err
	}
	var trailing any
	if err := decoder.Decode(&trailing); err != io.EOF {
		return ErrConfiguration
	}
	return nil
}
