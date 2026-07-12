package protocol

// ServerConfig 是 GET /v1/config 的响应体(§6.7/§6.8):通告支持的线协议
// 版本列表与限额,供客户端预检——中转是第三方长期部署的基础设施,
// 客户端↔中转版本错配必然发生,错配即明确拒绝优于神秘失败。
type ServerConfig struct {
	ProtocolVersions []string     `json:"protocolVersions"`
	Limits           ServerLimits `json:"limits"`
}

// ServerLimits publishes deployment policy that clients can use for preflight.
// Live capacity and internal protocol-abuse/queue guards are omitted because
// they are neither stable client budgets nor actionable before dialing.
type ServerLimits struct {
	// MaxFrameSize:数据面单帧上限(含内层帧头,不含转发包裹头)。
	MaxFrameSize int64 `json:"maxFrameSize"`
	// MaxManifestSize:清单帧内 sealedManifest 的上限;发送端出链接前预检(§6.9)。
	MaxManifestSize int64 `json:"maxManifestSize"`
	// MaxSignalingMessageBytes:单条信令 JSON 的上限。
	MaxSignalingMessageBytes int64 `json:"maxSignalingMessageBytes"`
	// MaxHeaderBytes bounds the HTTP request headers before WebSocket upgrade.
	MaxHeaderBytes int `json:"maxHeaderBytes"`
	// Admission exposes effective static policy. Live remaining capacity is
	// intentionally not advertised because it races every request.
	Admission AdmissionLimits `json:"admission"`
	// Timeouts are milliseconds to avoid language-specific duration encodings.
	Timeouts ServerTimeouts `json:"timeouts"`
}

type RateLimit struct {
	PerSecond float64 `json:"perSecond"`
	Burst     int     `json:"burst"`
}

type AdmissionLimits struct {
	MaxConnections            int       `json:"maxConnections"`
	MaxConcurrentShares       int       `json:"maxConcurrentShares"`
	MaxSharesPerSource        int       `json:"maxSharesPerSource"`
	MaxManifestBytesPerSource int64     `json:"maxManifestBytesPerSource"`
	MaxTotalManifestBytes     int64     `json:"maxTotalManifestBytes"`
	RegisterPerSource         RateLimit `json:"registerPerSource"`
	JoinPerSource             RateLimit `json:"joinPerSource"`
	JoinPerShare              RateLimit `json:"joinPerShare"`
}

type ServerTimeouts struct {
	HTTPReadHeaderMilliseconds       int64 `json:"httpReadHeaderMilliseconds"`
	HTTPReadMilliseconds             int64 `json:"httpReadMilliseconds"`
	HTTPIdleMilliseconds             int64 `json:"httpIdleMilliseconds"`
	WebSocketRoleMilliseconds        int64 `json:"webSocketRoleMilliseconds"`
	WebSocketKeepaliveMilliseconds   int64 `json:"webSocketKeepaliveMilliseconds"`
	SenderReconnectGraceMilliseconds int64 `json:"senderReconnectGraceMilliseconds"`
}

// Health 是 GET /healthz 的响应体。
type Health struct {
	Status string `json:"status"`
}

// HealthOK 是健康响应的唯一正常值;不健康的节点直接不回 200。
const HealthOK = "ok"
