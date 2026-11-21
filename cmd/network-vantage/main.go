// network-vantage polls the authority from one enrolled network zone.
package main

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"time"

	"github.com/windshare/windshare/core/nodedirectory"
	"github.com/windshare/windshare/engine/builtinnetwork"
	"github.com/windshare/windshare/engine/networkconfig"
	"github.com/windshare/windshare/internal/networkprobe"
	v2 "github.com/windshare/windshare/relay/protocol/v2"
)

const (
	pollInterval = time.Minute
	checkTimeout = 45 * time.Second
	pollDomain   = "windshare/vantage-poll/v1\x00"
	resultDomain = "windshare/vantage-result/v1\x00"
)

type agent struct {
	key      ed25519.PrivateKey
	config   *networkconfig.Config
	client   *http.Client
	mainland bool
}

func main() {
	keyPath := flag.String("key", "", "probe-only Ed25519 PKCS8 PEM")
	mainland := flag.Bool("mainland", false, "check the split-DNS mainland frontend mirror")
	flag.Parse()
	slog.SetDefault(slog.New(slog.NewJSONHandler(os.Stderr, nil)))
	err := run(*keyPath, *mainland)
	if err != nil {
		slog.Error("vantage stopped", "error", err)
		os.Exit(1)
	}
}

func run(path string, mainland bool) error {
	raw, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	block, _ := pem.Decode(raw)
	if block == nil {
		return errors.New("expected PKCS8 probe key")
	}
	private, err := x509.ParsePKCS8PrivateKey(block.Bytes)
	if err != nil {
		return err
	}
	key, ok := private.(ed25519.PrivateKey)
	if !ok {
		return errors.New("expected Ed25519 probe key")
	}
	config, err := builtinnetwork.Config("")
	if err != nil {
		return err
	}
	if config.Authority == nil {
		return errors.New("release does not specify an authority")
	}
	a := agent{key, config, networkprobe.Client(), mainland}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()
	ticks := time.NewTicker(pollInterval)
	defer ticks.Stop()
	for {
		if err := a.cycle(ctx); err != nil {
			slog.Warn("vantage poll failed", "error", err)
		}
		select {
		case <-ctx.Done():
			return nil
		case <-ticks.C:
		}
	}
}

func (a *agent) mirror(ctx context.Context) *string {
	if !a.mainland {
		return nil
	}
	data, err := networkprobe.Fetch(ctx, a.client, a.config.LinkHost+"/.well-known/windshare-network", nodedirectory.MaximumEnvelopeBytes)
	if err != nil {
		return nil
	}
	dir := a.config.Directory
	snapshot, err := nodedirectory.Verify(data, dir.Network, dir.Roots)
	if err != nil || !snapshot.Usable(time.Now(), 0) {
		return nil
	}
	digest := snapshot.Cursor().Digest
	value := base64.RawURLEncoding.EncodeToString(digest[:])
	return &value
}

func (a *agent) cycle(ctx context.Context) error {
	var shares [networkprobe.OriginCount]networkprobe.SyntheticShare
	var ids [networkprobe.OriginCount]v2.ShareID
	for i := range shares {
		share, err := networkprobe.NewShare()
		if err != nil {
			return err
		}
		shares[i] = share
		ids[i] = share.ID()
	}
	mirror := a.mirror(ctx)
	var work *networkprobe.Work
	if err := a.post(ctx, "poll", pollDomain, map[string]any{"at": time.Now().Unix(), "shares": ids, "mirror": mirror}, &work); err != nil || work == nil {
		return err
	}
	probe, cancel := context.WithTimeout(ctx, checkTimeout)
	failure := networkprobe.Check(probe, a.client, *work, shares)
	cancel()
	var reason *string
	if failure != nil {
		message := failure.Error()
		if len(message) > 240 {
			message = message[:240]
		}
		reason = &message
	}
	slog.Info("enrollment verification", "operation_id", work.OperationID, "node_id", work.Node.NodeKey, "error", failure)
	return a.post(ctx, "result", resultDomain, map[string]any{"at": time.Now().Unix(), "lease": work.Lease, "node_key": work.Node.NodeKey, "error": reason}, nil)
}

func (a *agent) post(ctx context.Context, path, domain string, body, out any) error {
	payload, err := json.Marshal(body)
	if err != nil {
		return err
	}
	b64 := base64.RawURLEncoding
	proof, err := json.Marshal(map[string]string{"key": b64.EncodeToString(a.key.Public().(ed25519.PublicKey)), "payload": b64.EncodeToString(payload), "signature": b64.EncodeToString(ed25519.Sign(a.key, append([]byte(domain), payload...)))})
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, a.config.Authority.URL+"/v1/vantage/"+path, bytes.NewReader(proof))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	res, err := a.client.Do(req)
	if err != nil {
		return err
	}
	defer res.Body.Close()
	if res.StatusCode != http.StatusOK {
		return fmt.Errorf("authority %s returned HTTP %d", path, res.StatusCode)
	}
	if out == nil {
		return nil
	}
	const limit = 32 << 10
	data, err := io.ReadAll(io.LimitReader(res.Body, limit+1))
	if err != nil {
		return err
	}
	if len(data) > limit {
		return errors.New("oversized probe assignment")
	}
	return json.Unmarshal(data, out)
}
