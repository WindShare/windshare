package manifest

import (
	"bytes"
	"crypto/aes"
	"crypto/cipher"
	"errors"
	"reflect"
	"strings"
	"testing"

	"github.com/windshare/windshare/core/link"
)

// testKey/testNonce:固定值使 sealed 字节可复现(黄金向量同款做法,B5/B11)。
func testKey() []byte {
	key := make([]byte, 32)
	for i := range key {
		key[i] = byte(i)
	}
	return key
}

func testNonce() []byte {
	nonce := make([]byte, 12)
	for i := range nonce {
		nonce[i] = byte(0xa0 + i)
	}
	return nonce
}

// gcmSeal 用 stdlib 原语独立封装,不经被测代码——用于构造对照密文与恶意输入。
func gcmSeal(t *testing.T, key, nonce, plain, aad []byte) []byte {
	t.Helper()
	block, err := aes.NewCipher(key)
	if err != nil {
		t.Fatalf("NewCipher: %v", err)
	}
	g, err := cipher.NewGCM(block)
	if err != nil {
		t.Fatalf("NewGCM: %v", err)
	}
	return g.Seal(append([]byte(nil), nonce...), nonce, plain, aad)
}

type errReader struct{}

func (errReader) Read([]byte) (int, error) { return 0, errors.New("rng failure") }

func TestSealDeterministicBytes(t *testing.T) {
	key, nonce := testKey(), testNonce()
	first, err := Seal(key, goldenManifest(), bytes.NewReader(nonce))
	if err != nil {
		t.Fatalf("Seal: %v", err)
	}
	second, err := Seal(key, goldenManifest(), bytes.NewReader(nonce))
	if err != nil {
		t.Fatalf("Seal: %v", err)
	}
	if !bytes.Equal(first, second) {
		t.Fatalf("Seal bytes differ with a fixed rng")
	}
	if !bytes.Equal(first[:len(nonce)], nonce) {
		t.Fatalf("nonce is not prefixed to sealed data: %x", first[:len(nonce)])
	}
	// 对照:独立实现的 nonce‖GCM(CBOR, aad=0x01)。0x01 取字面量,连带钉住
	// link.SuiteAESGCM 的协议值。
	want := gcmSeal(t, key, nonce, mustHex(t, hexGolden), []byte{0x01})
	if !bytes.Equal(first, want) {
		t.Fatalf("sealed data differs from independent reference:\n got=%x\nwant=%x", first, want)
	}
}

func TestSealedFingerprintOwnsManifestIdentity(t *testing.T) {
	aead, err := newAEAD(testKey())
	if err != nil {
		t.Fatalf("newAEAD: %v", err)
	}
	if aead.NonceSize() != sealedNonceBytes || aead.Overhead() != FingerprintBytes {
		t.Fatalf("suite envelope shape = nonce %d/tag %d, want %d/%d", aead.NonceSize(), aead.Overhead(), sealedNonceBytes, FingerprintBytes)
	}
	sealed, err := Seal(testKey(), goldenManifest(), bytes.NewReader(testNonce()))
	if err != nil {
		t.Fatalf("Seal: %v", err)
	}
	fingerprint, err := SealedFingerprint(sealed)
	if err != nil {
		t.Fatalf("SealedFingerprint: %v", err)
	}
	if !bytes.Equal(fingerprint[:], sealed[len(sealed)-FingerprintBytes:]) {
		t.Fatalf("fingerprint %x is not the sealed manifest tag", fingerprint)
	}
	sealed[len(sealed)-1] ^= 0xff
	if bytes.Equal(fingerprint[:], sealed[len(sealed)-FingerprintBytes:]) {
		t.Fatal("fingerprint retained mutable sealed storage")
	}
}

func TestSealedFingerprintRejectsInvalidEnvelopeSize(t *testing.T) {
	tests := []struct {
		name   string
		sealed []byte
		want   error
	}{
		{"short", make([]byte, sealedNonceBytes+FingerprintBytes-1), ErrSealedTooShort},
		{"oversized", make([]byte, MaxManifestSize+1), ErrManifestTooLarge},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if _, err := SealedFingerprint(tt.sealed); !errors.Is(err, tt.want) {
				t.Fatalf("SealedFingerprint = %v, want %v", err, tt.want)
			}
		})
	}
}

func TestSealOpenRoundTrip(t *testing.T) {
	m := New(1<<20, []Entry{
		{Path: "docs", IsDir: true, MTime: 1710000000000},
		{Path: "docs/读我.md", Size: 42, MTime: -1},
		{Path: "empty", Size: 0, MTime: 0},
	})
	// rng 传 nil 走 crypto/rand:生产路径的缺省行为也须能往返。
	sealed, err := Seal(testKey(), m, nil)
	if err != nil {
		t.Fatalf("Seal: %v", err)
	}
	got, err := Open(testKey(), sealed)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	if !reflect.DeepEqual(got, m) {
		t.Fatalf("round-trip mismatch:\n got=%+v\nwant=%+v", got, m)
	}
}

func TestAuthenticatedCanonicalManifestMTimeBoundaries(t *testing.T) {
	tests := []struct {
		mtime   int64
		invalid bool
	}{
		{mtime: MinMTimeMilliseconds},
		{mtime: MaxMTimeMilliseconds},
		{mtime: MinMTimeMilliseconds - 1, invalid: true},
		{mtime: MaxMTimeMilliseconds + 1, invalid: true},
	}
	for _, tt := range tests {
		m := New(1<<20, []Entry{{Path: "a", MTime: tt.mtime}})
		plain, err := encMode.Marshal(m)
		if err != nil {
			t.Fatalf("Marshal(mtime=%d): %v", tt.mtime, err)
		}
		sealed := gcmSeal(t, testKey(), testNonce(), plain, suiteAAD(link.SuiteAESGCM))
		opened, err := Open(testKey(), sealed)
		if err != nil {
			t.Fatalf("Open authenticated canonical manifest (mtime=%d): %v", tt.mtime, err)
		}
		validateErr := opened.Validate()
		_, sealErr := Seal(testKey(), m, bytes.NewReader(testNonce()))
		if tt.invalid {
			if !errors.Is(validateErr, ErrMTimeOutOfRange) || !errors.Is(sealErr, ErrMTimeOutOfRange) {
				t.Fatalf("mtime=%d Validate/Seal errors = %v / %v, want ErrMTimeOutOfRange", tt.mtime, validateErr, sealErr)
			}
			continue
		}
		if validateErr != nil || sealErr != nil {
			t.Fatalf("safe mtime=%d Validate/Seal errors = %v / %v", tt.mtime, validateErr, sealErr)
		}
	}
}

func TestSealOpenEmptyEntries(t *testing.T) {
	sealed, err := Seal(testKey(), New(1<<20, nil), bytes.NewReader(testNonce()))
	if err != nil {
		t.Fatalf("Seal: %v", err)
	}
	got, err := Open(testKey(), sealed)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	if len(got.Entries) != 0 {
		t.Fatalf("empty manifest round trip produced extra entries: %+v", got.Entries)
	}
}

func TestOpenRejectsTamperAndWrongKey(t *testing.T) {
	key := testKey()
	sealed, err := Seal(key, goldenManifest(), bytes.NewReader(testNonce()))
	if err != nil {
		t.Fatalf("Seal: %v", err)
	}
	flip := func(pos int) []byte {
		b := append([]byte(nil), sealed...)
		b[pos] ^= 0x01
		return b
	}
	tests := []struct {
		name   string
		key    []byte
		sealed []byte
	}{
		{name: "篡改 nonce", key: key, sealed: flip(0)},
		{name: "篡改密文首字节", key: key, sealed: flip(12)},
		{name: "篡改 tag 末字节", key: key, sealed: flip(len(sealed) - 1)},
		{name: "截断至不足 nonce+tag", key: key, sealed: sealed[:27]},
		{name: "空 sealed", key: key, sealed: nil},
		{name: "错误密钥", key: append([]byte{0xff}, key[1:]...), sealed: sealed},
		{name: "密钥长度不符", key: key[:16], sealed: sealed},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if m, err := Open(tt.key, tt.sealed); err == nil {
				t.Fatalf("expected rejection, got %+v", m)
			}
		})
	}
}

func TestOpenRejectsShortSealedWithSentinel(t *testing.T) {
	for _, sealed := range [][]byte{nil, make([]byte, len(testNonce())+aes.BlockSize-1)} {
		if _, err := Open(testKey(), sealed); !errors.Is(err, ErrSealedTooShort) {
			t.Fatalf("len=%d: err=%v, want ErrSealedTooShort", len(sealed), err)
		}
	}
}

func TestOpenRejectsWrongAAD(t *testing.T) {
	// 用 0x02 作 AAD 封装:密文本身完好,但域分隔不符,Open(按 0x01)必须拒绝。
	sealed := gcmSeal(t, testKey(), testNonce(), mustHex(t, hexGolden), []byte{0x02})
	if m, err := Open(testKey(), sealed); err == nil {
		t.Fatalf("unseal succeeded with mismatched AAD: %+v", m)
	}
}

func TestOpenUnsupportedVersionEndToEnd(t *testing.T) {
	// 未来版本清单经完整 Open 路径必须报"请升级",而非 CBOR 结构错误(B15)。
	v2 := mustHex(t, "a3"+hexKeyV+"02"+hexKeyEntries+"80"+hexKeyChunkSize+hexChunk1MiB)
	sealed := gcmSeal(t, testKey(), testNonce(), v2, []byte{0x01})
	if _, err := Open(testKey(), sealed); !errors.Is(err, ErrUnsupportedVersion) {
		t.Fatalf("want ErrUnsupportedVersion, got %v", err)
	}
}

func TestOpenSuiteDispatch(t *testing.T) {
	sealed, err := Seal(testKey(), goldenManifest(), bytes.NewReader(testNonce()))
	if err != nil {
		t.Fatalf("Seal: %v", err)
	}
	if _, err := openSuite(0x7f, testKey(), sealed); !errors.Is(err, ErrUnsupportedSuite) {
		t.Fatalf("want ErrUnsupportedSuite, got %v", err)
	}
}

func TestOpenRejectsOversizedSealed(t *testing.T) {
	if _, err := Open(testKey(), make([]byte, MaxManifestSize+1)); !errors.Is(err, ErrManifestTooLarge) {
		t.Fatalf("want ErrManifestTooLarge, got %v", err)
	}
}

func TestSealRejectsOversizedManifest(t *testing.T) {
	// 单条超长路径把 CBOR 撑过 MaxManifestSize:预检必须在发送端就明确报错。
	m := New(1<<20, []Entry{{Path: strings.Repeat("a", MaxManifestSize), Size: 1}})
	if _, err := Seal(testKey(), m, bytes.NewReader(testNonce())); !errors.Is(err, ErrManifestTooLarge) {
		t.Fatalf("want ErrManifestTooLarge, got %v", err)
	}
}

func TestSealErrors(t *testing.T) {
	valid := goldenManifest()
	tests := []struct {
		name    string
		key     []byte
		m       *Manifest
		rng     interface{ Read([]byte) (int, error) }
		wantErr error
	}{
		{name: "非法清单被拒", key: testKey(), m: New(1<<20, []Entry{{Path: "../x"}}), rng: bytes.NewReader(testNonce()), wantErr: ErrInvalidPath},
		{name: "版本未填(零值)", key: testKey(), m: &Manifest{ChunkSize: 1 << 20}, rng: bytes.NewReader(testNonce()), wantErr: ErrUnsupportedVersion},
		{name: "密钥长度不符", key: testKey()[:16], m: valid, rng: bytes.NewReader(testNonce())},
		{name: "rng 读取失败", key: testKey(), m: valid, rng: errReader{}},
		{name: "rng 供给不足", key: testKey(), m: valid, rng: bytes.NewReader(testNonce()[:4])},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := Seal(tt.key, tt.m, tt.rng)
			if err == nil {
				t.Fatalf("expected error")
			}
			if tt.wantErr != nil && !errors.Is(err, tt.wantErr) {
				t.Fatalf("error category mismatch: got %v, want %v", err, tt.wantErr)
			}
		})
	}
}
