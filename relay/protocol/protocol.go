package protocol

import "time"

// ProtocolVersion 是线协议版本位(§6.7):覆盖信令 JSON 与数据面帧布局,
// 体现在 WS 端点路径 /v1/ws/<shareId> 与 health/config 的版本通告。
// suiteByte 只版本化加密套件,管不到线协议。
const ProtocolVersion = "v1"

const (
	// KeepaliveInterval 是发送端 WS 保活间隔(§8);中转对 keepalive 原样回显,
	// 兼作应用层 pong。
	KeepaliveInterval = 20 * time.Second

	// SenderReconnectGrace:发送端断开后 shareId 会话挂起的宽限期,期内凭
	// resumeToken 原像 + sealedManifest 字节一致重注册恢复,期满回收(§6.8)。
	SenderReconnectGrace = 60 * time.Second

	// ResumeTokenBytes 是发送端会话续接令牌长度(crypto/rand 生成的本地秘密,
	// 不经链接/清单流出;每中转独立,§6.8)。register 提交其 SHA-256。
	ResumeTokenBytes = 16

	// SessionIDBytes:中转在 join 时分配的接收会话标识长度。值在线上保持
	// opaque;中转可在内部使用结构化发行来识别连接生命周期内的旧会话。
	SessionIDBytes = 8

	// MaxSignalingMessageBytes 是单条信令 JSON 文本帧的上限。信令只承载控制
	// 字段与 SDP/ICE(几 KiB 量级),64 KiB 已远超正常需要;设上限防止
	// 恶意超长 JSON 撑爆中转解析缓冲。
	MaxSignalingMessageBytes = 64 * 1024

	// MaxSignalingJSONDepth counts nested JSON objects and arrays, including the
	// top-level signaling object. A protocol-owned bound avoids inheriting the
	// different and unstable recursion ceilings of Go and JavaScript parsers.
	MaxSignalingJSONDepth = 64
)

// WebRTC 协商消息的 kind 取值(§6.7)。中转不解析、原样转发;常量供两端
// 客户端使用。
const (
	SignalKindOffer     = "offer"
	SignalKindAnswer    = "answer"
	SignalKindCandidate = "candidate"
)

// 信令 error 消息的 code 值域。取自描述字符串而非数字:信令面是 JSON,
// 可读的 code 让客户端分支与人工排障都不必查表。
const (
	// ErrCodeProtocol:消息违反线协议(未知类型、字段缺失、时序错误等),
	// 连接级致命。
	ErrCodeProtocol = "protocol_error"
	// ErrCodeShareIDMismatch:register/join 消息内 shareId 与 WS 路径不一致(§6.7)。
	ErrCodeShareIDMismatch = "share_id_mismatch"
	// ErrCodeShareIDCollision:shareId 已有活跃发送端;客户端应重生成(D13)。
	ErrCodeShareIDCollision = "share_id_collision"
	// ErrCodeResumeRejected:宽限期重注册的 token 原像校验失败或
	// sealedManifest 字节不一致(§6.8)。
	ErrCodeResumeRejected = "resume_rejected"
	// ErrCodeManifestTooLarge:清单帧超 MaxManifestSize。
	ErrCodeManifestTooLarge = "manifest_too_large"
	// ErrCodeManifestBudget:节点清单驻留内存预算耗尽,拒新 register(§6.8)。
	ErrCodeManifestBudget = "manifest_budget_exceeded"
	// ErrCodeRateLimited:join 命中每 shareId 或每 IP 限速(§6.8)。
	ErrCodeRateLimited = "rate_limited"
	// ErrCodeFrameTooLarge:二进制帧超限(转发帧超 MaxFrameSize+包裹头,
	// 或清单帧超 MaxManifestSize)。
	ErrCodeFrameTooLarge = "frame_too_large"
	// ErrCodeSessionOverflow:该会话的有界转发队列已满(慢接收端/恶意),
	// 中转断会话以保发送端 WS 不被队头阻塞(§6.8)。
	ErrCodeSessionOverflow = "session_overflow"
	// ErrCodeUnknownSession:sessionId 不存在或已随会话结束失效。
	ErrCodeUnknownSession = "unknown_session"
	// ErrCodeSenderGone:发送端断开,会话终结;接收端应指数退避 rejoin(§6.8)。
	ErrCodeSenderGone = "sender_gone"
)
