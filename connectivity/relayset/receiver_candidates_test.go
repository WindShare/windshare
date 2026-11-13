package relayset

import (
	"context"
	"encoding/base64"
	"fmt"
	"testing"
	"time"

	"github.com/windshare/windshare/core/link"
	"github.com/windshare/windshare/core/liveshare"
	v2 "github.com/windshare/windshare/relay/protocol/v2"
	"github.com/windshare/windshare/transport/relayv2"
)

func TestReceiverExpandsAliasesWithoutBlockingKnownDialAndOwnsCancellation(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	refresh := make(chan struct{})
	dialed := make(chan string, 2)
	raw := make([]byte, v2.ShareIDBytes)
	raw[0] = 1
	set, err := NewReceiver(ctx, ReceiverConfig{Receiver: liveshare.ReceiverConfig{Capacity: testReceiveCapacity(t),
		Capability: link.Link{ShareID: base64.RawURLEncoding.EncodeToString(raw), Relays: []string{"https://known.example"}}},
		ResolveEndpoints: func(ctx context.Context) ([]string, error) {
			select {
			case <-ctx.Done():
				return nil, ctx.Err()
			case <-refresh:
				return []string{"https://known.example/", "https://resolved.example"}, nil
			}
		},
		Dial: func(ctx context.Context, config relayv2.ReceiverConfig) (ReceiverConnection, error) {
			dialed <- config.RelayBaseURL
			<-ctx.Done()
			return nil, ctx.Err()
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	defer set.Close()
	select {
	case got := <-dialed:
		if got != "https://known.example" {
			t.Fatal(got)
		}
	case <-ctx.Done():
		t.Fatal("known endpoint waited for directory")
	}
	close(refresh)
	select {
	case got := <-dialed:
		if got != "https://resolved.example" {
			t.Fatal(got)
		}
	case <-ctx.Done():
		t.Fatal("resolved alias was not attempted")
	}
	set.Close()
	select {
	case duplicate := <-dialed:
		t.Fatal("duplicate canonical endpoint", duplicate)
	default:
	}
}

func TestExpandedEndpointsShareThePublishedLimit(t *testing.T) {
	var extra []string
	for index := range 8 {
		extra = append(extra, fmt.Sprintf("https://node%d.example", index))
	}
	if _, err := mergeEndpoints([]string{"https://configured.example"}, extra); err == nil {
		t.Fatal("nine endpoint expansion")
	}
	if _, err := mergeEndpoints(nil, []string{"invalid"}); err == nil {
		t.Fatal("invalid expanded endpoint")
	}
	if err := validateReceiverCandidates(ReceiverConfig{ResolveEndpoints: func(context.Context) ([]string, error) { return nil, nil }}); err != nil {
		t.Fatal("alias-only link", err)
	}
}
