package link_test

import (
	"bytes"
	"crypto/ed25519"
	"encoding/base64"
	"errors"
	"io"
	"reflect"
	"strings"
	"testing"

	"github.com/windshare/windshare/core/link"
)

const retiredShareIDBytes = 9

var (
	testSecret    = []byte{0, 1, 2, 3, 4, 5, 6, 7, 8, 9, 10, 11, 12, 13, 14, 15}
	testPublicKey = ed25519.NewKeyFromSeed(
		bytes.Repeat([]byte{0x42}, ed25519.SeedSize),
	).Public().(ed25519.PublicKey)
	testCapability = newLink()
	testShareID    = testCapability.ShareID
	testKey        = mustKeyString(testCapability)
)

func mustKeyString(capability link.Link) string {
	key, err := capability.KeyString()
	if err != nil {
		panic(err)
	}
	return key
}

func newLink(relays ...string) link.Link {
	capability, err := link.NewSenderAuthenticated(testSecret, testPublicKey, relays)
	if err != nil {
		panic(err)
	}
	return capability
}

func TestProtocolConstants(t *testing.T) {
	if link.SuiteSenderAuthenticated != 0x02 ||
		link.SenderAuthenticatedShareIDBytes != 12 ||
		link.PKHashBytes != 16 ||
		link.ReadSecretBytes != 16 {
		t.Fatal("native capability identity constants changed")
	}
}

func TestURLRoundTrip(t *testing.T) {
	tests := []struct {
		name string
		l    link.Link
		base string
	}{
		{"无中转", newLink(), "https://windshare.top"},
		{"单中转", newLink("relay.example.com"), "https://windshare.top"},
		{"多中转保序", newLink("relay-a.example.com", "relay-b.example.com"), "https://windshare.top"},
		{"中转带端口", newLink("relay.example.com:8443"), "https://windshare.top"},
		{"基址尾斜杠", newLink(), "https://windshare.top/"},
		{"基址带子路径", newLink("r.example.com"), "https://ex.com/app"},
		{"localhost 开发基址", newLink(), "http://localhost:5173"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			full, err := tt.l.URL(tt.base)
			if err != nil {
				t.Fatalf("URL: %v", err)
			}
			got, err := link.Parse(full)
			if err != nil {
				t.Fatalf("Parse(%q): %v", full, err)
			}
			if !reflect.DeepEqual(got, tt.l) {
				t.Errorf("round-trip mismatch:\n got  %+v\n want %+v", got, tt.l)
			}
		})
	}
}

func TestFragmentEncoding(t *testing.T) {
	full, err := newLink().URL("https://windshare.top")
	if err != nil {
		t.Fatal(err)
	}
	_, frag, ok := strings.Cut(full, "#")
	if !ok {
		t.Fatalf("link is missing fragment: %q", full)
	}
	if len(frag) != 44 {
		t.Errorf("fragment length %d, want 44: %q", len(frag), frag)
	}
	if strings.Contains(frag, "=") {
		t.Errorf("fragment contains padding: %q", frag)
	}
}

func TestParseErrors(t *testing.T) {
	base := "https://ex.com/" + testShareID
	tests := []struct {
		name    string
		raw     string
		wantErr error
	}{
		{"无 fragment", base, link.ErrMissingFragment},
		{"空 fragment", base + "#", link.ErrMissingFragment},
		{"fragment 非 base64url", base + "#!!!", link.ErrMalformedFragment},
		{"fragment 带填充", base + "#" + testKey + "=", link.ErrMalformedFragment},
		{
			"fragment short by one byte",
			base + "#" + rawB64(append(append([]byte{0x02}, testSecret...), testCapability.PKHash[:15]...)),
			link.ErrMalformedFragment,
		},
		{
			"fragment long by one byte",
			base + "#" + rawB64(append(append(append([]byte{0x02}, testSecret...), testCapability.PKHash...), 0xaa)),
			link.ErrMalformedFragment,
		},
		{"retired suite 0x01", base + "#" + rawB64(append([]byte{0x01}, testSecret...)), link.ErrUnknownSuite},
		{"unknown suite 0x00", base + "#" + rawB64(append([]byte{0x00}, testSecret...)), link.ErrUnknownSuite},
		{
			"retired shareId width",
			"https://ex.com/" + rawB64(bytes.Repeat([]byte{1}, retiredShareIDBytes)) + "#" + testKey,
			link.ErrMalformedShareID,
		},
		{"shareId 错长", "https://ex.com/short#" + testKey, link.ErrMalformedShareID},
		{"shareId 非 base64url", "https://ex.com/!!!!!!!!!!!!#" + testKey, link.ErrMalformedShareID},
		{"shareId 缺失", "https://ex.com/#" + testKey, link.ErrMalformedShareID},
		{"无主机", "/" + testShareID + "#" + testKey, link.ErrMalformedLink},
		{"无 scheme", "ex.com/" + testShareID + "#" + testKey, link.ErrMalformedLink},
		{"URL 不可解析", "https://ex.com/%zz#" + testKey, link.ErrMalformedLink},
		{"查询百分号编码无效", base + "?r=%zz#" + testKey, link.ErrMalformedLink},
		{"查询不是有效 UTF-8", base + "?r=%FF#" + testKey, link.ErrMalformedLink},
		{"查询裸分号有歧义", base + "?r=a;b#" + testKey, link.ErrMalformedLink},
		{"前缀路径含反斜杠", "https://ex.com/prefix\\ignored/" + testShareID + "#" + testKey, link.ErrMalformedLink},
		{"空串", "", link.ErrMalformedLink},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := link.Parse(tt.raw)
			if !errors.Is(err, tt.wantErr) {
				t.Errorf("Parse(%q) err = %v, want %v", tt.raw, err, tt.wantErr)
			}
		})
	}
}

func TestParseQueryEdgeParity(t *testing.T) {
	got, err := link.Parse("https://ex.com/" + testShareID + "?r=a%3Bb&r=c#" + testKey)
	if err != nil {
		t.Fatal(err)
	}
	if want := []string{"a;b", "c"}; !reflect.DeepEqual(got.Relays, want) {
		t.Fatalf("Relays = %q, want %q", got.Relays, want)
	}
}

func TestMalformedLinkErrorsDoNotExposeCapabilityFragment(t *testing.T) {
	for _, raw := range []string{
		"ex.com/" + testShareID + "#" + testKey,
		"https://ex.com/%zz#" + testKey,
	} {
		_, err := link.Parse(raw)
		if !errors.Is(err, link.ErrMalformedLink) {
			t.Fatalf("Parse(%q) err = %v, want ErrMalformedLink", raw, err)
		}
		if strings.Contains(err.Error(), testKey) {
			t.Fatalf("malformed-link diagnostic exposed capability fragment: %v", err)
		}
	}
}

func rawB64(b []byte) string { return base64.RawURLEncoding.EncodeToString(b) }

func TestSplitMergeRoundTrip(t *testing.T) {
	l := newLink("relay.example.com")
	full, err := l.URL("https://windshare.top")
	if err != nil {
		t.Fatal(err)
	}
	bare, key, err := link.Split(full)
	if err != nil {
		t.Fatalf("Split: %v", err)
	}
	if strings.Contains(bare, "#") {
		t.Errorf("bare link still contains fragment: %q", bare)
	}
	if key != testKey {
		t.Errorf("key = %q, want %q", key, testKey)
	}
	got, err := link.Merge(bare, key)
	if err != nil {
		t.Fatalf("Merge: %v", err)
	}
	if !reflect.DeepEqual(got, l) {
		t.Errorf("Split-to-Merge round-trip mismatch:\n got  %+v\n want %+v", got, l)
	}
}

// SplitURL(构造侧)与 Split(字符串侧)必须产出同一对「裸链接 + 密钥串」。
func TestSplitURLAgreesWithSplit(t *testing.T) {
	l := newLink("relay.example.com")
	full, err := l.URL("https://windshare.top")
	if err != nil {
		t.Fatal(err)
	}
	bare1, key1, err := l.SplitURL("https://windshare.top")
	if err != nil {
		t.Fatal(err)
	}
	bare2, key2, err := link.Split(full)
	if err != nil {
		t.Fatal(err)
	}
	if bare1 != bare2 || key1 != key2 {
		t.Errorf("SplitURL(%q, %q) ≠ Split(%q, %q)", bare1, key1, bare2, key2)
	}
}

// 分离密钥接收侧的宽容矩阵(§6.10):纯密钥串 / #前缀 / 误粘完整链接 / 空白。
func TestMergeTolerantKeyForms(t *testing.T) {
	bare := "https://windshare.top/" + testShareID + "?r=relay.example.com"
	full := bare + "#" + testKey
	want := newLink("relay.example.com")
	tests := []struct {
		name string
		key  string
	}{
		{"纯密钥串", testKey},
		{"带 # 前缀", "#" + testKey},
		{"误粘完整链接", full},
		{"首尾空白", "  " + testKey + " \n"},
		{"# 前缀加空白", "\t #" + testKey + " "},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := link.Merge(bare, tt.key)
			if err != nil {
				t.Fatalf("Merge(key=%q): %v", tt.key, err)
			}
			if !reflect.DeepEqual(got, want) {
				t.Errorf("got %+v, want %+v", got, want)
			}
		})
	}
}

func TestMergeErrors(t *testing.T) {
	bare := "https://windshare.top/" + testShareID
	otherCapability := newLink()
	otherCapability.ReadSecret = bytes.Repeat([]byte{0xff}, link.ReadSecretBytes)
	otherKey, err := otherCapability.KeyString()
	if err != nil {
		t.Fatal(err)
	}
	tests := []struct {
		name    string
		bare    string
		key     string
		wantErr error
	}{
		{"空密钥串", bare, "", link.ErrMalformedFragment},
		{"仅 # 号", bare, "###", link.ErrMalformedFragment},
		{"非 base64url", bare, "not base64!", link.ErrMalformedFragment},
		{"未知 suite", bare, rawB64(append([]byte{0x7F}, testSecret...)), link.ErrUnknownSuite},
		{"wrong key width", bare, rawB64([]byte{0x02, 0xab}), link.ErrMalformedFragment},
		{"坏裸链接", "https://ex.com/bad", testKey, link.ErrMalformedShareID},
		{"自带密钥冲突", bare + "#" + otherKey, testKey, link.ErrKeyConflict},
		{"自带 fragment 不可解", bare + "#!!!", testKey, link.ErrMalformedFragment},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := link.Merge(tt.bare, tt.key)
			if !errors.Is(err, tt.wantErr) {
				t.Errorf("Merge err = %v, want %v", err, tt.wantErr)
			}
		})
	}
	// 完整链接粘到两处且一致 → 宽容接受。
	t.Run("自带密钥一致", func(t *testing.T) {
		got, err := link.Merge(bare+"#"+testKey, testKey)
		if err != nil {
			t.Fatalf("Merge: %v", err)
		}
		if !bytes.Equal(got.ReadSecret, testSecret) {
			t.Errorf("ReadSecret = %x", got.ReadSecret)
		}
	})
}

func TestSplitErrors(t *testing.T) {
	tests := []struct {
		name    string
		full    string
		wantErr error
	}{
		{"无 fragment", "https://ex.com/" + testShareID, link.ErrMissingFragment},
		{"坏 fragment", "https://ex.com/" + testShareID + "#!!!", link.ErrMalformedFragment},
		{"坏链接", "not-a-url", link.ErrMalformedLink},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, _, err := link.Split(tt.full)
			if !errors.Is(err, tt.wantErr) {
				t.Errorf("Split err = %v, want %v", err, tt.wantErr)
			}
		})
	}
}

func TestEncodeErrors(t *testing.T) {
	unknownSuite := newLink()
	unknownSuite.Suite = 0x03
	shortSecret := newLink()
	shortSecret.ReadSecret = testSecret[:8]
	invalidShareID := newLink()
	invalidShareID.ShareID = "xx"
	tests := []struct {
		name    string
		l       link.Link
		base    string
		wantErr error
	}{
		{"unknown suite", unknownSuite, "https://ex.com", link.ErrUnknownSuite},
		{"wrong readSecret width", shortSecret, "https://ex.com", link.ErrMalformedFragment},
		{"invalid shareId", invalidShareID, "https://ex.com", link.ErrMalformedShareID},
		{"基址无 scheme", newLink(), "windshare.top", link.ErrMalformedLink},
		{"基址为空", newLink(), "", link.ErrMalformedLink},
		{"基址查询编码无效", newLink(), "https://ex.com/?r=%zz", link.ErrMalformedLink},
		{"基址查询不是有效 UTF-8", newLink(), "https://ex.com/?r=%FF", link.ErrMalformedLink},
		{"基址路径含反斜杠", newLink(), "https://ex.com/prefix\\ignored", link.ErrMalformedLink},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if _, err := tt.l.URL(tt.base); !errors.Is(err, tt.wantErr) {
				t.Errorf("URL err = %v, want %v", err, tt.wantErr)
			}
			if _, _, err := tt.l.SplitURL(tt.base); !errors.Is(err, tt.wantErr) {
				t.Errorf("SplitURL err = %v, want %v", err, tt.wantErr)
			}
		})
	}
}

func TestSenderAuthenticatedLinkMatchesFrozenIdentityVector(t *testing.T) {
	seed := make([]byte, ed25519.SeedSize)
	for index := range seed {
		seed[index] = 0x20 + byte(index)
	}
	publicKey := ed25519.NewKeyFromSeed(seed).Public().(ed25519.PublicKey)
	got, err := link.NewSenderAuthenticated(testSecret, publicKey, []string{"relay.example"})
	if err != nil {
		t.Fatal(err)
	}
	if got.ShareID != "tVW-68OSeLBZTpU-" {
		t.Fatalf("share ID = %q", got.ShareID)
	}
	key, err := got.KeyString()
	if err != nil {
		t.Fatal(err)
	}
	if key != "AgABAgMEBQYHCAkKCwwNDg8kQqCgOKPbpFQm33t9wk_E" {
		t.Fatalf("key string = %q", key)
	}
	full, err := got.URL("https://windshare.example/app")
	if err != nil {
		t.Fatal(err)
	}
	parsed, err := link.Parse(full)
	if err != nil || !reflect.DeepEqual(parsed, got) {
		t.Fatalf("v2 round trip = %+v, %v", parsed, err)
	}
	bare, separate, err := link.Split(full)
	if err != nil || separate != key {
		t.Fatalf("v2 split = %q, %q, %v", bare, separate, err)
	}
	merged, err := link.Merge(bare, separate)
	if err != nil || !reflect.DeepEqual(merged, got) {
		t.Fatalf("v2 merge = %+v, %v", merged, err)
	}

	tampered := got
	tampered.ShareID = rawB64(bytes.Repeat([]byte{0xff}, link.SenderAuthenticatedShareIDBytes))
	if _, err := tampered.URL("https://windshare.example"); !errors.Is(err, link.ErrMalformedShareID) {
		t.Fatalf("identity substitution error = %v", err)
	}
	badKey := append([]byte(nil), got.PKHash...)
	badKey[0] ^= 1
	tampered = got
	tampered.PKHash = badKey
	if _, err := tampered.URL("https://windshare.example"); !errors.Is(err, link.ErrMalformedShareID) {
		t.Fatalf("pkHash substitution error = %v", err)
	}
}

func TestNewReadSecret(t *testing.T) {
	secret, err := link.NewReadSecret(bytes.NewReader([]byte{
		1, 2, 3, 4, 5, 6, 7, 8, 9, 10, 11, 12, 13, 14, 15, 16,
	}))
	if err != nil {
		t.Fatal(err)
	}
	want := []byte{1, 2, 3, 4, 5, 6, 7, 8, 9, 10, 11, 12, 13, 14, 15, 16}
	if !bytes.Equal(secret, want) {
		t.Errorf("NewReadSecret = %x, want %x from the injected source", secret, want)
	}
	if _, err := link.NewSenderAuthenticated(secret, testPublicKey, nil); err != nil {
		t.Errorf("generated secret failed capability construction: %v", err)
	}
	if _, err := link.NewReadSecret(bytes.NewReader([]byte{1, 2, 3})); !errors.Is(err, io.ErrUnexpectedEOF) {
		t.Errorf("NewReadSecret with short random source err = %v", err)
	}
}
