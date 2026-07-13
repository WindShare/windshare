package link

import (
	"bytes"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"net/url"
	"strings"
	"unicode/utf8"
)

// suite 常量的语义家在本包:suiteByte 是链接 fragment 的首字节,兼作 AEAD 的
// AAD 域分隔(§6.3);chunk/manifest 从这里取值,保证全仓库同一来源。
const (
	// SuiteAESGCM = suite 0x01:{AES-256-GCM, HKDF-SHA256, 128-bit readSecret, 随机 nonce}。
	SuiteAESGCM byte = 0x01

	// ReadSecretBytes:16B/128-bit,链接中唯一秘密;128-bit 随机密钥免疫离线爆破(§2.2)。
	ReadSecretBytes = 16

	// ShareIDBytes:9B/72-bit 路由句柄,抗枚举且 base64url 恰 12 字符、无填充。
	ShareIDBytes = 9
)

// relayParam 是中转提示的查询参数名(?r=,非秘密;自始按多值列表解析,§6.3)。
const relayParam = "r"

var strictRawURLEncoding = base64.RawURLEncoding.Strict()

var (
	// ErrMissingFragment 供接收侧区分「坏链接」与「分离密钥分享」:
	// fragment 为空 → 转入密钥输入流程(网页输入框 / CLI --key,§6.10)。
	ErrMissingFragment = errors.New("link: link does not contain a key fragment")

	// ErrUnknownSuite:fragment 布局随 suite 而变,未知 suite 按 0x01 硬解只会
	// 得到错误密钥,必须明确报错引导升级(与 B15 清单版本同一策略)。
	ErrUnknownSuite = errors.New("link: unsupported link suite; upgrade required")

	ErrMalformedLink     = errors.New("link: malformed link")
	ErrMalformedShareID  = errors.New("link: malformed shareId")
	ErrMalformedFragment = errors.New("link: malformed key string")

	// ErrKeyConflict:裸链接自带密钥且与另行提供的密钥串不一致时,静默偏袒
	// 任何一方都可能让用户拿错密钥解错分享,必须报错让用户裁决。
	ErrKeyConflict = errors.New("link: embedded key and separately supplied key do not match")
)

// Link 是能力链接的语义结构(§6.3)。
type Link struct {
	Suite      byte     // 套件/版本位,fragment 首字节
	ReadSecret []byte   // 16B;链接中唯一秘密
	ShareID    string   // 9B 随机的 base64url;纯路由句柄,不入密钥
	Relays     []string // ?r= 多值,自始按列表解析(M1 仅用首个)
}

// NewReadSecret 从 rng 取 ReadSecretBytes 随机字节。随机源注入而非内取
// crypto/rand:金标向量与测试需要确定性(§7);生产传 crypto/rand.Reader。
func NewReadSecret(rng io.Reader) ([]byte, error) {
	buf := make([]byte, ReadSecretBytes)
	if _, err := io.ReadFull(rng, buf); err != nil {
		return nil, fmt.Errorf("link: generate readSecret: %w", err)
	}
	return buf, nil
}

// NewShareID 生成 ShareIDBytes 随机字节的 base64url 路由句柄(不入密钥,§6.3)。
func NewShareID(rng io.Reader) (string, error) {
	buf := make([]byte, ShareIDBytes)
	if _, err := io.ReadFull(rng, buf); err != nil {
		return "", fmt.Errorf("link: generate shareId: %w", err)
	}
	return base64.RawURLEncoding.EncodeToString(buf), nil
}

// URL 构造完整能力链接:<base>/<shareId>?r=<relay>#base64url(suiteByte‖readSecret)。
// base 为前端基址(前端域是部署期配置,§6.3);fragment 由浏览器语义保证不发往服务器。
func (l Link) URL(base string) (string, error) {
	u, err := l.bareURL(base)
	if err != nil {
		return "", err
	}
	frag, err := l.KeyString()
	if err != nil {
		return "", err
	}
	u.Fragment = frag
	return u.String(), nil
}

// SplitURL 输出「裸链接 + 密钥串」两段(分离密钥,§6.10):由工具代剪,
// 杜绝用户手工截断出错;两段经不同渠道分发,密钥不落单渠道。
func (l Link) SplitURL(base string) (bare, key string, err error) {
	u, err := l.bareURL(base)
	if err != nil {
		return "", "", err
	}
	key, err = l.KeyString()
	if err != nil {
		return "", "", err
	}
	return u.String(), key, nil
}

// KeyString 编码能力令牌 base64url(suiteByte‖readSecret),无填充。
func (l Link) KeyString() (string, error) {
	n, err := secretLen(l.Suite)
	if err != nil {
		return "", err
	}
	if len(l.ReadSecret) != n {
		return "", fmt.Errorf("%w: readSecret must be %d bytes, got %d", ErrMalformedFragment, n, len(l.ReadSecret))
	}
	buf := make([]byte, 0, 1+n)
	buf = append(buf, l.Suite)
	buf = append(buf, l.ReadSecret...)
	return base64.RawURLEncoding.EncodeToString(buf), nil
}

func (l Link) bareURL(base string) (*url.URL, error) {
	if err := validShareID(l.ShareID); err != nil {
		return nil, err
	}
	u, _, err := parseAbsoluteURL(base)
	if err != nil {
		return nil, err
	}
	u = u.JoinPath(l.ShareID)
	// 查询与 fragment 由链接语义独占,base 携带的一律丢弃。
	u.RawQuery = ""
	if len(l.Relays) > 0 {
		u.RawQuery = url.Values{relayParam: l.Relays}.Encode()
	}
	u.Fragment = ""
	return u, nil
}

// Parse 解析完整能力链接。fragment 缺失返回 ErrMissingFragment,接收侧据此
// 转入分离密钥输入流程(Merge)。
func Parse(raw string) (Link, error) {
	l, u, err := parseBare(raw)
	if err != nil {
		return Link{}, err
	}
	if u.Fragment == "" {
		return Link{}, ErrMissingFragment
	}
	l.Suite, l.ReadSecret, err = decodeKey(u.Fragment)
	if err != nil {
		return Link{}, err
	}
	return l, nil
}

// Split 把完整链接拆为「裸链接 + 密钥串」(发送侧 --split-key 的字符串形态;
// 与 Merge 构成 §6.3 的拆合纯函数对)。拆前校验密钥串可解,不放走坏链接。
func Split(full string) (bare, key string, err error) {
	_, u, err := parseBare(full)
	if err != nil {
		return "", "", err
	}
	if u.Fragment == "" {
		return "", "", ErrMissingFragment
	}
	if _, _, err := decodeKey(u.Fragment); err != nil {
		return "", "", err
	}
	key = u.Fragment
	u.Fragment = ""
	return u.String(), key, nil
}

// Merge 把裸链接与另行送达的密钥串合回 Link(接收侧;key 宽容解析,见 decodeKey)。
// 裸链接若自带 fragment(用户把完整链接同时粘到两处):一致则接受,不一致报
// ErrKeyConflict。
func Merge(bare, key string) (Link, error) {
	l, u, err := parseBare(bare)
	if err != nil {
		return Link{}, err
	}
	suite, secret, err := decodeKey(key)
	if err != nil {
		return Link{}, err
	}
	if u.Fragment != "" {
		s2, sec2, err := decodeKey(u.Fragment)
		if err != nil {
			return Link{}, err
		}
		if s2 != suite || !bytes.Equal(sec2, secret) {
			return Link{}, ErrKeyConflict
		}
	}
	l.Suite, l.ReadSecret = suite, secret
	return l, nil
}

// parseBare 解析链接的非密钥部分,fragment 留在返回的 *url.URL 上由调用方裁决
// (Parse 要求有、Split 拆走、Merge 用作一致性核对)。
func parseBare(raw string) (Link, *url.URL, error) {
	u, query, err := parseAbsoluteURL(raw)
	if err != nil {
		return Link{}, nil, err
	}
	shareID := lastSegment(u.Path)
	if err := validShareID(shareID); err != nil {
		return Link{}, nil, err
	}
	return Link{ShareID: shareID, Relays: query[relayParam]}, u, nil
}

func parseAbsoluteURL(raw string) (*url.URL, url.Values, error) {
	trimmed := strings.TrimSpace(raw)
	// WHATWG parsing repairs backslashes into authority or path separators while
	// net/url preserves some of them. Rejecting the ambiguous spelling gives a
	// capability URL one identity in both runtimes.
	if strings.ContainsRune(trimmed, '\\') {
		return nil, nil, fmt.Errorf("%w: invalid URL syntax", ErrMalformedLink)
	}
	u, err := url.Parse(trimmed)
	if err != nil || u.Scheme == "" || u.Host == "" {
		// net/url parse errors retain and render the complete input URL. Returning
		// that detail would copy a capability fragment into CLI diagnostics.
		return nil, nil, fmt.Errorf("%w: invalid URL syntax or missing scheme/host", ErrMalformedLink)
	}
	// URL.Query silently discards malformed pairs. Capability parsing must fail
	// closed instead, or Go and the browser can derive different relay lists from
	// the same copied link. Escaped semicolons (%3B) remain valid relay content.
	query, err := url.ParseQuery(u.RawQuery)
	if err != nil {
		return nil, nil, fmt.Errorf("%w: invalid URL query", ErrMalformedLink)
	}
	for key, values := range query {
		if !utf8.ValidString(key) {
			return nil, nil, fmt.Errorf("%w: URL query is not valid UTF-8", ErrMalformedLink)
		}
		for _, value := range values {
			if !utf8.ValidString(value) {
				return nil, nil, fmt.Errorf("%w: URL query is not valid UTF-8", ErrMalformedLink)
			}
		}
	}
	return u, query, nil
}

// lastSegment 取路径最后一个非空段:前端可部署在子路径下(前端域与路径为
// 部署期配置),shareId 恒为末段。
func lastSegment(p string) string {
	p = strings.Trim(p, "/")
	if i := strings.LastIndexByte(p, '/'); i >= 0 {
		return p[i+1:]
	}
	return p
}

// validShareID 只保证与链接/中转编码互操作(base64url 恰解出 9B);shareId
// 是纯路由句柄,此校验不承担安全职责。
func validShareID(s string) error {
	raw, err := strictRawURLEncoding.DecodeString(s)
	if err != nil || len(raw) != ShareIDBytes {
		return fmt.Errorf("%w:%q", ErrMalformedShareID, s)
	}
	return nil
}

// decodeKey 宽容解析密钥串(§6.10 接收侧):接受纯 base64url 串、带 # 前缀的
// 串、或误粘的完整链接(取其 fragment)——手剪单链与 --split-key 产物等价,
// 接收侧不区分来源。首尾空白一律容忍(聊天渠道复制常见)。
func decodeKey(s string) (suite byte, secret []byte, err error) {
	s = strings.TrimSpace(s)
	if i := strings.IndexByte(s, '#'); i >= 0 {
		s = strings.TrimSpace(s[i+1:])
	}
	if s == "" {
		return 0, nil, fmt.Errorf("%w: key string is empty", ErrMalformedFragment)
	}
	raw, err := strictRawURLEncoding.DecodeString(s)
	if err != nil {
		return 0, nil, fmt.Errorf("%w:%w", ErrMalformedFragment, err)
	}
	n, err := secretLen(raw[0])
	if err != nil {
		return 0, nil, err
	}
	if len(raw) != 1+n {
		return 0, nil, fmt.Errorf("%w: got %d bytes, suite 0x%02x requires %d", ErrMalformedFragment, len(raw), raw[0], 1+n)
	}
	return raw[0], raw[1:], nil
}

// secretLen 给出 fragment 中 suite 字节之后的密钥长度,按 suite 分派:
// 0x01 = 16B readSecret;0x02(M2)将为 readSecret‖pkHash 共 32B(§6.14)。
// 长度不跨 suite 硬编码,未知 suite 明确引导升级而非硬解出错误密钥。
func secretLen(suite byte) (int, error) {
	switch suite {
	case SuiteAESGCM:
		return ReadSecretBytes, nil
	default:
		return 0, fmt.Errorf("%w(suite 0x%02x)", ErrUnknownSuite, suite)
	}
}
