package main

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"time"

	"github.com/windshare/windshare/engine/relayconfig"
	"github.com/windshare/windshare/engine/turnprovider"
)

type issuer interface {
	Issue(context.Context, turnprovider.Subject) (turnprovider.Issued, error)
	Revoke(context.Context, turnprovider.Issued) error
}

func trialIssuer() (issuer, error) {
	if path := os.Getenv("WINDSHARE_TURN_TRIAL_CONFIGURATION"); path != "" {
		config, err := (relayconfig.FileStore{Path: path}).Load()
		if err != nil {
			return nil, err
		}
		if len(config.Profiles) != 1 {
			return nil, errors.New("trial requires one enrolled deployment")
		}
		profile := config.Profiles[0]
		capability, err := turnprovider.Discover(context.Background(), nil, profile.PublicURL, profile.AccessKey)
		if err != nil {
			return nil, err
		}
		if capability == nil || !capability.Enabled {
			return nil, errors.New("managed TURN unavailable")
		}
		return turnprovider.NewRemote(turnprovider.RemoteConfig{
			RelayURL: profile.PublicURL, AccessKey: profile.AccessKey, ServiceID: capability.ServiceID,
		})
	}
	data, err := os.ReadFile(os.Getenv("WINDSHARE_TURN_TRIAL_KEY"))
	if err != nil {
		return nil, err
	}
	var key struct {
		ID     string `json:"uid"`
		Secret string `json:"secret"`
	}
	if err := json.Unmarshal(data, &key); err != nil {
		return nil, err
	}
	return turnprovider.NewCloudflare(turnprovider.CloudflareConfig{
		KeyID: key.ID, APIToken: key.Secret, Lifetime: 5 * time.Minute,
	})
}

func (p *probe) issue(ctx context.Context, id, previous byte) (turnprovider.Issued, error) {
	subject := turnprovider.Subject{SessionID: [16]byte{1}, PathID: [16]byte{id}}
	if previous != 0 {
		if source, ok := p.source.(interface {
			Renew(context.Context, turnprovider.Subject, turnprovider.Issued) (turnprovider.Issued, error)
		}); ok {
			p.mu.Lock()
			old := p.issued[previous]
			p.mu.Unlock()
			return source.Renew(ctx, subject, old)
		}
	}
	return p.source.Issue(ctx, subject)
}
