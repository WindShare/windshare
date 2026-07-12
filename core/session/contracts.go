package session

import (
	"context"
	"errors"
)

// ChannelState 是 FrameChannel 的生命周期状态。调度器凭它做热切换:
// Connecting 的通道先入池待命,Open 才参与分配,Closed 即出池并转移在途块(§6.6)。
type ChannelState int

const (
	Connecting ChannelState = iota
	Open
	Closed
)

// Frame 是一条完整的数据面线帧(定长小端布局,§6.7)。传输层原样搬运、
// 从不解释内容;语义只经本包 Encode*/Decode 出入。
type Frame []byte

// FrameChannel 是可靠有序的双向帧通道(§6.6)。WebRTC DataChannel 与 relay WS
// 在根模块各实现一个;调度器对两者无差别使用——"先中转、后 P2P、连上热切换、
// 双路聚合"复用同一套调度代码的前提,就是传输只剩"搬帧"这一件事(§3)。
type FrameChannel interface {
	Send(ctx context.Context, f Frame) error
	// SendTerminal is the only operation allowed to end a channel with a final
	// frame. A nil return means the transport accepted the terminal and will
	// expose it before closing Recv; Close must not overtake that acceptance.
	SendTerminal(ctx context.Context, f Frame) error
	Recv() <-chan Frame
	State() ChannelState
	Close() error
}

// BlockStore 是发送端的按块明文来源(§6.6)。实现读后须复核该块仍匹配快照
// (size/mtime),漂移即返回错误——发送会话把它转成分享级 ERROR 帧并中止。
type BlockStore interface {
	ReadBlock(index uint64) (plaintext []byte, err error)
	BlockCount() uint64
}

// DeliveryOrder states the ordering capability a Sink requires from the
// scheduler. It is an enum so future sink modes extend one semantic axis instead
// of accumulating caller-controlled booleans.
type DeliveryOrder uint8

const (
	// DeliveryAnyOrder is for random-write sinks whose block offsets are explicit.
	DeliveryAnyOrder DeliveryOrder = iota
	// DeliveryAscending requires WriteBlock calls in increasing global block order.
	DeliveryAscending
)

// Sink 落地已解密、已校验的明文(§6.6)。Have 报告已持有块,构造接收会话时
// 从需求集里扣除(断点续传);落盘前的路径校验在实现侧(§6.13)。
type Sink interface {
	WriteBlock(index uint64, plaintext []byte) error
	Have() Bitfield // 已持有块,用于断点续传
	DeliveryOrder() DeliveryOrder
}

// Sealer 是 chunk.Codec 在本包的消费侧投影(仅 Seal 面)。调度器与发送会话
// 只见密文字节与块号、不 import 加密包(§6.2),加解密一律经注入实现。
type Sealer interface {
	Seal(index uint64, plaintext []byte) ([]byte, error)
}

// Opener 验证并解密整块密文(nonce‖ct‖tag);AEAD 校验失败即返回错误,
// 调度器据此丢弃该次到达并重试。
type Opener interface {
	Open(index uint64, blockCT []byte) ([]byte, error)
}

// 会话级错误。
var (
	// ErrNilDependency:必需依赖未注入。会话全部 IO 经接口注入(§3 第二洞见),
	// 缺一即无法运行;构造期失败比运行期 panic 诚实。
	ErrNilDependency = errors.New("session: dependency must not be nil")

	// ErrSessionClosed:会话已被 Close 终止(或已结束,不再收编通道)。
	ErrSessionClosed = errors.New("session: session is closed")

	// ErrSessionReused:Run 只能调用一次——会话状态机不设复位,重跑请新建实例。
	ErrSessionReused = errors.New("session: Run may only be called once")

	// ErrPeerViolation:对端在可靠有序通道上发出畸形/越界帧。这不是丢包
	// (传输层保证完整送达),只能是实现缺陷或恶意,会话立即终止。
	ErrPeerViolation = errors.New("session: peer violated frame protocol")

	// ErrTerminalDelivery distinguishes the original session failure from a
	// transport that could not carry its terminal frame to the peer.
	ErrTerminalDelivery = errors.New("session: terminal frame delivery failed")

	// ErrInvalidDeliveryOrder prevents an unknown sink capability from silently
	// falling back to a mode that can corrupt a future sequential sink.
	ErrInvalidDeliveryOrder = errors.New("session: sink declared an invalid delivery order")
)
