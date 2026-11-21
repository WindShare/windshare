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

	"github.com/pion/stun/v4"
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
	node := work.Node
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
	if err != nil || descriptor.RelayBaseURL() != node.Relay {
		return errors.New("node descriptor mismatch")
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
			if err := binding(ctx, address); err != nil {
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

func binding(ctx context.Context, address string) error {
	ctx, cancel := context.WithTimeout(ctx, requestTimeout)
	defer cancel()
	conn, err := publicDial(ctx, "udp", address)
	if err != nil {
		return err
	}
	defer conn.Close()
	stop := context.AfterFunc(ctx, func() { _ = conn.Close() })
	defer stop()
	return exchangeBinding(conn)
}

func exchangeBinding(conn net.Conn) error {
	request, err := stun.Build(stun.TransactionID, stun.BindingRequest)
	if err != nil {
		return err
	}
	if _, err = conn.Write(request.Raw); err != nil {
		return err
	}
	var packet [2048]byte
	n, err := conn.Read(packet[:])
	if err != nil {
		return err
	}
	response := stun.Message{Raw: packet[:n]}
	if err := response.Decode(); err != nil {
		return err
	}
	var mapped stun.XORMappedAddress
	if response.Type != stun.BindingSuccess || response.TransactionID != request.TransactionID || mapped.GetFrom(&response) != nil {
		return errors.New("invalid binding reply")
	}
	return nil
}
