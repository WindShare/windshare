package relay

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/coder/websocket"

	"github.com/windshare/windshare/core/manifest"
	"github.com/windshare/windshare/core/session"
	"github.com/windshare/windshare/relay/forward"
	"github.com/windshare/windshare/relay/protocol"
)

const (
	// clientDataReadLimit 是会话期入站 WS 消息上限:转发帧(内层帧+包裹头)
	// 与信令 JSON 取大者,与中转的 dataReadLimit 同规——两端限额一致,
	// 合法帧不会死于任一侧的读限。
	clientDataReadLimit = max(session.MaxFrameSize+protocol.ForwardOverheadBytes, protocol.MaxSignalingMessageBytes)

	// manifestReadLimit 是 join 应答期的读限:该阶段唯一的大消息是清单帧,
	// 上限即中转允许存续的清单上限。
	manifestReadLimit = manifest.MaxManifestSize + protocol.ManifestOverheadBytes

	// writeTimeout 与中转同取值:出站已泵化,一次 WS 写还能拖过它的连接
	// 已不可用,及时失败触发链路清理(重连或上层 rejoin)。
	writeTimeout = 30 * time.Second

	// The client reuses relay/forward's session-aware outbox so local and relay
	// hops cannot disagree about terminal ordering or per-session fairness.
	connectionLaneMessages = 64
	sessionControlMessages = 16
	dataLaneFrames         = 16

	// Ordinary data may consume only recvBufferFrames slots. One additional
	// slot is reserved so an accepted terminal remains observable even when the
	// application has stopped draining ordinary frames.
	recvBufferFrames    = 32
	recvTerminalReserve = 1

	// A session event is emitted once, while signaling remains independently
	// bounded on each Channel. Neither queue may block the shared WS reader.
	sessionEventBuffer = 8
	signalBuffer       = 16

	// DefaultJoinRetryWindow 是 join 对 not_found 的默认退避窗口(§6.7
	// "短窗",覆盖 join 先于 register 的良性竞态)。断线 rejoin 需跨越
	// 发送端重连宽限时,调用方应把窗口放大到 SenderReconnectGrace 之上。
	DefaultJoinRetryWindow = 10 * time.Second

	// A relay hint is attacker-controlled link input. Its rendered authority is
	// capped so one failed dial cannot amplify it into an unbounded terminal log.
	maxRelayEndpointDiagnosticBytes = 512
)

// 指数退避默认参数:首拍短到人无感知,封顶后轮询节奏对中转无压力。
const (
	DefaultBackoffInitial = 200 * time.Millisecond
	DefaultBackoffMax     = 5 * time.Second
)

// Backoff 是指数退避参数(发送端重连与接收端 join 重试共用);零值取默认。
type Backoff struct {
	Initial time.Duration
	Max     time.Duration
}

func (b Backoff) withDefaults() Backoff {
	if b.Initial <= 0 {
		b.Initial = DefaultBackoffInitial
	}
	if b.Max <= 0 {
		b.Max = DefaultBackoffMax
	}
	return b
}

var (
	// ErrConnClosed:连接已被本地 Close 或链路已死。
	ErrConnClosed = errors.New("relay: connection closed")

	// ErrChannelClosed:FrameChannel 已闭合(bye/对端消失/链路断开)。
	ErrChannelClosed = errors.New("relay: frame channel closed")

	// ErrShareNotFound:join 在整个退避窗口内始终得到 not_found。
	ErrShareNotFound = errors.New("relay: share not found within retry window")

	ErrSessionQueueFull = errors.New("relay: session outbound queue full")
	ErrSessionTerminal  = errors.New("relay: session is terminal")
)

// ServerError 是中转经信令 error 消息报告的拒绝;Code 值域见 relay/protocol。
type ServerError struct {
	Code    string
	Message string
}

func (e *ServerError) Error() string {
	return fmt.Sprintf("relay: server rejected: %s (%s)", e.Code, e.Message)
}

// Signal is one opaque WebRTC negotiation message scoped to a Channel. Session
// identity belongs to the channel itself, so a signal cannot be applied to a
// sibling session accidentally.
type Signal struct {
	Kind    string
	Payload json.RawMessage
}

type relayURLSyntaxError struct{ cause error }

func (e *relayURLSyntaxError) Error() string { return "relay: invalid relay URL" }
func (e *relayURLSyntaxError) Unwrap() error { return e.cause }

type relayDialError struct {
	endpoint   string
	statusCode int
	cause      error
}

func (e *relayDialError) Error() string {
	if e.statusCode != 0 {
		return fmt.Sprintf("relay: dial %s: HTTP status %d", e.endpoint, e.statusCode)
	}
	return fmt.Sprintf("relay: dial %s: connection failed", e.endpoint)
}

func (e *relayDialError) Unwrap() error { return e.cause }

func publicRelayEndpoint(u *url.URL) string {
	redacted := *u
	// Relay hints can legitimately carry deployment credentials in userinfo,
	// path segments, or query parameters. They are needed for the request but
	// never for diagnosis; scheme and authority are sufficient to locate a node.
	redacted.User = nil
	redacted.Path = ""
	redacted.RawPath = ""
	redacted.RawQuery = ""
	redacted.ForceQuery = false
	redacted.Fragment = ""
	redacted.RawFragment = ""
	endpoint := strings.ToValidUTF8(redacted.String(), "\uFFFD")
	if len(endpoint) <= maxRelayEndpointDiagnosticBytes {
		return endpoint
	}
	end := maxRelayEndpointDiagnosticBytes
	for end > 0 && !utf8.RuneStart(endpoint[end]) {
		end--
	}
	return endpoint[:end] + "…"
}

// dialWS 建立到中转 /v1/ws/<shareId> 端点的 WS 连接(§6.7:shareId 入路径,
// 版本位取 protocol.ProtocolVersion)。
func dialWS(ctx context.Context, relayURL, shareID string, hc *http.Client) (*websocket.Conn, error) {
	u, err := relayWebSocketURL(relayURL, shareID)
	if err != nil {
		return nil, err
	}
	ws, response, err := websocket.Dial(ctx, u.String(), &websocket.DialOptions{HTTPClient: hc})
	if err != nil {
		statusCode := 0
		if response != nil {
			statusCode = response.StatusCode
		}
		// HTTP and websocket errors commonly retain the complete request URL.
		// Keep the cause machine-readable while rendering only a secret-free endpoint.
		return nil, &relayDialError{endpoint: publicRelayEndpoint(u), statusCode: statusCode, cause: err}
	}
	return ws, nil
}

// outItem 是出站队列元素;binary 区分 WS 文本(信令 JSON)与二进制帧。
type outItem struct {
	binary bool
	data   []byte
}

// link is one live WS connection. The shared session-aware pump is the sole
// writer; role-specific goroutines remain the sole readers.
type link struct {
	ws     *websocket.Conn
	ctx    context.Context
	cancel context.CancelFunc
	pump   *forward.Pump

	wg       sync.WaitGroup
	failOnce sync.Once
	err      error
}

type linkWriter struct{ l *link }

func (w linkWriter) WriteText(data []byte) error {
	return w.l.writeWS(outItem{data: data})
}

func (w linkWriter) WriteBinary(data []byte) error {
	return w.l.writeWS(outItem{binary: true, data: data})
}

func newLink(ws *websocket.Conn, keepalive time.Duration) *link {
	ctx, cancel := context.WithCancel(context.Background())
	l := &link{ws: ws, ctx: ctx, cancel: cancel}
	l.pump = forward.NewPump(linkWriter{l: l}, forward.Options{
		ConnectionQueueMessages: connectionLaneMessages,
		SessionControlMessages:  sessionControlMessages,
		SessionQueueFrames:      dataLaneFrames,
	})
	l.wg.Go(func() {
		l.keepaliveLoop(keepalive)
	})
	return l
}

// fail 终止链路(幂等):记录首因、撤销 ctx 并撕掉 WS。err 在 cancel 之前
// 落位,凭 channel close 的先行发生序,任何等到 ctx.Done 的读者都能安全读它。
func (l *link) fail(err error) {
	l.failOnce.Do(func() {
		l.err = err
		l.cancel()
		l.pump.Close()
		_ = l.ws.CloseNow()
	})
}

// cause 返回链路死亡首因;阻塞至链路确已终止。
func (l *link) cause() error {
	<-l.ctx.Done()
	return l.err
}

func (l *link) wait() {
	l.wg.Wait()
	<-l.pump.Done()
}

// writeWS 是 sendLoop 的出口;写失败即链路死亡(首因保真)。
func (l *link) writeWS(it outItem) error {
	ctx, cancel := context.WithTimeout(l.ctx, writeTimeout)
	defer cancel()
	typ := websocket.MessageText
	if it.binary {
		typ = websocket.MessageBinary
	}
	if err := l.ws.Write(ctx, typ, it.data); err != nil {
		l.fail(err)
		return err
	}
	return nil
}

// keepaliveLoop 周期性经高优先通道发 keepalive(§6.7);中转原样回显,
// 由读循环吞掉。发送端与接收端同开:退避/静默窗口里它同时兼任 NAT 保活。
func (l *link) keepaliveLoop(interval time.Duration) {
	wire := mustEncode(protocol.NewKeepalive())
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		select {
		case <-t.C:
			if result := l.pump.EnqueueConnection(false, wire); result != forward.Enqueued {
				l.fail(fmt.Errorf("relay: keepalive enqueue failed: %v", result))
				return
			}
		case <-l.ctx.Done():
			return
		}
	}
}

func (l *link) enqueueSessionControl(ctx context.Context, sid protocol.SessionID, it outItem) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	select {
	case <-l.ctx.Done():
		return ErrConnClosed
	default:
	}
	return sessionEnqueueError(l.pump.EnqueueSessionControl(sid, it.binary, it.data))
}

func (l *link) enqueueForward(ctx context.Context, sid protocol.SessionID, frame []byte) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	select {
	case <-l.ctx.Done():
		return ErrConnClosed
	default:
	}
	result := l.pump.EnqueueForwardContext(ctx, sid, frame)
	if result == forward.ContextDone {
		return ctx.Err()
	}
	return sessionEnqueueError(result)
}

func (l *link) deliverTerminal(ctx context.Context, sid protocol.SessionID, it outItem) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	select {
	case <-l.ctx.Done():
		return ErrConnClosed
	default:
	}
	result, delivered := l.pump.EnqueueSessionTerminal(sid, it.binary, it.data)
	if err := sessionEnqueueError(result); err != nil {
		return err
	}
	select {
	case err := <-delivered:
		if err != nil {
			return fmt.Errorf("relay: terminal delivery: %w", err)
		}
		return nil
	case <-l.ctx.Done():
		return ErrConnClosed
	case <-ctx.Done():
		return ctx.Err()
	}
}

// enqueueTerminalNoWait transfers terminal ownership to the shared pump without
// waiting for the socket writer. Ingress-overflow handling runs on the sole WS
// reader and must never let a slow outbound writer block sibling sessions.
func (l *link) enqueueTerminalNoWait(sid protocol.SessionID, it outItem) error {
	result, _ := l.pump.EnqueueSessionTerminal(sid, it.binary, it.data)
	return sessionEnqueueError(result)
}

func sessionEnqueueError(result forward.EnqueueResult) error {
	switch result {
	case forward.Enqueued:
		return nil
	case forward.Overflow:
		return ErrSessionQueueFull
	case forward.UnknownSession, forward.SessionTerminated:
		return ErrSessionTerminal
	case forward.PumpClosed:
		return ErrConnClosed
	default:
		return fmt.Errorf("relay: unknown outbox result %d", result)
	}
}

// mustEncode 编码本包字面构造的信令消息;失败只能是编程错误。
func mustEncode(m protocol.Message) []byte {
	b, err := protocol.Encode(m)
	if err != nil {
		panic("relay: encode signaling message: " + err.Error())
	}
	return b
}

// waitBackoff 睡一个退避周期(封顶剩余窗口)并倍增 delay;返回 false 表示
// 窗口已尽或 ctx/stop 先到。stop 为 nil 时仅受 ctx 与窗口约束。
func waitBackoff(ctx context.Context, stop <-chan struct{}, deadline time.Time, delay *time.Duration, maxDelay time.Duration) bool {
	remain := time.Until(deadline)
	if remain <= 0 {
		return false
	}
	d := min(*delay, remain)
	*delay = min(*delay*2, maxDelay)
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-t.C:
		return true
	case <-ctx.Done():
		return false
	case <-stop:
		return false
	}
}
