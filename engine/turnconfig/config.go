// Package turnconfig validates owner-supplied TURN accounts and service profiles.
// Parsing never contacts a provider; credentials are acquired on content demand.
package turnconfig

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"slices"
	"time"

	"github.com/windshare/windshare/connectivity/iceconfig"
	"github.com/windshare/windshare/engine/turnaccount"
	"github.com/windshare/windshare/engine/turnprovider"
)

const DelegationNotice = "TURN credentials are shared with authenticated recipients. They can reuse them until provider expiry or confirmed revocation; fixed passwords have no WindShare-enforced expiry. Issuer secrets remain on this device."

var ErrConfiguration = errors.New("invalid TURN configuration")

type Config struct {
	Accounts []Account `json:"accounts"`
	Profiles []Profile `json:"profiles"`
}

func (config Config) Clone() Config {
	config.Accounts = slices.Clone(config.Accounts)
	for index := range config.Accounts {
		account := &config.Accounts[index]
		account.Credentials = slices.Clone(account.Credentials)
		if account.Analytics != nil {
			value := *account.Analytics
			account.Analytics = &value
		}
	}
	config.Profiles = slices.Clone(config.Profiles)
	for index := range config.Profiles {
		config.Profiles[index].URLs = slices.Clone(config.Profiles[index].URLs)
	}
	return config
}

type Account struct {
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
type Profile struct {
	ID      string   `json:"id"`
	Account string   `json:"account"`
	URLs    []string `json:"urls"`
}
type Dependencies struct {
	Client  turnprovider.HTTPClient
	Now     func() time.Time
	Observe func(turnaccount.Event)
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

func (config Config) Build(dependencies Dependencies) (turnaccount.Config, error) {
	result := turnaccount.Config{Now: dependencies.Now, Observe: dependencies.Observe}
	if len(config.Accounts) > turnaccount.MaximumAccounts || len(config.Profiles) > turnaccount.MaximumProfiles {
		return result, ErrConfiguration
	}
	for _, account := range config.Accounts {
		source, err := account.source(dependencies)
		if err != nil {
			return turnaccount.Config{}, errors.Join(ErrConfiguration, err)
		}
		configured := turnaccount.Account{ID: account.ID, Provider: account.Provider, LocalBudgetBytes: account.LocalBudgetBytes, Source: source}
		if account.Analytics != nil {
			if account.Issuer != "cloudflare" {
				return turnaccount.Config{}, ErrConfiguration
			}
			var credentials CloudflareCredentials
			if err := decode(account.Credentials, &credentials); err != nil {
				return turnaccount.Config{}, err
			}
			reporter, err := turnprovider.NewCloudflareUsage(turnprovider.CloudflareUsageConfig{AccountID: account.Analytics.AccountID,
				KeyID: credentials.KeyID, APIToken: account.Analytics.APIToken, Client: dependencies.Client})
			if err != nil {
				return turnaccount.Config{}, err
			}
			configured.Usage = reporter
		}
		result.Accounts = append(result.Accounts, configured)
	}
	for _, profile := range config.Profiles {
		if len(profile.URLs) == 0 {
			for _, account := range config.Accounts {
				if account.ID == profile.Account && account.Issuer != "cloudflare" {
					return turnaccount.Config{}, ErrConfiguration
				}
			}
		}
		result.Profiles = append(result.Profiles, turnaccount.Profile{ID: profile.ID, AccountID: profile.Account, URLs: profile.URLs})
	}
	owner, err := turnaccount.New(result)
	if err != nil {
		return turnaccount.Config{}, errors.Join(ErrConfiguration, err)
	}
	_ = owner.Close()
	return result, nil
}
func (config Config) Validate() error { _, err := config.Build(Dependencies{}); return err }
func (account Account) source(dependencies Dependencies) (turnaccount.Source, error) {
	switch account.Issuer {
	case "static":
		var value StaticCredentials
		if err := decode(account.Credentials, &value); err != nil {
			return nil, err
		}
		credential, err := iceconfig.Fixed(value.Username, value.Password)
		if value.ExpiresAt != nil {
			credential, err = iceconfig.Expiring(value.Username, value.Password, *value.ExpiresAt)
		}
		if err != nil {
			return nil, err
		}
		return turnprovider.NewStatic(credential, dependencies.Now)
	case "cloudflare":
		var value CloudflareCredentials
		if err := decode(account.Credentials, &value); err != nil {
			return nil, err
		}
		return turnprovider.NewCloudflare(turnprovider.CloudflareConfig{KeyID: value.KeyID, APIToken: value.APIToken,
			Lifetime: time.Duration(value.LifetimeSeconds) * time.Second, Client: dependencies.Client, Now: dependencies.Now})
	case "coturn":
		var value CoturnCredentials
		if err := decode(account.Credentials, &value); err != nil {
			return nil, err
		}
		return turnprovider.NewCoturn(turnprovider.CoturnConfig{SharedSecret: value.SharedSecret,
			Lifetime: time.Duration(value.LifetimeSeconds) * time.Second, Now: dependencies.Now})
	default:
		return nil, ErrConfiguration
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
