package cli

import (
	"context"
	"crypto/rand"
	"errors"
	"fmt"

	"github.com/windshare/windshare/connectivity"
	"github.com/windshare/windshare/core/link"
	"github.com/windshare/windshare/core/manifest"
	"github.com/windshare/windshare/core/osfs"
	"github.com/windshare/windshare/core/share"
	"github.com/windshare/windshare/relay/protocol"
	"github.com/windshare/windshare/transport/relay"
)

// shareIDAttempts 是 shareId 活跃碰撞时的重生成次数(§6.3:碰撞概率
// astronomically rare,中转拒绝后客户端重生成;重试有限次让"中转异常地
// 一直报碰撞"不至于死循环)。
const shareIDAttempts = 3

// runShare 实现 `windshare share`(§6.9):仅 stat 出链接 → 注册中转 →
// registered ack 后立即打印链接 → 保持在线供块;Ctrl-C 即停止分享。
func (a *App) runShare(ctx context.Context, args []string) int {
	fs := a.newFlagSet("share")
	relayURL := fs.String("relay", DefaultRelayURL, "relay server URL")
	blockSize := fs.Int64("block-size", 0, "block size in bytes (power of two; 0 uses the default 1 MiB)")
	splitKey := fs.Bool("split-key", false, "print a bare link and separate key string")
	frontURL := fs.String("front-url", DefaultFrontURL, "frontend base URL embedded in the link")
	paths, err := parseInterleaved(fs, args)
	if err != nil {
		return ExitUsage
	}
	if len(paths) == 0 {
		a.logf("share: at least one path is required")
		return ExitUsage
	}

	// 仅 stat 快照:出链接耗时与内容体积无关(§6.4)。跳过条目必须可观测,
	// 否则"分享到的比预期少"对用户毫无线索。
	snap, err := osfs.Walk(paths)
	if err != nil {
		a.logf("share: %v", err)
		return ExitUsage
	}
	for _, sk := range snap.Skipped {
		a.logf("warning: skipped %s (%s)", sk.Path, sk.Reason)
	}
	if len(snap.Entries) == 0 {
		a.logf("share: selected paths contain no shareable content")
		return ExitUsage
	}
	metas := make([]share.FileMeta, len(snap.Entries))
	var totalBytes int64
	for i, e := range snap.Entries {
		metas[i] = share.FileMeta(e)
		totalBytes += metas[i].Size
	}

	sharer, conn, code := a.registerShare(ctx, snap, metas, *relayURL, *blockSize)
	if code != ExitOK {
		return code
	}

	lnk := sharer.Link()
	lnk.Relays = []string{*relayURL}
	if code := a.printLink(lnk, *frontURL, *splitKey); code != ExitOK {
		_ = conn.Close()
		return code
	}
	a.logf("share ready: %d entries, %s total, %d blocks; press Ctrl-C to stop", len(metas), formatBytes(totalBytes), sharer.NumChunks())

	return a.serveShare(ctx, sharer, conn)
}

// registerShare 组装 Sharer 并注册中转,shareId 活跃碰撞时重建重试(§6.3;
// 重建即换全新身份,旧身份未曾出链接,丢弃无副作用)。resumeToken 进程内
// crypto/rand 生成:发送端本地秘密,不经链接/清单流出(§6.8)。
// 退出码按阶段归类:本地构建失败 = 用户错误,注册失败 = 网络错误。
func (a *App) registerShare(ctx context.Context, snap *osfs.Snapshot, metas []share.FileMeta, relayURL string, blockSize int64) (*share.Sharer, *relay.SenderConn, int) {
	src := osfs.NewSource(snap)
	for attempt := 1; ; attempt++ {
		sharer, err := share.NewSharer(metas, src, share.Options{ChunkSize: blockSize})
		if err != nil {
			if errors.Is(err, manifest.ErrManifestTooLarge) {
				// 序列化预检在出链接前失败(§6.9):给出可执行的处置建议。
				a.logf("share: manifest exceeds the %s limit (%v)\nSplit the content into multiple shares or reduce the number of entries.", formatBytes(manifest.MaxManifestSize), err)
			} else {
				a.logf("share: %v", err)
			}
			return nil, nil, ExitUsage
		}
		sealed, err := sharer.SealedManifest()
		if err != nil {
			a.logf("share: %v", err)
			return nil, nil, ExitFailure
		}
		token := make([]byte, protocol.ResumeTokenBytes)
		if _, err := rand.Read(token); err != nil {
			a.logf("share: generate resumeToken: %v", err)
			return nil, nil, ExitFailure
		}
		conn, err := relay.DialSender(ctx, relay.SenderConfig{
			RelayURL:       relayURL,
			ShareID:        sharer.Link().ShareID,
			SealedManifest: sealed,
			ResumeToken:    token,
			Logf:           a.logf,
		})
		if err == nil {
			return sharer, conn, ExitOK
		}
		if srvErr, ok := errors.AsType[*relay.ServerError](err); ok &&
			srvErr.Code == protocol.ErrCodeShareIDCollision && attempt < shareIDAttempts {
			a.logf("share: shareId collision; regenerating (attempt %d)", attempt)
			continue
		}
		if errors.Is(err, context.Canceled) {
			a.logf("share: canceled")
			return nil, nil, ExitFailure
		}
		a.logf("share: register with relay: %v", err)
		return nil, nil, ExitNetwork
	}
}

// printLink 输出能力链接(§6.9):registered ack 已到(DialSender 返回即 ack,
// §10),此刻打印即"立即可分发"。链接是机器可读产物,走 stdout。
func (a *App) printLink(lnk link.Link, frontURL string, splitKey bool) int {
	if splitKey {
		bare, key, err := lnk.SplitURL(frontURL)
		if err != nil {
			a.logf("share: construct link: %v", err)
			return ExitUsage
		}
		fmt.Fprintf(a.Stdout, "Bare link: %s\nKey: %s\n", bare, key)
		a.logf("send the bare link and key over separate channels; the receiver runs windshare get <bare-link> --key <key>")
		return ExitOK
	}
	full, err := lnk.URL(frontURL)
	if err != nil {
		a.logf("share: construct link: %v", err)
		return ExitUsage
	}
	fmt.Fprintf(a.Stdout, "Link: %s\n", full)
	return ExitOK
}

// serveShare keeps CLI concerns at the boundary: it wires relay/Pion adapters,
// maps domain outcomes to user messages, and leaves fan-out lifecycle to the
// reusable connectivity coordinator.
func (a *App) serveShare(ctx context.Context, sharer *share.Sharer, conn *relay.SenderConn) int {
	source, err := connectivity.NewRelayReceiverSource(conn)
	if err != nil {
		_ = conn.Close()
		a.logf("share: initialize receiver source: %v", err)
		return ExitFailure
	}
	peerFactory := connectivity.NewPionChannelFactory(connectivity.DefaultPionConfiguration())
	sender, err := connectivity.NewShareSender(source, peerFactory, sharer.BlockStore(), sharer.Sealer(), connectivity.ShareSenderOptions{
		OnReceiverStart: func(receiver connectivity.AcceptedReceiver) {
			a.logf("receiver connected (session %s)", receiver.ID)
		},
		OnReceiverEnd: func(receiver connectivity.AcceptedReceiver, _ error) {
			a.logf("session %s ended", receiver.ID)
		},
	})
	if err != nil {
		_ = source.Close()
		a.logf("share: initialize connectivity: %v", err)
		return ExitFailure
	}
	err = sender.Run(ctx)
	switch {
	case err == nil:
		return ExitOK
	case errors.Is(err, context.Canceled) && ctx.Err() != nil:
		a.logf("interrupt received; stopping share")
		return ExitOK
	case errors.Is(err, connectivity.ErrReceiverSourceEnded):
		a.logf("relay connection ended: %v", err)
		return ExitNetwork
	case errors.Is(err, osfs.ErrDrift):
		a.logf("share aborted: a source file changed; create a new share (%v)", err)
		return ExitDrift
	default:
		a.logf("share aborted: %v", err)
		return ExitFailure
	}
}
