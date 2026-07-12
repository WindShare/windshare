package cli

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"time"

	"github.com/windshare/windshare/connectivity"
	"github.com/windshare/windshare/core/link"
	"github.com/windshare/windshare/core/manifest"
	"github.com/windshare/windshare/core/osfs"
	"github.com/windshare/windshare/core/session"
	"github.com/windshare/windshare/core/share"
	"github.com/windshare/windshare/relay/protocol"
	"github.com/windshare/windshare/transport/relay"
)

// journalFlushInterval 是落块后 journal 的节流刷新间隔(§6.12):每块都刷
// 会把小块下载变成 journal 写放大,崩溃最多重下一个间隔内的块,可接受。
const journalFlushInterval = 500 * time.Millisecond

// rejoinWindow 是传输中断后 rejoin 的退避总窗:须熬过发送端的断线重连宽限
// (SenderReconnectGrace),再留一段拨号/join 的余量(transport/relay 的
// ReceiverConfig 文档要求)。
const rejoinWindow = protocol.SenderReconnectGrace + 15*time.Second

// runGet 实现 `windshare get`(§6.9):解析链接 → 拉清单 → (可选 --only)
// → 断点续传下载 → 收尾物化 → 删除 journal。
func (a *App) runGet(ctx context.Context, args []string) int {
	fs := a.newFlagSet("get")
	outDir := fs.String("o", ".", "output directory")
	keyStr := fs.String("key", "", "separate key string when the link has no fragment")
	var only repeatedFlag
	fs.Var(&only, "only", "download only this manifest path; repeatable, and directories select their subtrees")
	pos, err := parseInterleaved(fs, args)
	if err != nil {
		return ExitUsage
	}
	if len(pos) != 1 {
		a.logf("get: exactly one link argument is required")
		return ExitUsage
	}
	lnk, err := a.resolveLink(pos[0], *keyStr)
	if err != nil {
		a.logf("get: %v", err)
		return ExitUsage
	}
	if len(lnk.Relays) == 0 {
		a.logf("get: link has no relay address (?r=); cannot locate the share")
		return ExitUsage
	}

	cfg := relay.ReceiverConfig{RelayURL: lnk.Relays[0], ShareID: lnk.ShareID, Logf: a.logf}
	conn, err := relay.DialReceiver(ctx, cfg)
	if err != nil {
		if ctxErr := ctx.Err(); ctxErr != nil {
			a.logf("get: interrupted")
			return ExitFailure
		}
		a.logf("get: connect to relay: %v", err)
		return ExitNetwork
	}
	// ReceiverRecovery owns every replacement link. This defer covers setup
	// failures before ownership reaches that coordinator and is idempotent after.
	defer func() { conn.Close() }()

	sealed := conn.SealedManifest()
	fingerprint, err := manifest.SealedFingerprint(sealed)
	if err != nil {
		a.logf("get: %v", err)
		return ExitUsage
	}
	jpath := journalPath(*outDir, fingerprint)
	journal, err := loadResume(jpath, fingerprint)
	if err != nil {
		a.logf("get: %v\nAction: remove that file and any incomplete output created by this transaction, then retry.", err)
		return ExitUsage
	}

	sink, err := osfs.NewSink(*outDir, osfs.SinkOptions{Ownership: journal})
	if err != nil {
		a.logf("get: %v", err)
		return ExitFailure
	}
	defer func() {
		if err := sink.Close(); err != nil {
			a.logf("warning: %v", err)
		}
	}()
	rcv, err := share.NewReceiver(lnk, sealed, sink)
	if err != nil {
		// 到这一步链接结构已合法,解封失败几乎只有一个原因:密钥不对。
		a.logf("get: %v\nCheck that the link or key is complete and belongs to this share.", err)
		return ExitUsage
	}
	plan, err := rcv.Plan([]string(only))
	if err != nil {
		a.logf("get: %v", err)
		return ExitUsage
	}
	if err := journal.Bind(plan); err != nil {
		a.logf("get: %v\nAction: use the same selection as the incomplete download, or remove %s and the incomplete output created by that transaction before retrying.", err, jpath)
		return ExitUsage
	}
	have := plan.Sink().Have()
	skipped := have.Count()
	if journal.Resuming() {
		a.logf("resume state found: %d/%d selected blocks already present", skipped, plan.Chunks().Count())
	}

	// journal 先于数据落盘建档:崩溃在"有数据无档"窗口会让重启撞上
	// 同名文件保护,先建档把该窗口消掉(§6.12)。
	if err := journal.Checkpoint(); err != nil {
		a.logf("get: %v", err)
		return ExitFailure
	}

	completedBytes, err := completedSelectedBytes(plan)
	if err != nil {
		a.logf("get: %v", err)
		return ExitFailure
	}
	prog := newProgress(a.stderrWriter(), plan.SelectedBytes(), completedBytes)
	jsink := &journalSink{inner: plan.Sink(), journal: journal, plan: plan, prog: prog}
	sess, err := session.NewReceiveSession(plan.Chunks(), jsink, rcv.Opener(), session.Options{
		MaxBlockBytes: rcv.MaxBlockBytes(),
	})
	if err != nil {
		a.logf("get: %v", err)
		return ExitFailure
	}
	runErr := a.runReceiveRecovery(ctx, sess, conn, cfg, fingerprint)
	bytesGot, elapsed := prog.done()
	if runErr != nil {
		// 最新位图落档:中断处即续传起点。
		if err := journal.Checkpoint(); err != nil {
			a.logf("get: save resume state: %v", err)
		}
		if errors.Is(runErr, osfs.ErrAlreadyExists) {
			if err := journal.RemoveIfUnowned(); err != nil {
				a.logf("warning: %v", err)
			}
		}
		return a.reportGetErr(runErr)
	}

	// 完成条件 = 所选块全通过且物化完成,journal 其后才删(§6.12)。
	if err := plan.Finalize(); err != nil {
		if errors.Is(err, osfs.ErrAlreadyExists) {
			if cleanupErr := journal.RemoveIfUnowned(); cleanupErr != nil {
				a.logf("warning: %v", cleanupErr)
			}
		}
		a.logf("get: finalize selected output: %v", err)
		return a.reportGetErr(err)
	}
	if err := journal.Remove(); err != nil {
		a.logf("warning: remove resume state %s: %v (it is safe to delete manually)", jpath, err)
	}

	absOut, err := filepath.Abs(*outDir)
	if err != nil {
		absOut = *outDir
	}
	a.logf("download complete: %d entries, received %s in %s (%d blocks skipped by resume)", len(plan.SelectedEntries()), formatBytes(bytesGot), elapsed.Round(time.Millisecond), skipped)
	a.logf("output directory: %s", absOut)
	return ExitOK
}

// resolveLink 落地 §6.9 的密钥合并矩阵:--key 提供即走 Merge(完整链接也可,
// Merge 自带一致性核对);仅链接则 Parse;无 fragment 且无 --key 转交互输入。
func (a *App) resolveLink(raw, key string) (link.Link, error) {
	if key != "" {
		return link.Merge(raw, key)
	}
	l, err := link.Parse(raw)
	if !errors.Is(err, link.ErrMissingFragment) {
		return l, err
	}
	fmt.Fprint(a.stderrWriter(), "Link has no key; enter the key string: ")
	line, rerr := bufio.NewReader(a.Stdin).ReadString('\n')
	line = strings.TrimSpace(line)
	if line == "" {
		if rerr != nil {
			return link.Link{}, fmt.Errorf("cli: read key string: %w", rerr)
		}
		return link.Link{}, errors.New("cli: no key string was provided")
	}
	return link.Merge(raw, line)
}

func completedSelectedBytes(plan *share.TransferPlan) (int64, error) {
	var completed int64
	for index := range plan.Sink().Have().SetBits() {
		bytesInChunk, err := plan.SelectedBytesInChunk(index)
		if err != nil {
			return 0, fmt.Errorf("cli: calculate resumed progress for chunk %d: %w", index, err)
		}
		completed += bytesInChunk
	}
	return completed, nil
}

// journalSink 包装接收落盘面:落块后节流刷新 journal(§6.12)并推进度。
// WriteBlock 仅由接收会话的单线程事件循环调用,状态无需加锁。
type journalSink struct {
	inner   session.Sink
	journal *resumeJournal
	plan    *share.TransferPlan
	prog    *progress

	lastFlush time.Time
}

func (s *journalSink) WriteBlock(index uint64, plaintext []byte) error {
	if err := s.inner.WriteBlock(index, plaintext); err != nil {
		return err
	}
	selectedBytes, err := s.plan.SelectedBytesInChunk(index)
	if err != nil {
		return err
	}
	if time.Since(s.lastFlush) >= journalFlushInterval {
		if err := s.journal.Checkpoint(); err != nil {
			// journal 写不动的盘,数据面多半也写不动;诚实失败胜过静默丢
			// 续传保障。
			return err
		}
		s.lastFlush = time.Now()
	}
	// Progress is also the process-level resume test's observation boundary.
	// Publishing it after the checkpoint ensures an observer never mistakes a
	// materialized-but-not-yet-resumable first block for durable progress.
	s.prog.step(selectedBytes)
	return nil
}

func (s *journalSink) Have() session.Bitfield { return s.inner.Have() }

func (s *journalSink) DeliveryOrder() session.DeliveryOrder { return s.inner.DeliveryOrder() }

// runReceiveRecovery only wires CLI adapters and messages. Relay recovery,
// identity gating, terminal arbitration, and cleanup live in connectivity so
// E2E and the future engine exercise the same domain lifecycle.
func (a *App) runReceiveRecovery(ctx context.Context, sess *session.ReceiveSession, conn *relay.ReceiverConn, cfg relay.ReceiverConfig, fingerprint manifest.Fingerprint) error {
	peerFactory := connectivity.NewPionChannelFactory(connectivity.DefaultPionConfiguration())
	channels, err := connectivity.NewReceiverPool(ctx, sess, peerFactory, connectivity.ReceiverPoolOptions{
		OnPeerError: func(err error) {
			a.logf("P2P path unavailable; continuing with relay (%v)", err)
		},
	})
	if err != nil {
		return err
	}
	initial, err := connectivity.NewRelayReceiverLink(conn)
	if err != nil {
		_ = channels.Close()
		return err
	}
	cfg.JoinRetryWindow = rejoinWindow
	if a.RejoinWindow > 0 {
		cfg.JoinRetryWindow = a.RejoinWindow
	}
	validator := connectivity.ManifestIdentityValidatorFunc(func(sealed []byte) error {
		rejoinedFingerprint, err := manifest.SealedFingerprint(sealed)
		if err != nil {
			return err
		}
		if rejoinedFingerprint != fingerprint {
			return fmt.Errorf("%w: share was recreated with a different manifest; run get again", errJournalFingerprint)
		}
		return nil
	})
	recovery, err := connectivity.NewReceiverRecovery(
		sess,
		channels,
		connectivity.NewRelayReceiverDialer(cfg),
		validator,
		connectivity.ReceiverRecoveryOptions{
			RetryPolicy: connectivity.ImmediateRecoveryRetry(),
			OnEvent: func(event connectivity.RecoveryEvent) {
				switch event.Kind {
				case connectivity.RecoveryRelayInterrupted:
					if event.Err != nil {
						a.logf("transfer interrupted (%v); reconnecting...", event.Err)
					} else {
						a.logf("sender ended the session; attempting to reconnect...")
					}
				case connectivity.RecoveryContinuingOnPeer:
					a.logf("relay unavailable; continuing over the established P2P path (%v)", event.Err)
				case connectivity.RecoveryPeerEnded:
					a.logf("P2P path ended; retrying relay recovery")
				case connectivity.RecoveryRelayRestored:
					a.logf("reconnected; continuing download")
				}
			},
		},
	)
	if err != nil {
		_ = channels.Close()
		_ = initial.Close()
		return err
	}
	return recovery.Run(ctx, initial)
}

// reportGetErr 把接收终局错误折算成退出码与给人的处置提示。
func (a *App) reportGetErr(err error) int {
	if peerErr, ok := errors.AsType[*session.Error](err); ok {
		switch peerErr.Code {
		case session.ErrCodeBlockRead:
			// The M1 sender uses block-read terminal failures for snapshot drift.
			a.logf("get: sender aborted the share because a source file may have changed; ask the sender to share again (%v)", err)
			return ExitDrift
		case session.ErrCodeSeal:
			a.logf("get: sender aborted the share after an encryption failure; ask the sender to share again (%v)", err)
			return ExitFailure
		}
	}
	switch {
	case errors.Is(err, context.Canceled):
		a.logf("get: interrupted; resume state was saved, so rerun the same command to continue")
		return ExitFailure
	case errors.Is(err, connectivity.ErrRelayRecoveryFailed), errors.Is(err, session.ErrBlockExhausted):
		a.logf("get: %v\nThe share may be offline. Resume state was saved; retry later.", err)
		return ExitNetwork
	case errors.Is(err, errJournalFingerprint):
		a.logf("get: %v", err)
		return ExitUsage
	case errors.Is(err, osfs.ErrAlreadyExists):
		a.logf("get: %v\nThe output directory already contains that path without matching resume ownership. WindShare will not overwrite it; choose another output directory or move the existing path.", err)
		return ExitUsage
	default:
		a.logf("get: %v", err)
		return ExitFailure
	}
}
