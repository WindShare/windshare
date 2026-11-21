package networkprobe

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"slices"
	"strings"
	"time"

	"github.com/windshare/windshare/core/networkendpoint"
	"github.com/windshare/windshare/core/nodedirectory"
	"github.com/windshare/windshare/core/serviceauthority"
)

const OriginCount = 3

type Work struct {
	OperationID string             `json:"operation_id"`
	Lease       string             `json:"lease"`
	Node        nodedirectory.Node `json:"spec"`
	Origins     []string           `json:"origins"`
	Tickets     []string           `json:"tickets"`
}

func Check(ctx context.Context, client *http.Client, work Work, shares [OriginCount]SyntheticShare) error {
	return check(ctx, client, work, shares, binding)
}

func check(ctx context.Context, client *http.Client, work Work, shares [OriginCount]SyntheticShare, bind func(context.Context, string) error) error {
	node := work.Node
	if len(node.Roles) == 0 {
		return errors.New("node has no roles")
	}
	for _, role := range node.Roles {
		if role != "relay" && role != "stun" {
			return fmt.Errorf("unknown node role %q", role)
		}
	}
	base, err := url.Parse(node.Relay)
	if err != nil || base.Scheme != "https" || base.Hostname() == "" || base.User != nil || base.RawQuery != "" || base.Fragment != "" || base.Path != "" {
		return errors.New("invalid enrolled relay origin")
	}
	key, err := serviceauthority.PublicKey(node.NodeKey)
	if err != nil {
		return err
	}
	health, err := Fetch(ctx, client, node.Relay+"/healthz", 128)
	if err != nil {
		return fmt.Errorf("health: %w", err)
	}
	if strings.TrimSpace(string(health)) != "ok" {
		return errors.New("node health check failed")
	}
	body, err := Fetch(ctx, client, node.Relay+"/.well-known/windshare", serviceauthority.MaximumDescriptorBytes)
	if err != nil {
		return err
	}
	descriptor, err := serviceauthority.VerifyDescriptor(body, key, time.Now())
	if err != nil {
		return fmt.Errorf("descriptor: %w", err)
	}
	if descriptor.RelayBaseURL() != node.Relay {
		return errors.New("node descriptor relay mismatch")
	}
	if node.HasRole("stun") {
		if !descriptor.Services.STUN || len(node.STUN) == 0 {
			return errors.New("STUN unavailable")
		}
		for _, raw := range node.STUN {
			address := strings.TrimPrefix(raw, "stun:")
			host, _, err := net.SplitHostPort(address)
			canonical, normErr := networkendpoint.NormalizeSTUN(raw)
			if err != nil || normErr != nil || canonical != raw || host != base.Hostname() || !slices.Contains(descriptor.STUN, raw) {
				return errors.New("STUN endpoint mismatch")
			}
			if err := bind(ctx, address); err != nil {
				return fmt.Errorf("STUN binding: %w", err)
			}
		}
	}
	if node.HasRole("relay") {
		if !descriptor.Services.Relay || len(work.Origins) != OriginCount || len(work.Tickets) != OriginCount {
			return errors.New("missing relay checks")
		}
		for i, origin := range work.Origins {
			if err := shares[i].exchange(ctx, client, node.Relay, origin, work.Tickets[i]); err != nil {
				return fmt.Errorf("relay (%s): %w", origin, err)
			}
		}
	}
	return nil
}
