package session

import (
	"encoding/binary"
	"errors"
	"fmt"
	"math"
	"unicode/utf8"
)

// 帧类型字节(M0 钉死,金标向量跨 Go/TS 逐字节对拍,值不可再改;契约 §7)。
const (
	FrameRequest byte = 0x01
	FrameBlock   byte = 0x02
	FrameError   byte = 0x03
)

// FlagLast(bit0)标记 BLOCK 帧是该块密文的末帧;其余位 M1 未定义,编解码
// 都要求为零——宽容未知位会让金标对拍失去唯一性,也给注入留缝。
const FlagLast byte = 0x01

// 各帧头长度由 §6.7 布局直接加总;解码以此界定最小长度,编码以此预分配。
const (
	requestHeaderLen = 1 + 4             // type ‖ n(u32)
	blockHeaderLen   = 1 + 8 + 4 + 1 + 4 // type ‖ index(u64) ‖ seq(u32) ‖ flags(u8) ‖ len(u32)
	errorHeaderLen   = 1 + 2 + 2         // type ‖ code(u16) ‖ msglen(u16)
)

// 统一不变量:任何编码后的线帧总长 ≤ MaxFrameSize——它是 DataChannel 单消息
// 的跨浏览器安全值,帧头不享豁免。由此派生各类型的载荷上限。
const (
	// MaxBlockPayload 是单 BLOCK 帧的 payload 上限;块密文按它切帧(§6.7)。
	MaxBlockPayload = MaxFrameSize - blockHeaderLen

	// MaxRequestIndices 是单 REQUEST 帧可携带的块号数上限。
	MaxRequestIndices = (MaxFrameSize - requestHeaderLen) / 8

	// MaxErrorMsgBytes 是 ERROR 帧消息的字节上限(亦受 msglen 的 u16 值域约束,
	// 两者取小即此值)。
	MaxErrorMsgBytes = MaxFrameSize - errorHeaderLen
)

// 数据面 ERROR code 值域(M1 在此钉死;契约 §7)。分级决定接收端反应:
// 通道级只弃用来路通道(其余通道继续供块),分享级令整个会话失败——
// 换通道重试也会命中同一份出错的源。
const (
	// ErrCodeBadRequest:对端违反帧协议(畸形帧/块号越界/不该出现的帧型)。通道级。
	ErrCodeBadRequest uint16 = 0x0001

	// ErrCodeBlockRead:发送端读块失败,含快照漂移中止(§6.6)。分享级。
	ErrCodeBlockRead uint16 = 0x0002

	// ErrCodeSeal:发送端加密失败,含 Seal 计数熔断(B12)。分享级。
	ErrCodeSeal uint16 = 0x0003
)

// FatalCode 报告 code 是否分享级致命(而非仅该通道作废)。导出供 TS 端调度
// 器对齐同一分级语义。
func FatalCode(code uint16) bool {
	return code == ErrCodeBlockRead || code == ErrCodeSeal
}

// 帧编解码错误。解码面向网络对端输入,拒绝必须可区分,调用方以 errors.Is 分派。
var (
	ErrUnknownFrameType = errors.New("session: unknown frame type")
	ErrFrameMalformed   = errors.New("session: frame does not match its declared layout")
	ErrFrameOversize    = errors.New("session: frame exceeds the size limit")
	ErrEmptyRequest     = errors.New("session: REQUEST contains no block indices")
	ErrEmptyPayload     = errors.New("session: BLOCK payload is empty")
	ErrUnknownFlags     = errors.New("session: BLOCK flags contain undefined bits")
	ErrInvalidUTF8      = errors.New("session: ERROR message is not valid UTF-8")
)

// Message 是数据面帧的解码形态,Decode 依 type 字节返回三者之一。
type Message interface{ frameType() byte }

// Request 请求若干块(§6.7)。
type Request struct{ Indices []uint64 }

// Block 承载某块密文的一帧:blockCT(nonce‖ct‖tag)作为整体字节串按
// MaxBlockPayload 切分,Seq 自 0 递增、末帧置 Last;nonce 自然落在首帧,
// 帧布局对加密细节零感知(§6.7)。
type Block struct {
	Index   uint64
	Seq     uint32
	Last    bool
	Payload []byte
}

// Error 报告错误(§6.7)。它同时实现 error:接收会话把对端报告的分享级
// 错误原样上抛,调用方 errors.As 取 Code 分派。
type Error struct {
	Code uint16
	Msg  string
}

func (*Request) frameType() byte { return FrameRequest }
func (*Block) frameType() byte   { return FrameBlock }
func (*Error) frameType() byte   { return FrameError }

func (e *Error) Error() string {
	return fmt.Sprintf("session: peer reported error 0x%04x: %s", e.Code, e.Msg)
}

// EncodeRequest 编码 REQUEST 帧。空请求无语义;块号数受整帧 ≤ MaxFrameSize
// 约束。
func EncodeRequest(indices []uint64) (Frame, error) {
	if len(indices) == 0 {
		return nil, ErrEmptyRequest
	}
	if len(indices) > MaxRequestIndices {
		return nil, fmt.Errorf("%w: REQUEST contains %d indices, limit %d", ErrFrameOversize, len(indices), MaxRequestIndices)
	}
	f := make(Frame, requestHeaderLen+8*len(indices))
	f[0] = FrameRequest
	binary.LittleEndian.PutUint32(f[1:], uint32(len(indices)))
	for k, idx := range indices {
		binary.LittleEndian.PutUint64(f[requestHeaderLen+8*k:], idx)
	}
	return f, nil
}

// EncodeBlock 编码 BLOCK 帧。空 payload 只可能是协议误用(blockCT 至少含
// nonce‖tag),且会让重组的帧数上界失效,直接拒绝。
func EncodeBlock(b Block) (Frame, error) {
	if len(b.Payload) == 0 {
		return nil, ErrEmptyPayload
	}
	if len(b.Payload) > MaxBlockPayload {
		return nil, fmt.Errorf("%w: BLOCK payload is %d bytes, limit %d", ErrFrameOversize, len(b.Payload), MaxBlockPayload)
	}
	f := make(Frame, blockHeaderLen+len(b.Payload))
	f[0] = FrameBlock
	binary.LittleEndian.PutUint64(f[1:], b.Index)
	binary.LittleEndian.PutUint32(f[9:], b.Seq)
	if b.Last {
		f[13] = FlagLast
	}
	binary.LittleEndian.PutUint32(f[14:], uint32(len(b.Payload)))
	copy(f[blockHeaderLen:], b.Payload)
	return f, nil
}

// truncateErrorMessage preserves UTF-8 because terminal diagnostics may be
// the peer's only explanation; splitting a rune would turn a valid error into
// a malformed terminal frame.
func truncateErrorMessage(msg string) string {
	if len(msg) <= MaxErrorMsgBytes {
		return msg
	}
	end := MaxErrorMsgBytes
	for end > 0 && !utf8.RuneStart(msg[end]) {
		end--
	}
	return msg[:end]
}

// EncodeError 编码 ERROR 帧。msg 是给人看的辅助信息,语义以 code 为准。
func EncodeError(code uint16, msg string) (Frame, error) {
	if !utf8.ValidString(msg) {
		return nil, ErrInvalidUTF8
	}
	if len(msg) > MaxErrorMsgBytes {
		return nil, fmt.Errorf("%w: ERROR message is %d bytes, limit %d", ErrFrameOversize, len(msg), MaxErrorMsgBytes)
	}
	f := make(Frame, errorHeaderLen+len(msg))
	f[0] = FrameError
	binary.LittleEndian.PutUint16(f[1:], code)
	binary.LittleEndian.PutUint16(f[3:], uint16(len(msg)))
	copy(f[errorHeaderLen:], msg)
	return f, nil
}

// Decode 解码一条线帧。任何长度与声明不符、未知类型/标志位、越限一律拒绝:
// 帧来自网络对端,宽容解析等于把畸形输入放进调度器(§6.7)。返回的 Message
// 与输入缓冲无共享——传输层可能复用 Frame 底层数组。
func Decode(f Frame) (Message, error) {
	if len(f) > MaxFrameSize {
		return nil, fmt.Errorf("%w: %d bytes exceeds MaxFrameSize", ErrFrameOversize, len(f))
	}
	if len(f) == 0 {
		return nil, fmt.Errorf("%w: empty frame", ErrFrameMalformed)
	}
	switch f[0] {
	case FrameRequest:
		if len(f) < requestHeaderLen {
			return nil, fmt.Errorf("%w: truncated REQUEST header (%d bytes)", ErrFrameMalformed, len(f))
		}
		n := binary.LittleEndian.Uint32(f[1:])
		if n == 0 {
			return nil, ErrEmptyRequest
		}
		if uint64(len(f)) != requestHeaderLen+8*uint64(n) {
			return nil, fmt.Errorf("%w: REQUEST declares %d indices in a %d-byte frame", ErrFrameMalformed, n, len(f))
		}
		indices := make([]uint64, n)
		for k := range indices {
			indices[k] = binary.LittleEndian.Uint64(f[requestHeaderLen+8*k:])
		}
		return &Request{Indices: indices}, nil

	case FrameBlock:
		if len(f) < blockHeaderLen {
			return nil, fmt.Errorf("%w: truncated BLOCK header (%d bytes)", ErrFrameMalformed, len(f))
		}
		flags := f[13]
		if flags&^FlagLast != 0 {
			return nil, fmt.Errorf("%w:0x%02x", ErrUnknownFlags, flags)
		}
		plen := binary.LittleEndian.Uint32(f[14:])
		if plen == 0 {
			return nil, ErrEmptyPayload
		}
		if uint64(len(f)) != blockHeaderLen+uint64(plen) {
			return nil, fmt.Errorf("%w: BLOCK declares a %d-byte payload in a %d-byte frame", ErrFrameMalformed, plen, len(f))
		}
		return &Block{
			Index:   binary.LittleEndian.Uint64(f[1:]),
			Seq:     binary.LittleEndian.Uint32(f[9:]),
			Last:    flags&FlagLast != 0,
			Payload: append([]byte(nil), f[blockHeaderLen:]...),
		}, nil

	case FrameError:
		if len(f) < errorHeaderLen {
			return nil, fmt.Errorf("%w: truncated ERROR header (%d bytes)", ErrFrameMalformed, len(f))
		}
		msglen := binary.LittleEndian.Uint16(f[3:])
		if len(f) != errorHeaderLen+int(msglen) {
			return nil, fmt.Errorf("%w: ERROR declares a %d-byte message in a %d-byte frame", ErrFrameMalformed, msglen, len(f))
		}
		msg := f[errorHeaderLen:]
		if !utf8.Valid(msg) {
			return nil, ErrInvalidUTF8
		}
		return &Error{
			Code: binary.LittleEndian.Uint16(f[1:]),
			Msg:  string(msg),
		}, nil

	default:
		return nil, fmt.Errorf("%w:0x%02x", ErrUnknownFrameType, f[0])
	}
}

// SplitBlockCT 把整块密文(nonce‖ct‖tag)切成 BLOCK 帧序列:seq 自 0 递增、
// 末帧置 last(§6.7)。纯函数;maxPayload 参数化以便小尺寸单测,生产路径
// 一律传 MaxBlockPayload。
func SplitBlockCT(index uint64, blockCT []byte, maxPayload int) ([]Frame, error) {
	if len(blockCT) == 0 {
		return nil, fmt.Errorf("%w: block ciphertext is empty", ErrEmptyPayload)
	}
	if maxPayload <= 0 || maxPayload > MaxBlockPayload {
		return nil, fmt.Errorf("%w: maxPayload=%d is outside 1..%d", ErrFrameOversize, maxPayload, MaxBlockPayload)
	}
	nFrames := (len(blockCT) + maxPayload - 1) / maxPayload
	if uint64(nFrames) > math.MaxUint32 {
		// seq 是 u32;按 MaxBlockBytes 的现实取值不可达,仅防常量演化悄然破界。
		return nil, fmt.Errorf("%w: block requires %d frames, exceeding the seq range", ErrFrameOversize, nFrames)
	}
	frames := make([]Frame, 0, nFrames)
	for seq := range nFrames {
		lo := seq * maxPayload
		hi := min(lo+maxPayload, len(blockCT))
		f, err := EncodeBlock(Block{
			Index:   index,
			Seq:     uint32(seq),
			Last:    seq == nFrames-1,
			Payload: blockCT[lo:hi],
		})
		if err != nil {
			return nil, err
		}
		frames = append(frames, f)
	}
	return frames, nil
}
