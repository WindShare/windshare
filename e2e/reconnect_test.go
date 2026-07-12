// 发送端断线重连:黑盒难以只切断 share 进程那一条 WS(其 socket 不可从外部
// 触及),故按 §T6.1 许可降级为进程内测试——真实中转 + 真实 core 管线
// (Sharer/Receiver + osfs + AEAD),经可切断的拨号器切断发送端链路,验证宽限
// 重注册 + 接收端 rejoin 后下载完成且哈希一致(§6.8/§6.12)。
package e2e

import (
	"context"
	"crypto/rand"
	"errors"
	"net/http"
	"path/filepath"
	"testing"
	"time"

	"github.com/windshare/windshare/connectivity"
	"github.com/windshare/windshare/core/manifest"
	"github.com/windshare/windshare/core/osfs"
	"github.com/windshare/windshare/core/session"
	"github.com/windshare/windshare/core/share"
	"github.com/windshare/windshare/internal/testnetwork"
	"github.com/windshare/windshare/relay/protocol"
	"github.com/windshare/windshare/transport/relay"
)

// serveSender deliberately performs only adapter construction; the production
// share-level coordinator is the E2E subject rather than a copied fan-out loop.
func serveSender(ctx context.Context, sc *relay.SenderConn, sharer *share.Sharer) error {
	source, err := connectivity.NewRelayReceiverSource(sc)
	if err != nil {
		return err
	}
	sender, err := connectivity.NewShareSender(
		source,
		connectivity.NewPionChannelFactory(connectivity.DefaultPionConfiguration()),
		sharer.BlockStore(),
		sharer.Sealer(),
		connectivity.ShareSenderOptions{},
	)
	if err != nil {
		return err
	}
	return sender.Run(ctx)
}

// downloadWithRejoin wires the production recovery coordinator with real relay,
// Pion, scheduler, and manifest identity adapters.
func downloadWithRejoin(t *testing.T, ctx context.Context, sess *session.ReceiveSession, conn *relay.ReceiverConn, cfg relay.ReceiverConfig, fp manifest.Fingerprint) error {
	t.Helper()
	initial, err := connectivity.NewRelayReceiverLink(conn)
	if err != nil {
		return err
	}
	pool, err := connectivity.NewReceiverPool(ctx, sess, connectivity.NewPionChannelFactory(connectivity.DefaultPionConfiguration()), connectivity.ReceiverPoolOptions{})
	if err != nil {
		return err
	}
	recovery, err := connectivity.NewReceiverRecovery(
		sess,
		pool,
		connectivity.NewRelayReceiverDialer(cfg),
		connectivity.ManifestIdentityValidatorFunc(func(sealed []byte) error {
			got, err := manifest.SealedFingerprint(sealed)
			if err != nil {
				return err
			}
			if got != fp {
				return connectivity.ErrManifestIdentity
			}
			return nil
		}),
		connectivity.ReceiverRecoveryOptions{OnEvent: func(event connectivity.RecoveryEvent) {
			if event.Kind == connectivity.RecoveryRelayRestored {
				t.Logf("receiver rejoined (sessionId=%s)", event.ReceiverID)
			}
		}},
	)
	if err != nil {
		_ = pool.Close()
		return err
	}
	return recovery.Run(ctx, initial)
}

func TestSenderReconnectInProcess(t *testing.T) {
	testnetwork.RequireOSNetwork(t)
	t.Parallel()
	relayURL := startInProcRelay(t)
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()

	// 约 2 MiB 单文件:块数够多,足以在下载中途稳定切断发送端。
	payload := make([]byte, 2<<20)
	for i := range payload {
		payload[i] = byte(i*29 + 5)
	}
	srcRoot := writeTree(t, treeSpec{files: map[string][]byte{"stream.bin": payload}})
	snap, err := osfs.Walk([]string{srcRoot})
	if err != nil {
		t.Fatal(err)
	}
	metas := make([]share.FileMeta, len(snap.Entries))
	for i, e := range snap.Entries {
		metas[i] = share.FileMeta(e)
	}
	sharer, err := share.NewSharer(metas, osfs.NewSource(snap), share.Options{ChunkSize: e2eBlockSizeInt})
	if err != nil {
		t.Fatal(err)
	}
	sealed, err := sharer.SealedManifest()
	if err != nil {
		t.Fatal(err)
	}
	lnk := sharer.Link()
	fp, err := manifest.SealedFingerprint(sealed)
	if err != nil {
		t.Fatal(err)
	}

	// 发送端:可切断的拨号器 + 宽限重连。
	tracker := &dialTracker{}
	token := make([]byte, protocol.ResumeTokenBytes)
	if _, err := rand.Read(token); err != nil {
		t.Fatal(err)
	}
	sconn, err := relay.DialSender(ctx, relay.SenderConfig{
		RelayURL:          relayURL,
		ShareID:           lnk.ShareID,
		SealedManifest:    sealed,
		ResumeToken:       token,
		HTTPClient:        &http.Client{Transport: &http.Transport{DialContext: tracker.DialContext}},
		KeepaliveInterval: 200 * time.Millisecond,
		ReconnectGrace:    30 * time.Second,
		Backoff:           relay.Backoff{Initial: 20 * time.Millisecond, Max: 200 * time.Millisecond},
	})
	if err != nil {
		t.Fatalf("DialSender: %v", err)
	}
	t.Cleanup(func() { _ = sconn.Close() })
	senderDone := make(chan error, 1)
	go func() { senderDone <- serveSender(ctx, sconn, sharer) }()

	// 接收端:真实落盘 + 全量块需求集。
	out := t.TempDir()
	outTree := filepath.Join(out, "tree")
	sink, err := osfs.NewSink(out, osfs.SinkOptions{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = sink.Close() })
	rcv, err := share.NewReceiver(lnk, sealed, sink)
	if err != nil {
		t.Fatal(err)
	}
	plan, err := rcv.Plan(nil)
	if err != nil {
		t.Fatal(err)
	}
	sess, err := session.NewReceiveSession(plan.Chunks(), plan.Sink(), rcv.Opener(), session.Options{MaxBlockBytes: rcv.MaxBlockBytes()})
	if err != nil {
		t.Fatal(err)
	}
	cfg := relay.ReceiverConfig{
		RelayURL:          relayURL,
		ShareID:           lnk.ShareID,
		KeepaliveInterval: 200 * time.Millisecond,
		JoinRetryWindow:   30 * time.Second,
		Backoff:           relay.Backoff{Initial: 20 * time.Millisecond, Max: 200 * time.Millisecond},
	}
	conn, err := relay.DialReceiver(ctx, cfg)
	if err != nil {
		t.Fatalf("DialReceiver: %v", err)
	}
	// 下载中途切断发送端链路(≥1 块落盘后),触发宽限重连 + 接收端 rejoin。
	go func() {
		deadline := time.Now().Add(10 * time.Second)
		for time.Now().Before(deadline) {
			if treeBytes(outTree) > 0 {
				break
			}
			time.Sleep(10 * time.Millisecond)
		}
		tracker.severAll()
	}()

	if err := downloadWithRejoin(t, ctx, sess, conn, cfg, fp); err != nil {
		t.Fatalf("download should complete after reconnect: %v", err)
	}

	if err := plan.Finalize(); err != nil {
		t.Fatalf("finalize materialization: %v", err)
	}
	assertTreeEqual(t, srcRoot, outTree)

	// 发送端在重连后应仍存活(未因单次断链终结)。
	if sconn.Err() != nil {
		t.Errorf("sender should not terminate after link loss: %v", sconn.Err())
	}
	cancel()
	select {
	case err := <-senderDone:
		if !errors.Is(err, context.Canceled) {
			t.Errorf("share sender shutdown error = %v", err)
		}
	case <-time.After(procIOTimeout):
		t.Fatal("share sender did not settle after test cancellation")
	}
}
