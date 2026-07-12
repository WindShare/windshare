package protocol

import (
	"bytes"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"unicode/utf8"
)

// 信令 JSON 消息类型标签(§6.7)。
const (
	TypeRegister   = "register"
	TypeRegistered = "registered"
	TypeKeepalive  = "keepalive"
	TypeJoin       = "join"
	TypeManifest   = "manifest"
	TypeNotFound   = "not_found"
	TypeSignal     = "signal"
	TypeBye        = "bye"
	TypeError      = "error"
)

// Message 是信令消息的封闭集合:每个具体类型自带 Type 标签与字段校验。
// Encode/Decode 是唯一出入口,保证两端(transport/relay 客户端与中转)
// 对 schema 的理解逐字节一致。
type Message interface {
	// messageType 返回类型标签;非导出以封闭实现集合——线协议消息不允许
	// 在包外扩展,新增消息类型必须落在本包并升版本位。
	messageType() string
	// validate 校验必填字段;Encode 与 Decode 双向执行,坏消息不出门也不进门。
	validate() error
}

// Register:发送端→中转,注册分享;清单二进制帧紧随其后(§6.7)。
// ResumeToken 仅在断线宽限期重注册时出示(原像,§6.8),首次注册留空。
type Register struct {
	Type            string `json:"type"`
	ShareID         string `json:"shareId"`
	ResumeTokenHash string `json:"resumeTokenHash"`       // base64url 无填充的 SHA-256(resumeToken)
	ResumeToken     string `json:"resumeToken,omitempty"` // 宽限期重注册出示的原像(base64url 无填充)
}

// Registered:中转→发送端,注册(含清单入库)成功确认。发送端收到它才可
// 打印链接(§6.9)——没有 ack 就只能以"未收到 error"猜测成功,测试与
// 客户端都得靠 sleep,违背最小惊讶。
type Registered struct {
	Type    string `json:"type"`
	ShareID string `json:"shareId"`
}

// Keepalive:发送端↔中转保活;中转原样回显兼作应用层 pong(§6.7)。
type Keepalive struct {
	Type string `json:"type"`
}

// Join:接收端→中转,加入分享(§6.7)。
type Join struct {
	Type    string `json:"type"`
	ShareID string `json:"shareId"`
}

// Manifest:中转→接收端,join 成功;清单二进制帧紧随其后。SessionID 由
// 中转分配、标识该接收会话,随会话结束失效(§6.7)。
type Manifest struct {
	Type      string `json:"type"`
	SessionID string `json:"sessionId"`
}

// NotFound:中转→接收端,shareId 无活跃分享;接收端短窗指数退避重试(§6.7)。
type NotFound struct {
	Type string `json:"type"`
}

// Signal:WebRTC 协商消息,双向经中转转发(§6.7)。Payload 对中转不透明,
// 原样转发零解析。
type Signal struct {
	Type      string          `json:"type"`
	SessionID string          `json:"sessionId"`
	Kind      string          `json:"kind"`
	Payload   json.RawMessage `json:"payload"`
}

// Bye:结束会话,双向(§6.7)。接收端断连时中转也会代其向发送端合成 Bye,
// 使发送端无需区分"对端主动告别"与"对端消失"。
type Bye struct {
	Type      string `json:"type"`
	SessionID string `json:"sessionId"`
}

// Error:中转→客户端的错误通报。SessionID 非空表示会话级错误(该会话
// 已终结、连接仍可用),为空表示连接级错误(随后连接关闭)。
type Error struct {
	Type      string `json:"type"`
	Code      string `json:"code"`
	Message   string `json:"message,omitempty"`
	SessionID string `json:"sessionId,omitempty"`
}

func NewRegister(shareID, resumeTokenHash string) *Register {
	return &Register{Type: TypeRegister, ShareID: shareID, ResumeTokenHash: resumeTokenHash}
}

func NewResumeRegister(shareID, resumeTokenHash, resumeToken string) *Register {
	return &Register{Type: TypeRegister, ShareID: shareID, ResumeTokenHash: resumeTokenHash, ResumeToken: resumeToken}
}

func NewRegistered(shareID string) *Registered {
	return &Registered{Type: TypeRegistered, ShareID: shareID}
}
func NewKeepalive() *Keepalive     { return &Keepalive{Type: TypeKeepalive} }
func NewJoin(shareID string) *Join { return &Join{Type: TypeJoin, ShareID: shareID} }
func NewManifest(sessionID string) *Manifest {
	return &Manifest{Type: TypeManifest, SessionID: sessionID}
}
func NewNotFound() *NotFound { return &NotFound{Type: TypeNotFound} }

func NewSignal(sessionID, kind string, payload json.RawMessage) *Signal {
	return &Signal{Type: TypeSignal, SessionID: sessionID, Kind: kind, Payload: payload}
}

func NewBye(sessionID string) *Bye { return &Bye{Type: TypeBye, SessionID: sessionID} }

func NewError(code, message string) *Error {
	return &Error{Type: TypeError, Code: code, Message: message}
}

func NewSessionError(sessionID, code, message string) *Error {
	return &Error{Type: TypeError, Code: code, Message: message, SessionID: sessionID}
}

func (m *Register) messageType() string   { return TypeRegister }
func (m *Registered) messageType() string { return TypeRegistered }
func (m *Keepalive) messageType() string  { return TypeKeepalive }
func (m *Join) messageType() string       { return TypeJoin }
func (m *Manifest) messageType() string   { return TypeManifest }
func (m *NotFound) messageType() string   { return TypeNotFound }
func (m *Signal) messageType() string     { return TypeSignal }
func (m *Bye) messageType() string        { return TypeBye }
func (m *Error) messageType() string      { return TypeError }

func (m *Register) validate() error {
	if err := ValidateShareID(m.ShareID); err != nil {
		return err
	}
	if _, err := decodeB64Fixed(m.ResumeTokenHash, sha256.Size, "resumeTokenHash"); err != nil {
		return err
	}
	if m.ResumeToken != "" {
		if _, err := decodeB64Fixed(m.ResumeToken, ResumeTokenBytes, "resumeToken"); err != nil {
			return err
		}
	}
	return nil
}

func (m *Registered) validate() error { return ValidateShareID(m.ShareID) }
func (m *Keepalive) validate() error  { return nil }
func (m *Join) validate() error       { return ValidateShareID(m.ShareID) }
func (m *Manifest) validate() error   { return validateSessionIDString(m.SessionID) }
func (m *NotFound) validate() error   { return nil }

func (m *Signal) validate() error {
	if err := validateSessionIDString(m.SessionID); err != nil {
		return err
	}
	if m.Kind == "" {
		return errors.New("protocol: signal is missing kind")
	}
	if !utf8.ValidString(m.Kind) {
		return errors.New("protocol: signal kind must be Unicode scalar text")
	}
	if len(m.Payload) == 0 || !utf8.Valid(m.Payload) {
		return errors.New("protocol: signal payload must be valid UTF-8 JSON")
	}
	if !jsonNestingWithinLimit(m.Payload, MaxSignalingJSONDepth-1) {
		return fmt.Errorf("protocol: signal payload exceeds JSON nesting depth %d", MaxSignalingJSONDepth-1)
	}
	if !json.Valid(m.Payload) {
		return errors.New("protocol: signal payload must be valid UTF-8 JSON")
	}
	return nil
}

func (m *Bye) validate() error { return validateSessionIDString(m.SessionID) }

func (m *Error) validate() error {
	if m.Code == "" {
		return errors.New("protocol: error is missing code")
	}
	if !utf8.ValidString(m.Code) {
		return errors.New("protocol: error code must be Unicode scalar text")
	}
	if m.Message != "" && !utf8.ValidString(m.Message) {
		return errors.New("protocol: error message must be Unicode scalar text")
	}
	if m.SessionID != "" {
		return validateSessionIDString(m.SessionID)
	}
	return nil
}

// Encode 序列化一条信令消息;先校验,坏消息不出门。
func Encode(m Message) ([]byte, error) {
	if m == nil {
		return nil, errors.New("protocol: signaling message is nil")
	}
	if got, want := typeTagOf(m), m.messageType(); got != want {
		return nil, fmt.Errorf("protocol: Type field %q does not match message type %q", got, want)
	}
	if err := m.validate(); err != nil {
		return nil, err
	}
	encoded, err := marshalSignalingJSON(m)
	if err != nil {
		return nil, fmt.Errorf("protocol: encode %s: %w", m.messageType(), err)
	}
	if !jsonNestingWithinLimit(encoded, MaxSignalingJSONDepth) {
		return nil, fmt.Errorf("protocol: signaling JSON exceeds nesting depth %d", MaxSignalingJSONDepth)
	}
	if len(encoded) > MaxSignalingMessageBytes {
		return nil, fmt.Errorf("protocol: signaling message is %d bytes, limit %d", len(encoded), MaxSignalingMessageBytes)
	}
	return encoded, nil
}

// JavaScript JSON.stringify leaves HTML punctuation and the two Unicode line
// separators literal. Matching that spelling prevents Go relay forwarding from
// expanding an accepted browser message beyond the shared byte limit.
func marshalSignalingJSON(m Message) ([]byte, error) {
	var output bytes.Buffer
	encoder := json.NewEncoder(&output)
	encoder.SetEscapeHTML(false)
	if err := encoder.Encode(m); err != nil {
		return nil, err
	}
	encoded := bytes.TrimSuffix(output.Bytes(), []byte{'\n'})
	return unescapeJSONLineSeparators(encoded), nil
}

func unescapeJSONLineSeparators(encoded []byte) []byte {
	var normalized []byte
	last := 0
	for index := 0; index+5 < len(encoded); index++ {
		if encoded[index] != '\\' || encoded[index+1] != 'u' ||
			(encoded[index+2] != '2' || encoded[index+3] != '0' || encoded[index+4] != '2' ||
				(encoded[index+5] != '8' && encoded[index+5] != '9')) {
			continue
		}
		precedingSlashes := 0
		for cursor := index - 1; cursor >= 0 && encoded[cursor] == '\\'; cursor-- {
			precedingSlashes++
		}
		if precedingSlashes%2 != 0 {
			continue
		}
		normalized = append(normalized, encoded[last:index]...)
		separator := rune('\u2028')
		if encoded[index+5] == '9' {
			separator = '\u2029'
		}
		normalized = utf8.AppendRune(normalized, separator)
		last = index + 6
		index += 5
	}
	if normalized == nil {
		return encoded
	}
	return append(normalized, encoded[last:]...)
}

// Decode 解析一条信令 JSON 消息为具体类型;未知 type 与字段校验失败都在
// 此处拦下,调用方拿到的消息保证结构完整。
func Decode(data []byte) (Message, error) {
	if len(data) > MaxSignalingMessageBytes {
		return nil, fmt.Errorf("protocol: signaling message is %d bytes, limit %d", len(data), MaxSignalingMessageBytes)
	}
	if !utf8.Valid(data) {
		return nil, errors.New("protocol: invalid signaling JSON: input is not valid UTF-8")
	}
	if !jsonNestingWithinLimit(data, MaxSignalingJSONDepth) {
		return nil, fmt.Errorf("protocol: signaling JSON exceeds nesting depth %d", MaxSignalingJSONDepth)
	}
	var object signalingObject
	if err := json.Unmarshal(data, &object); err != nil {
		return nil, fmt.Errorf("protocol: invalid signaling JSON: %w", err)
	}
	if object == nil {
		return nil, errors.New("protocol: signaling JSON must be an object")
	}
	typeName, err := object.requiredString("type")
	if err != nil {
		return nil, err
	}
	var m Message
	switch typeName {
	case TypeRegister:
		shareID, err := object.requiredString("shareId")
		if err != nil {
			return nil, err
		}
		resumeTokenHash, err := object.requiredString("resumeTokenHash")
		if err != nil {
			return nil, err
		}
		resumeToken, _, err := object.optionalString("resumeToken")
		if err != nil {
			return nil, err
		}
		m = &Register{Type: typeName, ShareID: shareID, ResumeTokenHash: resumeTokenHash, ResumeToken: resumeToken}
	case TypeRegistered:
		shareID, err := object.requiredString("shareId")
		if err != nil {
			return nil, err
		}
		m = &Registered{Type: typeName, ShareID: shareID}
	case TypeKeepalive:
		m = &Keepalive{Type: typeName}
	case TypeJoin:
		shareID, err := object.requiredString("shareId")
		if err != nil {
			return nil, err
		}
		m = &Join{Type: typeName, ShareID: shareID}
	case TypeManifest:
		sessionID, err := object.requiredString("sessionId")
		if err != nil {
			return nil, err
		}
		m = &Manifest{Type: typeName, SessionID: sessionID}
	case TypeNotFound:
		m = &NotFound{Type: typeName}
	case TypeSignal:
		sessionID, err := object.requiredString("sessionId")
		if err != nil {
			return nil, err
		}
		kind, err := object.requiredString("kind")
		if err != nil {
			return nil, err
		}
		payload, ok := object["payload"]
		if !ok {
			return nil, errors.New("protocol: signal is missing payload")
		}
		m = &Signal{
			Type:      typeName,
			SessionID: sessionID,
			Kind:      kind,
			Payload:   append(json.RawMessage(nil), payload...),
		}
	case TypeBye:
		sessionID, err := object.requiredString("sessionId")
		if err != nil {
			return nil, err
		}
		m = &Bye{Type: typeName, SessionID: sessionID}
	case TypeError:
		code, err := object.requiredString("code")
		if err != nil {
			return nil, err
		}
		message, _, err := object.optionalString("message")
		if err != nil {
			return nil, err
		}
		sessionID, _, err := object.optionalString("sessionId")
		if err != nil {
			return nil, err
		}
		m = &Error{Type: typeName, Code: code, Message: message, SessionID: sessionID}
	default:
		return nil, fmt.Errorf("protocol: unknown signaling message type %q", typeName)
	}
	if err := m.validate(); err != nil {
		return nil, err
	}
	return m, nil
}

// jsonNestingWithinLimit is deliberately non-recursive and only owns the
// structural bound. encoding/json remains the syntax authority after hostile
// depth has been rejected cheaply and consistently across runtimes.
func jsonNestingWithinLimit(data []byte, limit int) bool {
	depth := 0
	inString := false
	escaped := false
	for _, character := range data {
		if inString {
			switch {
			case escaped:
				escaped = false
			case character == '\\':
				escaped = true
			case character == '"':
				inString = false
			}
			continue
		}
		switch character {
		case '"':
			inString = true
		case '{', '[':
			depth++
			if depth > limit {
				return false
			}
		case '}', ']':
			depth--
		}
	}
	return true
}

type signalingObject map[string]json.RawMessage

func (o signalingObject) requiredString(field string) (string, error) {
	raw, ok := o[field]
	if !ok {
		return "", fmt.Errorf("protocol: signaling field %s is missing", field)
	}
	raw = bytes.TrimSpace(raw)
	if !validJSONScalarString(raw) {
		return "", fmt.Errorf("protocol: signaling field %s must be Unicode scalar text", field)
	}
	var value string
	if err := json.Unmarshal(raw, &value); err != nil {
		return "", fmt.Errorf("protocol: decode signaling field %s: %w", field, err)
	}
	if value == "" {
		return "", fmt.Errorf("protocol: signaling field %s must be non-empty", field)
	}
	return value, nil
}

func (o signalingObject) optionalString(field string) (string, bool, error) {
	if _, ok := o[field]; !ok {
		return "", false, nil
	}
	value, err := o.requiredString(field)
	return value, true, err
}

func validJSONScalarString(raw []byte) bool {
	if len(raw) < 2 || raw[0] != '"' || raw[len(raw)-1] != '"' || !utf8.Valid(raw) {
		return false
	}
	for i := 1; i < len(raw)-1; {
		if raw[i] != '\\' {
			if raw[i] < 0x20 || raw[i] == '"' {
				return false
			}
			_, size := utf8.DecodeRune(raw[i:])
			i += size
			continue
		}
		if i+1 >= len(raw)-1 {
			return false
		}
		escape := raw[i+1]
		if escape != 'u' {
			if !bytes.ContainsRune([]byte(`"\\/bfnrt`), rune(escape)) {
				return false
			}
			i += 2
			continue
		}
		unit, ok := decodeJSONHexUnit(raw, i+2)
		if !ok {
			return false
		}
		i += 6
		switch {
		case unit >= 0xd800 && unit <= 0xdbff:
			if i+6 > len(raw)-1 || raw[i] != '\\' || raw[i+1] != 'u' {
				return false
			}
			low, ok := decodeJSONHexUnit(raw, i+2)
			if !ok || low < 0xdc00 || low > 0xdfff {
				return false
			}
			i += 6
		case unit >= 0xdc00 && unit <= 0xdfff:
			return false
		}
	}
	return true
}

func decodeJSONHexUnit(raw []byte, start int) (uint16, bool) {
	if start+4 > len(raw)-1 {
		return 0, false
	}
	var value uint16
	for _, digit := range raw[start : start+4] {
		value <<= 4
		switch {
		case digit >= '0' && digit <= '9':
			value |= uint16(digit - '0')
		case digit >= 'a' && digit <= 'f':
			value |= uint16(digit-'a') + 10
		case digit >= 'A' && digit <= 'F':
			value |= uint16(digit-'A') + 10
		default:
			return 0, false
		}
	}
	return value, true
}

// typeTagOf reads the concrete wire field without serializing attacker-sized
// payloads merely to validate a closed-set type discriminator.
func typeTagOf(m Message) string {
	switch message := m.(type) {
	case *Register:
		if message != nil {
			return message.Type
		}
	case *Registered:
		if message != nil {
			return message.Type
		}
	case *Keepalive:
		if message != nil {
			return message.Type
		}
	case *Join:
		if message != nil {
			return message.Type
		}
	case *Manifest:
		if message != nil {
			return message.Type
		}
	case *NotFound:
		if message != nil {
			return message.Type
		}
	case *Signal:
		if message != nil {
			return message.Type
		}
	case *Bye:
		if message != nil {
			return message.Type
		}
	case *Error:
		if message != nil {
			return message.Type
		}
	}
	return ""
}

// MaxShareIDChars 限定 shareId 字符串长度。shareId 对中转是不透明路由句柄
// (D13,长度由客户端版本决定),但它入 URL 路径与注册表键,须限制字符集
// 与长度防注入/滥用;64 字符已容纳数倍于当前 9 字节句柄的演进空间。
const MaxShareIDChars = 64

// ValidateShareID 校验 shareId 为非空、不超长的 base64url 字符串。
// 只做路由句柄的卫生检查,不钉死具体长度——语义家在 core/link(§8)。
func ValidateShareID(s string) error {
	if s == "" {
		return errors.New("protocol: shareId is empty")
	}
	if len(s) > MaxShareIDChars {
		return fmt.Errorf("protocol: shareId is too long (%d > %d)", len(s), MaxShareIDChars)
	}
	for i := 0; i < len(s); i++ {
		c := s[i]
		if !(c >= 'A' && c <= 'Z' || c >= 'a' && c <= 'z' || c >= '0' && c <= '9' || c == '-' || c == '_') {
			return fmt.Errorf("protocol: shareId contains non-base64url character %q", c)
		}
	}
	return nil
}

// EncodeResumeToken 把 resumeToken 原像编码为线格式(base64url 无填充)。
func EncodeResumeToken(token []byte) string {
	return base64.RawURLEncoding.EncodeToString(token)
}

// HashResumeToken 计算 register 提交的 resumeTokenHash 线格式:
// base64url 无填充的 SHA-256(token)(§6.8)。
func HashResumeToken(token []byte) string {
	sum := sha256.Sum256(token)
	return base64.RawURLEncoding.EncodeToString(sum[:])
}

var strictRawURLEncoding = base64.RawURLEncoding.Strict()

// VerifyResumeToken 校验宽限期重注册出示的原像与注册时提交的哈希匹配。
// 常数时间比较:token 是会话所有权凭证,不给旁路留时序信道。
func VerifyResumeToken(tokenB64, wantHashB64 string) bool {
	token, err := decodeB64Fixed(tokenB64, ResumeTokenBytes, "resumeToken")
	if err != nil {
		return false
	}
	want, err := decodeB64Fixed(wantHashB64, sha256.Size, "resumeTokenHash")
	if err != nil {
		return false
	}
	sum := sha256.Sum256(token)
	return subtle.ConstantTimeCompare(sum[:], want) == 1
}

func decodeB64Fixed(s string, wantLen int, field string) ([]byte, error) {
	// Fixed-size protocol fields have one wire spelling. Strict decoding rejects
	// aliases with non-zero trailing pad bits before identity or authentication
	// logic can accidentally treat distinct strings as the same credential.
	b, err := strictRawURLEncoding.DecodeString(s)
	if err != nil {
		return nil, fmt.Errorf("protocol: %s is invalid base64url: %w", field, err)
	}
	if len(b) != wantLen {
		return nil, fmt.Errorf("protocol: %s length is %d, want %d", field, len(b), wantLen)
	}
	return b, nil
}
