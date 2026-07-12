package protocol

import (
	"encoding/base64"
	"fmt"
)

// 二进制 WS 帧的类型前缀(§6.7)。同一 WS 复用文本(信令 JSON)与二进制帧,
// 二进制帧以首字节区分用途;中转对帧体原样搬运、零解析。
const (
	// BinTypeManifest:清单帧,`0x01 ‖ sealedManifest`。紧随所属 JSON 消息
	// (register 上行 / manifest 下行),同连接 WS 有序,天然关联(§6.7)。
	BinTypeManifest byte = 0x01
	// BinTypeForward:回退数据面转发帧,`0x02 ‖ sessionId(8B) ‖ 内层数据面帧`。
	// 包裹只为路由;内层 REQUEST/BLOCK/ERROR 布局归 core/session,中转不解析。
	BinTypeForward byte = 0x02
	// BinTypeTerminalForward carries the final opaque inner frame. Its distinct
	// envelope lets every hop prioritize and tombstone the session without
	// inspecting encrypted block traffic or the inner ERROR payload.
	BinTypeTerminalForward byte = 0x03
)

// ForwardOverheadBytes 是转发帧包裹头长度(类型前缀 + sessionId);
// 中转的二进制帧限额 = MaxFrameSize + 此值。
const ForwardOverheadBytes = 1 + SessionIDBytes

// TerminalForwardOverheadBytes intentionally matches the ordinary envelope;
// the type byte alone carries lifecycle semantics.
const TerminalForwardOverheadBytes = 1 + SessionIDBytes

// ManifestOverheadBytes 是清单帧头长度(仅类型前缀)。
const ManifestOverheadBytes = 1

// SessionID 是中转分配的接收会话标识的原始字节形式。JSON 消息里以
// base64url 无填充字符串出现,转发帧头里以定长原始字节出现——定长使
// 帧头解析零分配、无需长度字段。
type SessionID [SessionIDBytes]byte

// String 返回 JSON 消息使用的线格式。
func (id SessionID) String() string {
	return base64.RawURLEncoding.EncodeToString(id[:])
}

// ParseSessionID 解析 JSON 消息里的 sessionId 线格式。
func ParseSessionID(s string) (SessionID, error) {
	var id SessionID
	b, err := decodeB64Fixed(s, SessionIDBytes, "sessionId")
	if err != nil {
		return id, err
	}
	copy(id[:], b)
	return id, nil
}

func validateSessionIDString(s string) error {
	_, err := ParseSessionID(s)
	return err
}

// EncodeManifestFrame 构造清单二进制帧。
func EncodeManifestFrame(sealed []byte) []byte {
	f := make([]byte, ManifestOverheadBytes+len(sealed))
	f[0] = BinTypeManifest
	copy(f[ManifestOverheadBytes:], sealed)
	return f
}

// DecodeManifestFrame 取出清单帧内的 sealedManifest 字节(共享底层数组,
// 不拷贝——中转按"原样字节"存储与回放,拷贝由调用方按需决定)。
func DecodeManifestFrame(frame []byte) ([]byte, error) {
	if len(frame) < ManifestOverheadBytes || frame[0] != BinTypeManifest {
		return nil, fmt.Errorf("protocol: not a manifest frame (len=%d)", len(frame))
	}
	return frame[ManifestOverheadBytes:], nil
}

// EncodeForwardFrame 用 sessionId 包裹一条内层数据面帧。
func EncodeForwardFrame(id SessionID, inner []byte) []byte {
	return encodeSessionFrame(BinTypeForward, id, inner)
}

func EncodeTerminalForwardFrame(id SessionID, inner []byte) []byte {
	return encodeSessionFrame(BinTypeTerminalForward, id, inner)
}

func encodeSessionFrame(kind byte, id SessionID, inner []byte) []byte {
	f := make([]byte, ForwardOverheadBytes+len(inner))
	f[0] = kind
	copy(f[1:], id[:])
	copy(f[ForwardOverheadBytes:], inner)
	return f
}

// DecodeForwardFrame 解出转发帧的路由头与内层帧(内层共享底层数组,零拷贝
// ——中转对内层零知识,原样入队转发)。
func DecodeForwardFrame(frame []byte) (SessionID, []byte, error) {
	return decodeSessionFrame(BinTypeForward, "forward", frame)
}

func DecodeTerminalForwardFrame(frame []byte) (SessionID, []byte, error) {
	return decodeSessionFrame(BinTypeTerminalForward, "terminal forward", frame)
}

func decodeSessionFrame(kind byte, name string, frame []byte) (SessionID, []byte, error) {
	var id SessionID
	if len(frame) < ForwardOverheadBytes || frame[0] != kind {
		return id, nil, fmt.Errorf("protocol: invalid %s frame (len=%d)", name, len(frame))
	}
	copy(id[:], frame[1:ForwardOverheadBytes])
	return id, frame[ForwardOverheadBytes:], nil
}

// BinType 返回二进制帧的类型前缀;空帧返回 0(非法值,两个已定义类型都非 0)。
func BinType(frame []byte) byte {
	if len(frame) == 0 {
		return 0
	}
	return frame[0]
}
