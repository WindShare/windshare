package link

import (
	"bytes"
	"crypto/ed25519"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"net/url"
	"strings"
	"unicode/utf8"
)

const (
	// SuiteSenderAuthenticated is the native capability and sender-object suite.
	SuiteSenderAuthenticated byte = 0x02

	// ReadSecretBytes is the 128-bit capability secret width.
	ReadSecretBytes = 16
	// SenderAuthenticatedShareIDBytes is the relay route width.
	SenderAuthenticatedShareIDBytes = 12
	// PKHashBytes is the truncated sender Ed25519 public-key hash width.
	PKHashBytes = 16
)

const (
	senderKeyDomain = "windshare/v2 sender-key\x00"
	shareIDDomain   = "windshare/v2 share-id\x00"
)

// relayParam 是中转提示的查询参数名(?r=,非秘密;自始按多值列表解析,§6.3)。
const relayParam = "r"

var strictRawURLEncoding = base64.RawURLEncoding.Strict()

var (
	// ErrMissingFragment 供接收侧区分「坏链接」与「分离密钥分享」:
	// fragment 为空 → 转入密钥输入流程(网页输入框 / CLI --key,§6.10)。
	ErrMissingFragment = errors.New("link: link does not contain a key fragment")

	// Unknown suites must fail before their key material can be interpreted.
	ErrUnknownSuite = errors.New("link: unsupported link suite")

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
	PKHash     []byte   // suite 0x02 only: first16(SHA-256(sender-key-domain || sender public key))
	ShareID    string   // suite-specific base64url relay route; never an encryption input
	Relays     []string // ?r= 多值,自始按列表解析(M1 仅用首个)
}

// SenderKeyHash binds a suite-0x02 capability to its Ed25519 sender identity.
func SenderKeyHash(publicKey ed25519.PublicKey) ([PKHashBytes]byte, error) {
	var result [PKHashBytes]byte
	if len(publicKey) != ed25519.PublicKeySize {
		return result, fmt.Errorf("%w: sender public key must be %d bytes", ErrMalformedFragment, ed25519.PublicKeySize)
	}
	digest := sha256.Sum256(append([]byte(senderKeyDomain), publicKey...))
	copy(result[:], digest[:PKHashBytes])
	return result, nil
}

// ShareIDForSenderKeyHash deterministically derives the v2 relay route from the public sender identity.
func ShareIDForSenderKeyHash(pkHash []byte) (string, error) {
	if len(pkHash) != PKHashBytes {
		return "", fmt.Errorf("%w: pkHash must be %d bytes", ErrMalformedFragment, PKHashBytes)
	}
	digest := sha256.Sum256(append([]byte(shareIDDomain), pkHash...))
	return base64.RawURLEncoding.EncodeToString(digest[:SenderAuthenticatedShareIDBytes]), nil
}

// NewSenderAuthenticated constructs the native suite-0x02 capability identity.
func NewSenderAuthenticated(readSecret []byte, publicKey ed25519.PublicKey, relays []string) (Link, error) {
	if len(readSecret) != ReadSecretBytes {
		return Link{}, fmt.Errorf("%w: readSecret must be %d bytes", ErrMalformedFragment, ReadSecretBytes)
	}
	pkHash, err := SenderKeyHash(publicKey)
	if err != nil {
		return Link{}, err
	}
	shareID, err := ShareIDForSenderKeyHash(pkHash[:])
	if err != nil {
		return Link{}, err
	}
	return Link{
		Suite: SuiteSenderAuthenticated, ReadSecret: bytes.Clone(readSecret), PKHash: bytes.Clone(pkHash[:]),
		ShareID: shareID, Relays: append([]string(nil), relays...),
	}, nil
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

// URL constructs the capability URL with the suite-specific fragment payload.
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

// KeyString encodes suite||readSecret||pkHash with strict unpadded base64url.
func (l Link) KeyString() (string, error) {
	if l.Suite != SuiteSenderAuthenticated {
		return "", fmt.Errorf("%w(suite 0x%02x)", ErrUnknownSuite, l.Suite)
	}
	if len(l.ReadSecret) != ReadSecretBytes {
		return "", fmt.Errorf("%w: readSecret must be %d bytes, got %d", ErrMalformedFragment, ReadSecretBytes, len(l.ReadSecret))
	}
	if len(l.PKHash) != PKHashBytes {
		return "", fmt.Errorf("%w: pkHash must be %d bytes, got %d", ErrMalformedFragment, PKHashBytes, len(l.PKHash))
	}
	buf := make([]byte, 0, 1+ReadSecretBytes+PKHashBytes)
	buf = append(buf, l.Suite)
	buf = append(buf, l.ReadSecret...)
	buf = append(buf, l.PKHash...)
	return base64.RawURLEncoding.EncodeToString(buf), nil
}

func (l Link) bareURL(base string) (*url.URL, error) {
	if err := validateIdentityForSuite(l); err != nil {
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
	l.Suite, l.ReadSecret, l.PKHash, err = decodeKey(u.Fragment)
	if err != nil {
		return Link{}, err
	}
	if err := validateIdentityForSuite(l); err != nil {
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
	suite, _, pkHash, err := decodeKey(u.Fragment)
	if err != nil {
		return "", "", err
	}
	if err := validateIdentityForSuite(Link{Suite: suite, PKHash: pkHash, ShareID: lastSegment(u.Path), ReadSecret: make([]byte, ReadSecretBytes)}); err != nil {
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
	suite, secret, pkHash, err := decodeKey(key)
	if err != nil {
		return Link{}, err
	}
	if u.Fragment != "" {
		s2, sec2, hash2, err := decodeKey(u.Fragment)
		if err != nil {
			return Link{}, err
		}
		if s2 != suite || !bytes.Equal(sec2, secret) || !bytes.Equal(hash2, pkHash) {
			return Link{}, ErrKeyConflict
		}
	}
	l.Suite, l.ReadSecret, l.PKHash = suite, secret, pkHash
	if err := validateIdentityForSuite(l); err != nil {
		return Link{}, err
	}
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
	if err := validBareShareID(shareID); err != nil {
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

func validBareShareID(s string) error {
	raw, err := strictRawURLEncoding.DecodeString(s)
	if err != nil || len(raw) != SenderAuthenticatedShareIDBytes {
		return fmt.Errorf("%w:%q", ErrMalformedShareID, s)
	}
	return nil
}

func validateIdentityForSuite(l Link) error {
	raw, err := strictRawURLEncoding.DecodeString(l.ShareID)
	if err != nil {
		return fmt.Errorf("%w:%q", ErrMalformedShareID, l.ShareID)
	}
	if l.Suite != SuiteSenderAuthenticated {
		return fmt.Errorf("%w(suite 0x%02x)", ErrUnknownSuite, l.Suite)
	}
	if len(raw) != SenderAuthenticatedShareIDBytes || len(l.PKHash) != PKHashBytes {
		return fmt.Errorf("%w:%q", ErrMalformedShareID, l.ShareID)
	}
	expected, err := ShareIDForSenderKeyHash(l.PKHash)
	if err != nil || !constantTimeStringEqual(expected, l.ShareID) {
		return fmt.Errorf("%w:%q", ErrMalformedShareID, l.ShareID)
	}
	return nil
}

func constantTimeStringEqual(left, right string) bool {
	return len(left) == len(right) && subtle.ConstantTimeCompare([]byte(left), []byte(right)) == 1
}

// decodeKey accepts a key string, a leading '#', or a complete copied URL and
// then applies strict suite-specific byte widths.
func decodeKey(s string) (suite byte, secret, pkHash []byte, err error) {
	s = strings.TrimSpace(s)
	if i := strings.IndexByte(s, '#'); i >= 0 {
		s = strings.TrimSpace(s[i+1:])
	}
	if s == "" {
		return 0, nil, nil, fmt.Errorf("%w: key string is empty", ErrMalformedFragment)
	}
	raw, err := strictRawURLEncoding.DecodeString(s)
	if err != nil {
		return 0, nil, nil, fmt.Errorf("%w:%w", ErrMalformedFragment, err)
	}
	if len(raw) == 0 {
		return 0, nil, nil, fmt.Errorf("%w: key string decodes to zero bytes", ErrMalformedFragment)
	}
	if raw[0] != SuiteSenderAuthenticated {
		return 0, nil, nil, fmt.Errorf("%w(suite 0x%02x)", ErrUnknownSuite, raw[0])
	}
	const encodedBytes = 1 + ReadSecretBytes + PKHashBytes
	if len(raw) != encodedBytes {
		return 0, nil, nil, fmt.Errorf(
			"%w: got %d bytes, suite 0x%02x requires %d",
			ErrMalformedFragment,
			len(raw),
			raw[0],
			encodedBytes,
		)
	}
	return raw[0], bytes.Clone(raw[1 : 1+ReadSecretBytes]), bytes.Clone(raw[1+ReadSecretBytes:]), nil
}
