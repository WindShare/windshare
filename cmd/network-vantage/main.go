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
	"strings"
	"syscall"
	"time"
	"unicode/utf8"

	"github.com/windshare/windshare/core/link"
	"github.com/windshare/windshare/core/nodedirectory"
	"github.com/windshare/windshare/engine/account"
	"github.com/windshare/windshare/engine/networkconfig"
	"github.com/windshare/windshare/internal/networkprobe"
)

const (
	pollInterval          = time.Minute
	checkTimeout          = 45 * time.Second
	reportTimeout         = 3 * time.Second
	maxFailureReasonBytes = 240
	pollDomain            = "windshare/vantage-poll/v1\x00"
	resultDomain          = "windshare/vantage-result/v1\x00"
)

type agent struct {
	key           ed25519.PrivateKey
	config        *networkconfig.Config
	client        *http.Client
	authority     *http.Client
	now           func() time.Time
	lastClaimedAt int64
}

func main() {
	keyPath := flag.String("key", "", "probe-only Ed25519 PKCS8 PEM")
	releasePath := flag.String("release", "", "optional release manifest overriding the embedded network configuration")
	authority := flag.String("authority", "", "optional authority HTTPS origin overriding the release authority URL")
	flag.Parse()
	slog.SetDefault(slog.New(slog.NewJSONHandler(os.Stderr, nil)))
	err := run(*keyPath, *releasePath, *authority)
	if err != nil {
		slog.Error("vantage stopped", "error", err)
		os.Exit(1)
	}
}

func loadConfig(releasePath, authorityOverride string) (*networkconfig.Config, error) {
	config, err := networkprobe.LoadRelease(releasePath)
	if err != nil {
		return nil, err
	}
	authorityURL, err := networkprobe.ResolveAuthority(config, authorityOverride)
	if err != nil {
		return nil, err
	}
	if config.Authority == nil {
		config.Authority = &account.Config{URL: authorityURL}
	} else {
		config.Authority.URL = authorityURL
	}
	return config, nil
}

func run(path, releasePath, authorityOverride string) error {
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
	config, err := loadConfig(releasePath, authorityOverride)
	if err != nil {
		return err
	}
	slog.Info("vantage started", "authority", config.Authority.URL, "release_override", releasePath != "", "authority_override", authorityOverride != "")
	a := agent{key: key, config: config, client: networkprobe.Client(), authority: networkprobe.ReleaseClient()}
	defer a.client.CloseIdleConnections()
	defer a.authority.CloseIdleConnections()
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	monitorDone := make(chan struct{})
	go func() { defer close(monitorDone); a.monitor(ctx) }()
	defer func() { stop(); <-monitorDone }()
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
	mirrorURL := strings.TrimRight(a.config.LinkHost, "/") + "/.well-known/windshare-network"
	data, err := networkprobe.Fetch(ctx, a.client, mirrorURL, nodedirectory.MaximumEnvelopeBytes)
	if err != nil {
		slog.Warn("mainland mirror fetch failed", "url", mirrorURL, "error", err)
		return nil
	}
	dir := a.config.Directory
	snapshot, err := nodedirectory.Verify(data, dir.Network, dir.Roots)
	if err != nil {
		slog.Warn("mainland mirror verification failed", "url", mirrorURL, "error", err)
		return nil
	}
	cursor := snapshot.Cursor()
	if !snapshot.Usable(time.Now(), 0) {
		slog.Warn("mainland mirror snapshot unusable", "url", mirrorURL, "generation", cursor.Generation, "serial", cursor.Serial)
		return nil
	}
	value := base64.RawURLEncoding.EncodeToString(cursor.Digest[:])
	return &value
}

func (a *agent) currentTime() time.Time {
	if a.now != nil {
		return a.now()
	}
	return time.Now()
}

// Compute a poll timestamp strictly after the last authority-persisted claim so
// small backward NTP steps do not fail replay checks. Only successful claims
// advance lastClaimedAt, matching the authority's vantage-lease persistence;
// latching earlier would wedge polling in the future if a fast RTC is rejected
// before NTP steps the clock back.
func (a *agent) pollTimestamp(now time.Time) int64 {
	ts := now.Unix()
	if a.lastClaimedAt > 0 && ts <= a.lastClaimedAt {
		return a.lastClaimedAt + 1
	}
	return ts
}

func truncateUTF8(s string, maxBytes int) string {
	if len(s) <= maxBytes {
		return s
	}
	cut := 0
	for cut < len(s) {
		_, size := utf8.DecodeRuneInString(s[cut:])
		if cut+size > maxBytes {
			break
		}
		cut += size
	}
	return s[:cut]
}

func (a *agent) cycle(ctx context.Context) error {
	var shares [networkprobe.OriginCount]networkprobe.SyntheticShare
	var ids [networkprobe.OriginCount][link.SenderAuthenticatedShareIDBytes]byte
	for i := range shares {
		share, err := networkprobe.NewShare()
		if err != nil {
			return err
		}
		shares[i] = share
		ids[i] = share.ID()
	}
	var work *networkprobe.Work
	now := a.currentTime()
	pollAt := a.pollTimestamp(now)
	if wallAt := now.Unix(); pollAt > wallAt {
		slog.Warn("vantage poll timestamp clamped after backward clock step", "wall_at", wallAt, "poll_at", pollAt, "last_claimed_at", a.lastClaimedAt)
	}
	if err := a.post(ctx, "poll", pollDomain, map[string]any{"at": pollAt, "shares": ids}, &work); err != nil || work == nil {
		return err
	}
	a.lastClaimedAt = pollAt
	probe, cancel := context.WithTimeout(ctx, checkTimeout)
	failure := networkprobe.Check(probe, a.client, *work, shares)
	cancel()
	outcome := map[string]string{"kind": "passed"}
	switch {
	case ctx.Err() != nil:
		// A stopped vantage releases its lease without attributing a failure to the node.
		outcome["kind"] = "released"
	case failure != nil:
		outcome = map[string]string{"kind": "failed", "reason": truncateUTF8(failure.Error(), maxFailureReasonBytes)}
	}
	slog.Info("enrollment verification", "operation_id", work.OperationID, "node_id", work.Node.NodeKey, "error", failure)
	report, stop := context.WithTimeout(context.WithoutCancel(ctx), reportTimeout)
	defer stop()
	return a.post(report, "result", resultDomain, map[string]any{"at": a.currentTime().Unix(), "lease": work.Lease, "node_key": work.Node.NodeKey, "outcome": outcome}, nil)
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
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, strings.TrimRight(a.config.Authority.URL, "/")+"/v1/vantage/"+path, bytes.NewReader(proof))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	res, err := a.authority.Do(req)
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
	const limit = 512 << 10
	data, err := io.ReadAll(io.LimitReader(res.Body, limit+1))
	if err != nil {
		return err
	}
	if len(data) > limit {
		return errors.New("oversized probe assignment")
	}
	return json.Unmarshal(data, out)
}
