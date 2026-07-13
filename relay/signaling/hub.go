package signaling

import (
	"bytes"
	"crypto/rand"
	"io"
	"sync"
	"sync/atomic"
	"time"

	"github.com/windshare/windshare/core/manifest"
	"github.com/windshare/windshare/core/session"
	"github.com/windshare/windshare/relay/admission"
	"github.com/windshare/windshare/relay/forward"
	"github.com/windshare/windshare/relay/protocol"
)

// DefaultMaxManifestSize 直接复用 core/manifest.MaxManifestSize(§8,16 MiB):
// 语义家唯一,中转限额与发送端 Seal 预检天然一致。
const DefaultMaxManifestSize = manifest.MaxManifestSize

const (
	DefaultRoleTimeout      = 15 * time.Second
	DefaultKeepaliveTimeout = 3 * protocol.KeepaliveInterval
)

// Admission is defined by its consumer so signaling depends on policy
// capabilities rather than a concrete controller. Retained register leases let
// reconnect attempts consume rate capacity without double-charging a share.
type Admission interface {
	AdmitConnection(source string) (*admission.Lease, admission.Decision)
	BeginRegister(source string) (*admission.Registration, admission.Decision)
	BeginJoin(source string) (*admission.JoinAttempt, admission.Decision)
	Limits() admission.Limits
}

// ConnectionLease reserves admission before the HTTP handler upgrades. This
// makes the global cap cover handshake work and lets saturation fail as HTTP
// 503 instead of creating a WebSocket whose close handshake can block for 10s.
type ConnectionLease struct {
	hub         *Hub
	lease       *admission.Lease
	once        sync.Once
	transferred atomic.Bool
}

func (l *ConnectionLease) Release() {
	if l == nil {
		return
	}
	l.once.Do(func() { l.lease.Release() })
}

// TransferToHandler marks the listener-to-hijacked-handler ownership handoff.
// ConnState calls this at StateHijacked, including the case where the upgrade
// later reports an error after hijacking the socket.
func (l *ConnectionLease) TransferToHandler() {
	if l != nil {
		l.transferred.Store(true)
	}
}

func (l *ConnectionLease) ReleaseIfTransferred() {
	if l != nil && l.transferred.Load() {
		l.Release()
	}
}

// Config 配置 hub;零值字段取默认。时长与容量全部可注入,测试用短宽限期
// 与小限额验证语义,不碰真实时钟常量。
type Config struct {
	// MaxManifestSize:单清单上限;超限拒 register(§6.8)。
	MaxManifestSize int64
	// MaxFrameSize:内层数据面帧上限;二进制转发帧限额 = 此值 + 包裹头。
	MaxFrameSize int64
	// SenderReconnectGrace:发送端断线宽限期(§6.8)。
	SenderReconnectGrace time.Duration
	// RoleTimeout bounds the first role message and register manifest handshake.
	RoleTimeout time.Duration
	// KeepaliveTimeout bounds inactivity after the role is established. Any
	// valid inbound frame proves activity; clients send keepalives while idle.
	KeepaliveTimeout time.Duration
	// Admission is nil only in focused protocol tests. Production must inject a
	// controller so every upgraded connection and registered share is leased.
	Admission Admission
	// Rand 供 sessionId 生成;nil 取 crypto/rand。注入点只为测试确定性。
	Rand io.Reader
	// Logf 为 nil 时丢弃日志。
	Logf func(format string, args ...any)
	// Pump 覆盖泵容量(测试用小队列制造背压溢出)。
	Pump forward.Options
}

func (c Config) withDefaults() Config {
	if c.MaxManifestSize <= 0 {
		c.MaxManifestSize = DefaultMaxManifestSize
	}
	if c.MaxFrameSize <= 0 {
		c.MaxFrameSize = session.MaxFrameSize
	}
	if c.SenderReconnectGrace <= 0 {
		c.SenderReconnectGrace = protocol.SenderReconnectGrace
	}
	if c.RoleTimeout <= 0 {
		c.RoleTimeout = DefaultRoleTimeout
	}
	if c.KeepaliveTimeout <= 0 {
		c.KeepaliveTimeout = DefaultKeepaliveTimeout
	}
	if c.Rand == nil {
		c.Rand = rand.Reader
	}
	if c.Logf == nil {
		c.Logf = func(string, ...any) {}
	}
	return c
}

// share 是一个 shareId 的注册表项:发送端连接、原样存储的清单字节与
// 接收会话集合(§6.8)。sender == nil 即断线宽限中的挂起态。
type share struct {
	id              string
	resumeTokenHash string
	manifestFrame   []byte
	sender          *conn
	sessions        map[protocol.SessionID]*receiverSession
	admissionLease  *admission.Lease
	// graceTimer 在挂起时计时;重注册成功即停。到期回调自行复核状态,
	// 天然免疫"Stop 与触发赛跑"——输了也只是空跑一趟。
	graceTimer *time.Timer
}

type receiverSession struct {
	id   protocol.SessionID
	recv *conn
}

// Hub 是按 shareId 的 WS hub(§6.8)。单把互斥锁守全部注册表状态:热路径
// (转发)在锁内只做 map 查找与非阻塞入队,任何 WS IO 都不持锁进行。
type Hub struct {
	cfg Config

	mu     sync.Mutex
	shares map[string]*share
	conns  map[*conn]struct{}
	closed bool
}

func NewHub(cfg Config) *Hub {
	cfg = cfg.withDefaults()
	return &Hub{
		cfg:    cfg,
		shares: make(map[string]*share),
		conns:  make(map[*conn]struct{}),
	}
}

// ServerLimits derives public policy from the exact live Hub/controller config.
// The HTTP server overlays its own header/read/idle limits before publication.
func (h *Hub) ServerLimits() protocol.ServerLimits {
	limits := protocol.ServerLimits{
		MaxFrameSize:             h.cfg.MaxFrameSize,
		MaxManifestSize:          h.cfg.MaxManifestSize,
		MaxSignalingMessageBytes: protocol.MaxSignalingMessageBytes,
		Timeouts: protocol.ServerTimeouts{
			WebSocketRoleMilliseconds:        h.cfg.RoleTimeout.Milliseconds(),
			WebSocketKeepaliveMilliseconds:   h.cfg.KeepaliveTimeout.Milliseconds(),
			SenderReconnectGraceMilliseconds: h.cfg.SenderReconnectGrace.Milliseconds(),
		},
	}
	if h.cfg.Admission == nil {
		return limits
	}
	policy := h.cfg.Admission.Limits()
	limits.Admission = protocol.AdmissionLimits{
		MaxConnections:            policy.MaxConnections,
		MaxConcurrentShares:       policy.MaxConcurrentShares,
		MaxSharesPerSource:        policy.MaxSharesPerSource,
		MaxManifestBytesPerSource: policy.MaxManifestBytesPerSource,
		MaxTotalManifestBytes:     policy.MaxTotalManifestBytes,
		RegisterPerSource:         protocol.RateLimit{PerSecond: policy.RegisterPerSource.PerSecond, Burst: policy.RegisterPerSource.Burst},
		JoinPerSource:             protocol.RateLimit{PerSecond: policy.JoinPerSource.PerSecond, Burst: policy.JoinPerSource.Burst},
		JoinPerShare:              protocol.RateLimit{PerSecond: policy.JoinPerShare.PerSecond, Burst: policy.JoinPerShare.Burst},
	}
	return limits
}

// AdmitConnection reserves the complete HTTP-upgrade/connection lifecycle.
func (h *Hub) AdmitConnection(source string) (*ConnectionLease, admission.Decision) {
	h.mu.Lock()
	closed := h.closed
	h.mu.Unlock()
	if closed {
		return nil, admission.ConnectionCapacityExceeded
	}
	var lease *admission.Lease
	if h.cfg.Admission != nil {
		var decision admission.Decision
		lease, decision = h.cfg.Admission.AdmitConnection(source)
		if !decision.Allowed() {
			return nil, decision
		}
	}
	return &ConnectionLease{hub: h, lease: lease}, admission.Allowed
}

// Close 停摆 hub:拒绝新连接并强制断开存量连接(不等积压送达——进程要
// 退了,对端靠断连信号 rejoin/重注册)。
func (h *Hub) Close() {
	h.mu.Lock()
	if h.closed {
		h.mu.Unlock()
		return
	}
	h.closed = true
	conns := make([]*conn, 0, len(h.conns))
	leases := make([]*admission.Lease, 0, len(h.shares))
	for c := range h.conns {
		conns = append(conns, c)
	}
	for _, sh := range h.shares {
		if sh.graceTimer != nil {
			sh.graceTimer.Stop()
		}
		if sh.admissionLease != nil {
			leases = append(leases, sh.admissionLease)
		}
	}
	h.shares = map[string]*share{}
	h.conns = map[*conn]struct{}{}
	h.mu.Unlock()
	for _, lease := range leases {
		lease.Release()
	}
	for _, c := range conns {
		c.closeNow()
	}
}

// registerOutcome enumerates the externally meaningful registration results.
type registerOutcome int

const (
	registerOK registerOutcome = iota
	registerCollision
	registerResumeRejected
	registerBudgetExceeded
	registerRateLimited
)

type registerAttempt struct {
	reservation *admission.Registration
	manifestPin *admission.LeasePin
	existing    *share
}

func (a *registerAttempt) release() {
	if a != nil && a.reservation != nil {
		a.reservation.Release()
	}
	if a != nil && a.manifestPin != nil {
		a.manifestPin.Release()
	}
}

// beginRegister charges rate and concurrency before accepting manifest bytes.
// Resume authentication is also checked up front, so an unauthenticated peer
// cannot force a large upload merely to learn that its token is invalid.
func (h *Hub) beginRegister(c *conn, reg *protocol.Register) (*registerAttempt, registerOutcome) {
	attempt := &registerAttempt{}
	if h.cfg.Admission != nil {
		var decision admission.Decision
		attempt.reservation, decision = h.cfg.Admission.BeginRegister(c.ip)
		if decision == admission.RegisterRateExceeded {
			return nil, registerRateLimited
		}
		if !decision.Allowed() || attempt.reservation == nil {
			return nil, registerBudgetExceeded
		}
	}

	h.mu.Lock()
	defer h.mu.Unlock()
	if h.closed {
		attempt.release()
		return nil, registerCollision
	}
	if existing, ok := h.shares[reg.ShareID]; ok {
		return h.beginResumeLocked(attempt, existing, reg)
	}
	if attempt.reservation != nil {
		decision := attempt.reservation.ReserveShare()
		if !decision.Allowed() {
			attempt.release()
			return nil, registerBudgetExceeded
		}
	}
	return attempt, registerOK
}

// beginResumeLocked validates a grace-period re-registration against the
// retained share. Caller holds Hub.mu, so the verdict and the manifest pin are
// atomic with respect to senderGone/reapShare.
func (h *Hub) beginResumeLocked(attempt *registerAttempt, existing *share, reg *protocol.Register) (*registerAttempt, registerOutcome) {
	if existing.sender != nil {
		attempt.release()
		return nil, registerCollision
	}
	// The token is the authentication boundary; manifest equality remains
	// defense in depth and is checked while streaming the next frame.
	if reg.ResumeToken == "" ||
		reg.ResumeTokenHash != existing.resumeTokenHash ||
		!protocol.VerifyResumeToken(reg.ResumeToken, existing.resumeTokenHash) {
		attempt.release()
		return nil, registerResumeRejected
	}
	if h.cfg.Admission != nil {
		var ok bool
		attempt.manifestPin, ok = existing.admissionLease.Pin()
		if !ok {
			attempt.release()
			return nil, registerBudgetExceeded
		}
	}
	attempt.existing = existing
	return attempt, registerOK
}

// finishRegister atomically publishes a fully received manifest or resumes the
// exact retained share. Provisional admission remains owned by attempt until
// this point, so every race and rejection can roll back without a leak.
func (h *Hub) finishRegister(c *conn, reg *protocol.Register, manifestFrame []byte, attempt *registerAttempt) (*share, registerOutcome) {
	if attempt == nil {
		return nil, registerBudgetExceeded
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.closed {
		return nil, registerCollision
	}

	if attempt.existing != nil {
		return h.finishResumeLocked(c, reg, manifestFrame, attempt)
	}

	if _, exists := h.shares[reg.ShareID]; exists {
		return nil, registerCollision
	}
	var lease *admission.Lease
	if attempt.reservation != nil {
		var decision admission.Decision
		lease, decision = attempt.reservation.Commit(nil)
		if !decision.Allowed() || lease == nil {
			return nil, registerBudgetExceeded
		}
	}
	sh := &share{
		id:              reg.ShareID,
		resumeTokenHash: reg.ResumeTokenHash,
		manifestFrame:   manifestFrame,
		sender:          c,
		sessions:        make(map[protocol.SessionID]*receiverSession),
		admissionLease:  lease,
	}
	h.shares[reg.ShareID] = sh
	return sh, registerOK
}

// finishResumeLocked republishes the exact retained share once the streamed
// manifest proved byte-identical. Caller holds Hub.mu: revalidation, admission
// commit and sender takeover form one atomic step against concurrent
// disconnect handling (see TestResumeRegisterOutcomePerDisconnectOrdering).
func (h *Hub) finishResumeLocked(c *conn, reg *protocol.Register, manifestFrame []byte, attempt *registerAttempt) (*share, registerOutcome) {
	existing := attempt.existing
	if h.shares[reg.ShareID] != existing || existing.sender != nil ||
		!bytes.Equal(manifestPayload(manifestFrame), manifestPayload(existing.manifestFrame)) {
		return nil, registerResumeRejected
	}
	if attempt.reservation != nil {
		lease, decision := attempt.reservation.Commit(existing.admissionLease)
		if !decision.Allowed() || lease != existing.admissionLease {
			if lease != nil && lease != existing.admissionLease {
				lease.Release()
			}
			return nil, registerBudgetExceeded
		}
	}
	if existing.graceTimer != nil {
		existing.graceTimer.Stop()
		existing.graceTimer = nil
	}
	existing.sender = c
	return existing, registerOK
}

func manifestPayload(frame []byte) []byte {
	if len(frame) < protocol.ManifestOverheadBytes {
		return nil
	}
	return frame[protocol.ManifestOverheadBytes:]
}

// senderGone 处理发送端断开:进入宽限挂起,终结全部接收会话(§6.8)。
// 接收端收到 sender_gone 后以指数退避 rejoin,凭 bitfield 续传(§6.12)。
func (h *Hub) senderGone(sh *share, c *conn) {
	h.mu.Lock()
	if h.closed || h.shares[sh.id] != sh || sh.sender != c {
		// 已被 Close 回收,或宽限期后新 register 顶替——都不归本连接管。
		h.mu.Unlock()
		return
	}
	sh.sender = nil
	victims := make([]*receiverSession, 0, len(sh.sessions))
	for _, sess := range sh.sessions {
		victims = append(victims, sess)
	}
	sh.sessions = make(map[protocol.SessionID]*receiverSession)
	sh.graceTimer = time.AfterFunc(h.cfg.SenderReconnectGrace, func() { h.reapShare(sh) })
	h.mu.Unlock()

	for _, sess := range victims {
		result, _ := sess.recv.sendSessionTerminal(sess.id, protocol.NewSessionError(sess.id.String(), protocol.ErrCodeSenderGone, "sender disconnected; retry join with backoff"))
		if result != forward.Enqueued {
			sess.recv.pump.CloseSession(sess.id)
		}
		sess.recv.closeAfterFlush()
	}
}

// reapShare 宽限期满回收挂起的 share,释放清单内存预算。
func (h *Hub) reapShare(sh *share) {
	h.mu.Lock()
	if h.shares[sh.id] != sh || sh.sender != nil {
		h.mu.Unlock()
		return // 已重注册或已被回收
	}
	delete(h.shares, sh.id)
	lease := sh.admissionLease
	h.mu.Unlock()
	lease.Release()
}

// openSession 为一次成功 join 分配会话:sessionId 由中转分配、随会话失效
// (§6.7)。返回 nil 表示 share 已非活跃(与 senderGone 竞态)。
func (h *Hub) openSession(sh *share, recv *conn) *receiverSession {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.closed || h.shares[sh.id] != sh || sh.sender == nil {
		return nil
	}
	sender := sh.sender
	for {
		id, ok := sender.sessionIDs.next(h.cfg.Rand)
		if !ok {
			return nil
		}
		// A detached diagnostic owns this exact wire ID. Retiring and skipping
		// it under the same lock that made the reservation prevents a future
		// active session from inheriting the diagnostic's terminal pump queue.
		if sender.unknownSessions.contains(id) {
			continue
		}
		if result := sender.pump.OpenSession(id); result != forward.Enqueued {
			return nil
		}
		if result := recv.pump.OpenSession(id); result != forward.Enqueued {
			sender.pump.CloseSession(id)
			return nil
		}

		sess := &receiverSession{id: id, recv: recv}
		// Publication is the commit point: both required lanes exist first, and
		// Hub.mu prevents routing from observing the retired candidate earlier.
		sh.sessions[id] = sess
		return sess
	}
}

// lookupSession 在锁内解析会话双方;ok=false 即 sessionId 已失效或伪造。
func (h *Hub) lookupSession(sh *share, id protocol.SessionID) (sess *receiverSession, sender *conn, ok bool) {
	h.mu.Lock()
	defer h.mu.Unlock()
	sess, found := sh.sessions[id]
	if !found || sh.sender == nil {
		return nil, nil, false
	}
	return sess, sh.sender, true
}

// lookupSenderSession preserves the distinction between any ID issued during
// this sender connection and an attacker-chosen ID. Outbound terminal delivery
// is not an inbound-drain barrier, so recognition spans the full connection.
func (h *Hub) lookupSenderSession(c *conn, sh *share, id protocol.SessionID) (*receiverSession, senderSessionState, unknownSessionObservation) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.shares[sh.id] != sh || sh.sender != c {
		return nil, senderSessionNeverKnown, c.unknownSessions.observe(id, maxDetachedSessionErrors)
	}
	if sess, ok := sh.sessions[id]; ok {
		return sess, senderSessionActive, unknownSessionRepeated
	}
	if c.sessionIDs.recognizes(id) {
		return nil, senderSessionTerminal, unknownSessionRepeated
	}
	return nil, senderSessionNeverKnown, c.unknownSessions.observe(id, maxDetachedSessionErrors)
}

func (h *Hub) takeSessionLocked(sh *share, id protocol.SessionID) (sess *receiverSession, sender *conn, ok bool) {
	sess, ok = sh.sessions[id]
	if !ok {
		return nil, nil, false
	}
	sender = sh.sender
	delete(sh.sessions, id)
	return sess, sender, true
}

// takeSession removes the active routing entry. The ID was recorded by the
// connection-lifetime issuer before publication, so concurrent post-terminal
// frames remain recognizable after this removal without a second transition.
func (h *Hub) takeSession(sh *share, id protocol.SessionID) (sess *receiverSession, sender *conn, ok bool) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.shares[sh.id] != sh {
		return nil, nil, false
	}
	return h.takeSessionLocked(sh, id)
}

// killSession 终结单个会话(背压溢出/泵停摆),不波及同 share 其他会话
// (§6.8)。reason 回给双方;接收端连接随会话关闭(其生命周期 = 该会话)。
func (h *Hub) killSession(sh *share, sess *receiverSession, code, reason string) {
	h.mu.Lock()
	if h.shares[sh.id] != sh || sh.sessions[sess.id] != sess {
		h.mu.Unlock()
		return
	}
	_, sender, _ := h.takeSessionLocked(sh, sess.id)
	h.mu.Unlock()

	terminal := protocol.NewSessionError(sess.id.String(), code, reason)
	if sender != nil {
		if result, _ := sender.sendSessionTerminal(sess.id, terminal); result != forward.Enqueued {
			sender.pump.CloseSession(sess.id)
		}
	}
	if result, _ := sess.recv.sendSessionTerminal(sess.id, terminal); result != forward.Enqueued {
		sess.recv.pump.CloseSession(sess.id)
	}
	sess.recv.closeAfterFlush()
}

// endSession 正常终结会话(bye 或接收端断开);synthesizeBye 表示接收端
// 未及告别,由中转代发 bye,让发送端无需区分"告别"与"消失"。
func (h *Hub) endSession(sh *share, sess *receiverSession, synthesizeBye bool) {
	h.mu.Lock()
	if h.shares[sh.id] != sh || sh.sessions[sess.id] != sess {
		h.mu.Unlock()
		return
	}
	_, sender, _ := h.takeSessionLocked(sh, sess.id)
	h.mu.Unlock()

	if sender != nil {
		if synthesizeBye {
			if result, _ := sender.sendSessionTerminal(sess.id, protocol.NewBye(sess.id.String())); result != forward.Enqueued {
				sender.pump.CloseSession(sess.id)
			}
		} else {
			sender.pump.CloseSession(sess.id)
		}
	}
	sess.recv.pump.CloseSession(sess.id)
}

// findShare 返回活跃 share;挂起(宽限中)与不存在同样回"无":会话需要
// 活的发送端,接收端拿 not_found 后按退避重试即可,无需感知挂起态。
func (h *Hub) findShare(shareID string) *share {
	h.mu.Lock()
	defer h.mu.Unlock()
	sh, ok := h.shares[shareID]
	if !ok || sh.sender == nil {
		return nil
	}
	return sh
}
