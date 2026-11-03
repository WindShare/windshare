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
	"unicode"
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
	Relays     []string // Ordered relay bases; Parse and Merge resolve omitted hints from the link origin.
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
	if relays := explicitRelays(u, l.Relays); len(relays) > 0 {
		u.RawQuery = url.Values{relayParam: relays}.Encode()
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
	l.Suite, l.ReadSecret, l.PKHash, err = decodeKeyPayload(browserFragment(u))
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
	key = browserFragment(u)
	suite, _, pkHash, err := decodeKeyPayload(key)
	if err != nil {
		return "", "", err
	}
	if err := validateIdentityForSuite(Link{Suite: suite, PKHash: pkHash, ShareID: routeSegment(u), ReadSecret: make([]byte, ReadSecretBytes)}); err != nil {
		return "", "", err
	}
	u.Fragment, u.RawFragment = "", ""
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
		s2, sec2, hash2, err := decodeKeyPayload(browserFragment(u))
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
	shareID := routeSegment(u)
	if err := validBareShareID(shareID); err != nil {
		return Link{}, nil, err
	}
	return Link{ShareID: shareID, Relays: resolveRelays(u, query)}, u, nil
}

func parseAbsoluteURL(raw string) (*url.URL, url.Values, error) {
	trimmed := TrimCopiedText(raw)
	// WHATWG parsing repairs backslashes into authority or path separators while
	// net/url preserves some of them. Rejecting the ambiguous spelling gives a
	// capability URL one identity in both runtimes.
	if strings.ContainsRune(trimmed, '\\') {
		return nil, nil, fmt.Errorf("%w: invalid URL syntax", ErrMalformedLink)
	}
	u, err := url.Parse(whatwgPreprocess(trimmed))
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

// routeSegment returns the last non-empty path segment: frontends may be
// deployed below a path prefix, and shareId is always the final segment. Like
// the browser, it resolves dot segments (including %2e spellings) before
// decoding, so an encoded slash stays inside its segment. Scanning from the end
// lets each ".." cancel the nearest earlier segment without building the path.
func routeSegment(u *url.URL) string {
	escaped := u.RawPath
	if escaped == "" {
		escaped = u.EscapedPath()
	}
	removed := 0
	for rest := escaped; rest != ""; {
		i := strings.LastIndexByte(rest, '/')
		segment := rest[i+1:]
		rest = rest[:max(i, 0)]
		switch {
		case isSingleDotSegment(segment):
		case isDoubleDotSegment(segment):
			removed++
		case removed > 0:
			removed--
		case segment != "":
			decoded, err := url.PathUnescape(segment)
			if err != nil {
				return ""
			}
			return decoded
		}
	}
	return ""
}

func isSingleDotSegment(s string) bool {
	return s == "." || strings.EqualFold(s, "%2e")
}

func isDoubleDotSegment(s string) bool {
	return s == ".." || strings.EqualFold(s, ".%2e") || strings.EqualFold(s, "%2e.") || strings.EqualFold(s, "%2e%2e")
}

// browserFragment returns location.hash without its '#': the copied fragment
// bytes with only the WHATWG fragment percent-encode set escaped. net/url's
// Fragment is percent-decoded and would accept keys the browser rejects.
//
// This relies on url.Parse keeping the original fragment spelling: it sets
// RawFragment to the text after the first '#' whenever that text differs from
// EscapedFragment's default escaping of Fragment, and leaves it empty only when
// the two are equal. Either way raw is the copied text verbatim. Calling
// EscapedFragment on a set RawFragment could substitute Go's escaping, so it is
// read directly. url.Parse rejects invalid %-escapes and control bytes in the
// fragment; the browser keeps them, but neither forms a valid key.
func browserFragment(u *url.URL) string {
	raw := u.RawFragment
	if raw == "" {
		raw = u.EscapedFragment()
	}
	var encoded strings.Builder
	for i := range len(raw) {
		switch c := raw[i]; {
		case c <= ' ' || c >= 0x7f || c == '"' || c == '<' || c == '>' || c == '`':
			fmt.Fprintf(&encoded, "%%%02X", c)
		default:
			encoded.WriteByte(c)
		}
	}
	return encoded.String()
}

// TrimCopiedText applies JavaScript's String.prototype.trim, which the browser
// receiver uses before parsing links and keys. Go's TrimSpace differs at U+0085
// and U+FEFF, so clients reading pasted capability text should use this rule.
func TrimCopiedText(s string) string {
	return strings.TrimFunc(s, func(r rune) bool {
		switch r {
		case '\t', '\n', '\v', '\f', '\r', '\u2028', '\u2029', '\ufeff':
			return true
		}
		return unicode.Is(unicode.Zs, r)
	})
}

// whatwgPreprocess mirrors the WHATWG URL parser's input cleanup: browsers drop
// C0 controls and spaces at the boundaries and every ASCII tab or newline, so a
// line-wrapped copy still names the same capability.
func whatwgPreprocess(s string) string {
	s = strings.TrimFunc(s, func(r rune) bool { return r <= ' ' })
	return strings.Map(func(r rune) rune {
		if r == '\t' || r == '\n' || r == '\r' {
			return -1
		}
		return r
	}, s)
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

// decodeKey resolves separately supplied key input as the browser's key box
// does: text containing "://" is a complete capability link and follows Parse,
// so its fragment is read verbatim and its route must match its key. Any other
// text is a key payload.
func decodeKey(s string) (suite byte, secret, pkHash []byte, err error) {
	s = TrimCopiedText(s)
	if strings.Contains(s, "://") {
		capability, parseErr := Parse(s)
		if errors.Is(parseErr, ErrMissingFragment) {
			// A link without a key is not a key; asking for one again would loop.
			return 0, nil, nil, fmt.Errorf("%w: key link has no fragment", ErrMalformedFragment)
		}
		if parseErr != nil {
			return 0, nil, nil, parseErr
		}
		return capability.Suite, capability.ReadSecret, capability.PKHash, nil
	}
	return decodeKeyPayload(s)
}

// decodeKeyPayload accepts a key string, optionally after text and a '#' as in
// the browser's keyPayload, then applies strict suite-specific byte widths.
func decodeKeyPayload(s string) (suite byte, secret, pkHash []byte, err error) {
	s = TrimCopiedText(s)
	if i := strings.IndexByte(s, '#'); i >= 0 {
		s = TrimCopiedText(s[i+1:])
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
