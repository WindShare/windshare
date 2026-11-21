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
	"sync"
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
	origin, err := relayOrigin(node.Relay)
	if err != nil {
		return err
	}
	node.Relay = origin.String()
	descriptor, err := descriptorAt(ctx, client, node)
	if err != nil {
		return err
	}
	if node.HasRole("stun") {
		if !descriptor.Services.STUN || len(node.STUN) == 0 {
			return errors.New("STUN unavailable")
		}
		for _, address := range node.STUN {
			if !slices.Contains(descriptor.STUN, address) {
				return errors.New("STUN endpoint mismatch")
			}
		}
		if err := checkSTUN(ctx, origin.Hostname(), node.STUN, bind); err != nil {
			return err
		}
	}
	if node.HasRole("relay") {
		if !descriptor.Services.Relay || len(work.Origins) != OriginCount || len(work.Tickets) != OriginCount {
			return errors.New("missing relay checks")
		}
		if err := checkRelayOrigins(ctx, work.Origins, work.Tickets, shares, func(originCtx context.Context, share SyntheticShare, origin, ticket string) error {
			return share.exchange(originCtx, client, node.Relay, origin, ticket)
		}); err != nil {
			return err
		}
	}
	return nil
}

type originExchange func(context.Context, SyntheticShare, string, string) error

// Run the independent origin probes concurrently so cross-border RTTs do not
// multiply sequentially across tight upgrade budgets. Cancel siblings on the
// first failure while holding the lock so the root-cause error is preserved,
// and wait for every goroutine so sibling STOP cleanups finish before return.
func checkRelayOrigins(
	ctx context.Context,
	origins, tickets []string,
	shares [OriginCount]SyntheticShare,
	exchange originExchange,
) error {
	if len(origins) != OriginCount || len(tickets) != OriginCount {
		return errors.New("missing relay checks")
	}
	groupCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	var (
		wg       sync.WaitGroup
		firstMu  sync.Mutex
		firstErr error
	)
	for i, origin := range origins {
		wg.Go(func() {
			err := exchange(groupCtx, shares[i], origin, tickets[i])
			if err == nil {
				return
			}
			firstMu.Lock()
			if firstErr == nil {
				firstErr = fmt.Errorf("relay (%s): %w", origin, err)
				cancel()
			}
			firstMu.Unlock()
		})
	}
	wg.Wait()
	return firstErr
}

func relayOrigin(raw string) (*url.URL, error) {
	base, err := parseHTTPSOrigin(raw)
	if err != nil {
		return nil, errors.New("invalid enrolled relay origin")
	}
	return base, nil
}

func descriptorAt(ctx context.Context, client *http.Client, node nodedirectory.Node) (serviceauthority.Descriptor, error) {
	var empty serviceauthority.Descriptor
	key, err := serviceauthority.PublicKey(node.NodeKey)
	if err != nil {
		return empty, err
	}
	health, err := Fetch(ctx, client, node.Relay+"/healthz", 128)
	if err != nil {
		return empty, fmt.Errorf("health: %w", err)
	}
	if strings.TrimSpace(string(health)) != "ok" {
		return empty, errors.New("node health check failed")
	}
	body, err := Fetch(ctx, client, node.Relay+"/.well-known/windshare", serviceauthority.MaximumDescriptorBytes)
	if err != nil {
		return empty, err
	}
	descriptor, err := serviceauthority.VerifyDescriptor(body, key, time.Now())
	if err != nil {
		return empty, fmt.Errorf("descriptor: %w", err)
	}
	if descriptor.RelayBaseURL() != node.Relay {
		return empty, errors.New("node descriptor relay mismatch")
	}
	return descriptor, nil
}

func checkSTUN(ctx context.Context, hostname string, endpoints []string, bind func(context.Context, string) error) error {
	if len(endpoints) == 0 {
		return errors.New("STUN unavailable")
	}
	for _, raw := range endpoints {
		address := strings.TrimPrefix(raw, "stun:")
		host, _, err := net.SplitHostPort(address)
		canonical, normErr := networkendpoint.NormalizeSTUN(raw)
		if err != nil || normErr != nil || canonical != raw || host != hostname {
			return errors.New("STUN endpoint mismatch")
		}
		if err := bind(ctx, address); err != nil {
			return fmt.Errorf("STUN binding: %w", err)
		}
	}
	return nil
}
