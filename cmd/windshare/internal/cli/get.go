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
// → 断点续传下载 → 收尾物化 → 删除 journal。各阶段助手返回非 ExitOK 即已
// 向用户报告过失败。
func (a *App) runGet(ctx context.Context, args []string) int {
	req, code := a.parseGetRequest(args)
	if code != ExitOK {
		return code
	}

	cfg := relay.ReceiverConfig{RelayURL: req.link.Relays[0], ShareID: req.link.ShareID, Logf: a.logf}
	conn, code := a.dialGetRelay(ctx, cfg)
	if code != ExitOK {
		return code
	}
	// ReceiverRecovery owns every replacement link. This defer covers setup
	// failures before ownership reaches that coordinator and is idempotent after.
	defer func() { conn.Close() }()

	transfer, code := a.prepareGetTransfer(conn, req)
	// The sink outlives even a failed preparation (it exists once created), so
	// its close-with-warning runs on every exit path below, before conn closes.
	defer a.closeSink(transfer.sink)
	if code != ExitOK {
		return code
	}
	return a.receiveAndFinalize(ctx, conn, cfg, req, transfer)
}

// getRequest 是一次 `windshare get` 调用的已验证输入。
type getRequest struct {
	outDir string
	only   []string
	link   link.Link
}

// parseGetRequest 解析旗标与位置参数并落地链接解析(含 --key 合并)。
func (a *App) parseGetRequest(args []string) (getRequest, int) {
	fs := a.newFlagSet("get")
	outDir := fs.String("o", ".", "output directory")
	keyStr := fs.String("key", "", "separate key string when the link has no fragment")
	var only repeatedFlag
	fs.Var(&only, "only", "download only this manifest path; repeatable, and directories select their subtrees")
	pos, err := parseInterleaved(fs, args)
	if err != nil {
		return getRequest{}, ExitUsage
	}
	if len(pos) != 1 {
		a.logf("get: exactly one link argument is required")
		return getRequest{}, ExitUsage
	}
	lnk, err := a.resolveLink(pos[0], *keyStr)
	if err != nil {
		a.logf("get: %v", err)
		return getRequest{}, ExitUsage
	}
	if len(lnk.Relays) == 0 {
		a.logf("get: link has no relay address (?r=); cannot locate the share")
		return getRequest{}, ExitUsage
	}
	return getRequest{outDir: *outDir, only: []string(only), link: lnk}, ExitOK
}

func (a *App) dialGetRelay(ctx context.Context, cfg relay.ReceiverConfig) (*relay.ReceiverConn, int) {
	conn, err := relay.DialReceiver(ctx, cfg)
	if err != nil {
		if ctxErr := ctx.Err(); ctxErr != nil {
			a.logf("get: interrupted")
			return nil, ExitFailure
		}
		a.logf("get: connect to relay: %v", err)
		return nil, ExitNetwork
	}
	return conn, ExitOK
}

// getTransfer 汇集一次下载所需的资源:resume journal(含给用户看的落盘
// 路径)、输出 sink、选择计划,以及守卫 rejoin 身份的清单指纹。
type getTransfer struct {
	fingerprint manifest.Fingerprint
	jpath       string
	journal     *resumeJournal
	sink        *osfs.Sink
	rcv         *share.Receiver
	plan        *share.TransferPlan
	skipped     uint64 // 续传已到位而无需重下的所选块数
}

// prepareGetTransfer 建立 journal → sink → 接收方 → 计划的依赖链。失败时
// 已创建的资源(尤其 sink)仍随返回值带出,由调用方的 defer 统一收尾。
func (a *App) prepareGetTransfer(conn *relay.ReceiverConn, req getRequest) (getTransfer, int) {
	var t getTransfer
	sealed := conn.SealedManifest()
	fingerprint, err := manifest.SealedFingerprint(sealed)
	if err != nil {
		a.logf("get: %v", err)
		return t, ExitUsage
	}
	t.fingerprint = fingerprint
	t.jpath = journalPath(req.outDir, fingerprint)
	t.journal, err = loadResume(t.jpath, fingerprint)
	if err != nil {
		a.logf("get: %v\nAction: remove that file and any incomplete output created by this transaction, then retry.", err)
		return t, ExitUsage
	}

	t.sink, err = osfs.NewSink(req.outDir, osfs.SinkOptions{Ownership: t.journal})
	if err != nil {
		a.logf("get: %v", err)
		return t, ExitFailure
	}
	t.rcv, err = share.NewReceiver(req.link, sealed, t.sink)
	if err != nil {
		// 到这一步链接结构已合法,解封失败几乎只有一个原因:密钥不对。
		a.logf("get: %v\nCheck that the link or key is complete and belongs to this share.", err)
		return t, ExitUsage
	}
	t.plan, err = t.rcv.Plan(req.only)
	if err != nil {
		a.logf("get: %v", err)
		return t, ExitUsage
	}
	if err := t.journal.Bind(t.plan); err != nil {
		a.logf("get: %v\nAction: use the same selection as the incomplete download, or remove %s and the incomplete output created by that transaction before retrying.", err, t.jpath)
		return t, ExitUsage
	}
	t.skipped = t.plan.Sink().Have().Count()
	if t.journal.Resuming() {
		a.logf("resume state found: %d/%d selected blocks already present", t.skipped, t.plan.Chunks().Count())
	}

	// journal 先于数据落盘建档:崩溃在"有数据无档"窗口会让重启撞上
	// 同名文件保护,先建档把该窗口消掉(§6.12)。
	if err := t.journal.Checkpoint(); err != nil {
		a.logf("get: %v", err)
		return t, ExitFailure
	}
	return t, ExitOK
}

// closeSink 收尾输出 sink;此时下载结局已定并报告,关闭失败只值一条警告。
func (a *App) closeSink(sink *osfs.Sink) {
	if sink == nil {
		return
	}
	if err := sink.Close(); err != nil {
		a.logf("warning: %v", err)
	}
}

// receiveAndFinalize 跑恢复监督下的接收会话,并按终局走中断固化或收尾物化。
func (a *App) receiveAndFinalize(ctx context.Context, conn *relay.ReceiverConn, cfg relay.ReceiverConfig, req getRequest, t getTransfer) int {
	completedBytes, err := completedSelectedBytes(t.plan)
	if err != nil {
		a.logf("get: %v", err)
		return ExitFailure
	}
	prog := newProgress(a.stderrWriter(), t.plan.SelectedBytes(), completedBytes)
	jsink := &journalSink{inner: t.plan.Sink(), journal: t.journal, plan: t.plan, prog: prog}
	sess, err := session.NewReceiveSession(t.plan.Chunks(), jsink, t.rcv.Opener(), session.Options{
		MaxBlockBytes: t.rcv.MaxBlockBytes(),
	})
	if err != nil {
		a.logf("get: %v", err)
		return ExitFailure
	}
	runErr := a.runReceiveRecovery(ctx, sess, conn, cfg, t.fingerprint)
	bytesGot, elapsed := prog.done()
	if runErr != nil {
		return a.settleInterruptedGet(runErr, t)
	}
	return a.finalizeGet(req, t, bytesGot, elapsed)
}

// settleInterruptedGet 固化中断现场为可续传状态,再折算退出码。
func (a *App) settleInterruptedGet(runErr error, t getTransfer) int {
	// 最新位图落档:中断处即续传起点。
	if err := t.journal.Checkpoint(); err != nil {
		a.logf("get: save resume state: %v", err)
	}
	if errors.Is(runErr, osfs.ErrAlreadyExists) {
		if err := t.journal.RemoveIfUnowned(); err != nil {
			a.logf("warning: %v", err)
		}
	}
	return a.reportGetErr(runErr)
}

func (a *App) finalizeGet(req getRequest, t getTransfer, bytesGot int64, elapsed time.Duration) int {
	// 完成条件 = 所选块全通过且物化完成,journal 其后才删(§6.12)。
	if err := t.plan.Finalize(); err != nil {
		if errors.Is(err, osfs.ErrAlreadyExists) {
			if cleanupErr := t.journal.RemoveIfUnowned(); cleanupErr != nil {
				a.logf("warning: %v", cleanupErr)
			}
		}
		a.logf("get: finalize selected output: %v", err)
		return a.reportGetErr(err)
	}
	if err := t.journal.Remove(); err != nil {
		a.logf("warning: remove resume state %s: %v (it is safe to delete manually)", t.jpath, err)
	}

	absOut, err := filepath.Abs(req.outDir)
	if err != nil {
		absOut = req.outDir
	}
	a.logf("download complete: %d entries, received %s in %s (%d blocks skipped by resume)", len(t.plan.SelectedEntries()), formatBytes(bytesGot), elapsed.Round(time.Millisecond), t.skipped)
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
