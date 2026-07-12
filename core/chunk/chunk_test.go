package chunk

import (
	"bytes"
	"crypto/aes"
	"crypto/cipher"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"math"
	"sync"
	"testing"

	"github.com/windshare/windshare/core/internal/keyderiv"
	"github.com/windshare/windshare/core/layout"
	"github.com/windshare/windshare/core/link"
)

var testStreamKey = func() []byte {
	k := make([]byte, 32)
	for i := range k {
		k[i] = byte(i)
	}
	return k
}()

// patternRNG 产出确定性伪随机字节(i*7+3 无特殊含义,只求非平凡且可复现),
// 供需要「注入固定随机源」的用例(§7:严禁测试路径落 crypto/rand 直调)。
func patternRNG(n int) *bytes.Reader {
	b := make([]byte, n)
	for i := range b {
		b[i] = byte(i*7 + 3)
	}
	return bytes.NewReader(b)
}

func newTestCodec(t *testing.T, chunkSize int64, rng io.Reader) *Codec {
	t.Helper()
	c, err := NewCodec(testStreamKey, chunkSize, rng)
	if err != nil {
		t.Fatalf("NewCodec: %v", err)
	}
	return c
}

// §8 的协议常量:值变更破坏既有分享的可解密性,钉死防误改。
func TestProtocolConstants(t *testing.T) {
	if DefaultChunkSize != 1<<20 {
		t.Errorf("DefaultChunkSize = %d, want 1 MiB", DefaultChunkSize)
	}
	if SegmentBytes != 16<<30 {
		t.Errorf("SegmentBytes = %d, want 16 GiB", SegmentBytes)
	}
	if SegmentBytes <= layout.MaxChunkSize || SegmentBytes%layout.MaxChunkSize != 0 {
		t.Errorf("SegmentBytes=%d must be a larger exact multiple of MaxChunkSize=%d", SegmentBytes, layout.MaxChunkSize)
	}
	if MaxSealsPerSegKey != 1<<32 {
		t.Errorf("MaxSealsPerSegKey = %d, want 2³²", MaxSealsPerSegKey)
	}
	if NonceBytes != 12 || TagBytes != 16 {
		t.Errorf("NonceBytes/TagBytes = %d/%d, want 12/16", NonceBytes, TagBytes)
	}
}

func TestSuiteOwnedSealedSizes(t *testing.T) {
	const plaintextBytes int64 = 1234
	exact, err := SealedSize(link.SuiteAESGCM, plaintextBytes)
	if err != nil {
		t.Fatalf("SealedSize: %v", err)
	}
	if want := plaintextBytes + NonceBytes + TagBytes; exact != want {
		t.Fatalf("SealedSize = %d, want %d", exact, want)
	}

	maximum, err := MaxSealedSize(link.SuiteAESGCM, layout.MaxChunkSize)
	if err != nil {
		t.Fatalf("MaxSealedSize: %v", err)
	}
	if want := layout.MaxChunkSize + NonceBytes + TagBytes; maximum != want {
		t.Fatalf("MaxSealedSize = %d, want %d", maximum, want)
	}

	tests := []struct {
		name string
		call func() error
		want error
	}{
		{"negative plaintext", func() error { _, err := SealedSize(link.SuiteAESGCM, -1); return err }, ErrInvalidPlaintextSize},
		{"size overflow", func() error { _, err := SealedSize(link.SuiteAESGCM, math.MaxInt64); return err }, ErrSealedSizeOverflow},
		{"unknown suite", func() error { _, err := SealedSize(0x7f, 0); return err }, ErrUnknownSuite},
		{"invalid maximum", func() error { _, err := MaxSealedSize(link.SuiteAESGCM, 1); return err }, ErrInvalidChunkSize},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if err := tt.call(); !errors.Is(err, tt.want) {
				t.Fatalf("error = %v, want %v", err, tt.want)
			}
		})
	}
}

func TestNewCodecErrors(t *testing.T) {
	rng := patternRNG(64)
	tests := []struct {
		name      string
		streamKey []byte
		chunkSize int64
		rng       io.Reader
		wantErr   error
	}{
		{"streamKey 短", testStreamKey[:16], DefaultChunkSize, rng, ErrInvalidStreamKey},
		{"streamKey 空", nil, DefaultChunkSize, rng, ErrInvalidStreamKey},
		{"chunkSize 零", testStreamKey, 0, rng, ErrInvalidChunkSize},
		{"chunkSize 负", testStreamKey, -1024, rng, ErrInvalidChunkSize},
		{"chunkSize 非 2 幂", testStreamKey, 1000, rng, ErrInvalidChunkSize},
		{"chunkSize 非 2 幂小值", testStreamKey, 3, rng, ErrInvalidChunkSize},
		{"chunkSize 低于几何下限", testStreamKey, layout.MinChunkSize / 2, rng, ErrInvalidChunkSize},
		{"chunkSize 超布局上限", testStreamKey, layout.MaxChunkSize * 2, rng, ErrInvalidChunkSize},
		{"rng 空", testStreamKey, DefaultChunkSize, nil, ErrNilRand},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if _, err := NewCodec(tt.streamKey, tt.chunkSize, tt.rng); !errors.Is(err, tt.wantErr) {
				t.Errorf("err = %v, want %v", err, tt.wantErr)
			}
		})
	}
	if _, err := NewCodec(testStreamKey, layout.MinChunkSize/2, rng); !errors.Is(err, layout.ErrChunkSizeTooSmall) {
		t.Errorf("geometry cause was not preserved: %v", err)
	}
	// 边界合法值:协议最小块与布局允许的最大块。
	for _, ok := range []int64{layout.MinChunkSize, layout.MaxChunkSize} {
		if _, err := NewCodec(testStreamKey, ok, rng); err != nil {
			t.Errorf("chunkSize=%d should be valid: %v", ok, err)
		}
	}
}

func TestSealOpenRoundTrip(t *testing.T) {
	const chunkSize = 4096
	tests := []struct {
		name string
		i    uint64
		pt   []byte
	}{
		{"整块", 0, bytes.Repeat([]byte{0xA5}, chunkSize)},
		{"末块短块", 1, []byte("短块明文")},
		{"单字节", 7, []byte{0x00}},
		{"空明文", 9, nil},
		{"几何上界块号", layout.MaxChunkCount - 1, []byte("全局块号上界")},
	}
	sealer := newTestCodec(t, chunkSize, patternRNG(NonceBytes*len(tests)))
	opener := newTestCodec(t, chunkSize, patternRNG(0)) // 解密侧不消耗随机
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			blockCT, err := sealer.Seal(tt.i, tt.pt)
			if err != nil {
				t.Fatalf("Seal: %v", err)
			}
			sealedSize, err := SealedSize(link.SuiteAESGCM, int64(len(tt.pt)))
			if err != nil {
				t.Fatalf("SealedSize: %v", err)
			}
			if got, want := len(blockCT), int(sealedSize); got != want {
				t.Errorf("ciphertext length %d, want %d (nonce‖ct‖tag)", got, want)
			}
			pt, err := opener.Open(tt.i, blockCT)
			if err != nil {
				t.Fatalf("Open: %v", err)
			}
			if !bytes.Equal(pt, tt.pt) {
				t.Errorf("round-trip mismatch: got %q, want %q", pt, tt.pt)
			}
		})
	}
}

// 注入相同固定 RNG 的两个 Codec 必须产出逐字节相同的密文——金标向量确定性
// 的前提(B11);且 nonce 逐字节等于注入源产出。
func TestSealDeterministicWithFixedRNG(t *testing.T) {
	pt := []byte("determinism is the lifeline of golden vectors")
	c1 := newTestCodec(t, 1024, patternRNG(NonceBytes))
	c2 := newTestCodec(t, 1024, patternRNG(NonceBytes))
	out1, err := c1.Seal(3, pt)
	if err != nil {
		t.Fatal(err)
	}
	out2, err := c2.Seal(3, pt)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(out1, out2) {
		t.Error("Seal output differs for identical input and random source")
	}
	wantNonce := make([]byte, NonceBytes)
	patternRNG(NonceBytes).Read(wantNonce)
	if !bytes.Equal(out1[:NonceBytes], wantNonce) {
		t.Errorf("nonce = %x, want first 12 injected bytes %x", out1[:NonceBytes], wantNonce)
	}
}

// manualSeal 用 keyderiv+GCM 手工复现线格式(独立于 Codec 的实现路径),
// 钉死 AAD=suiteByte‖u64_be(i) 与 nonce‖ct‖tag 布局不随重构漂移。
func manualSeal(t *testing.T, seg uint32, i uint64, nonce, pt []byte) []byte {
	t.Helper()
	block, err := aes.NewCipher(keyderiv.SegKey(testStreamKey, seg))
	if err != nil {
		t.Fatal(err)
	}
	g, err := cipher.NewGCM(block)
	if err != nil {
		t.Fatal(err)
	}
	aad := append([]byte{link.SuiteAESGCM}, binary.BigEndian.AppendUint64(nil, i)...)
	return g.Seal(append([]byte(nil), nonce...), nonce, pt, aad)
}

func TestSealMatchesManualConstruction(t *testing.T) {
	const chunkSize = 1024
	chunksPerSeg := uint64(SegmentBytes / chunkSize)
	pt := []byte("cross-check against hand-rolled GCM")
	tests := []struct {
		name string
		i    uint64
		seg  uint32
	}{
		{"段 0 首块", 0, 0},
		{"段 0 末块", chunksPerSeg - 1, 0},
		{"段 1 首块(16GiB 段边界)", chunksPerSeg, 1},
		{"段 2", 2*chunksPerSeg + 5, 2},
	}
	rng := patternRNG(NonceBytes * len(tests))
	c := newTestCodec(t, chunkSize, rng)
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := c.Seal(tt.i, pt)
			if err != nil {
				t.Fatalf("Seal: %v", err)
			}
			want := manualSeal(t, tt.seg, tt.i, got[:NonceBytes], pt)
			if !bytes.Equal(got, want) {
				t.Errorf("block %d (segment %d) differs from manual construction\n got  %x\n want %x", tt.i, tt.seg, got, want)
			}
		})
	}
	// 懒派生只触碰实际用到的段。
	if len(c.segs) != 3 {
		t.Errorf("cached segment count = %d, want 3", len(c.segs))
	}
}

// 段边界两侧密钥不同:把段 1 首块的密文挪回段 0 末块位置,tag 必须失败
// (密钥与 AAD 双重不符)。
func TestSegmentKeyIsolation(t *testing.T) {
	const chunkSize = 1024
	chunksPerSeg := uint64(SegmentBytes / chunkSize)
	c := newTestCodec(t, chunkSize, patternRNG(NonceBytes*2))
	pt := []byte("same plaintext, different segments")
	ctSeg1, err := c.Seal(chunksPerSeg, pt)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := c.Open(chunksPerSeg-1, ctSeg1); err == nil {
		t.Error("Open at the wrong segment position should fail")
	}
	if _, err := c.Open(chunksPerSeg, ctSeg1); err != nil {
		t.Errorf("Open at the original position should succeed: %v", err)
	}
}

func TestOpenRejectsTamperAndMisuse(t *testing.T) {
	const i = 5
	pt := []byte("authenticated payload")
	c := newTestCodec(t, 1024, patternRNG(NonceBytes))
	blockCT, err := c.Seal(i, pt)
	if err != nil {
		t.Fatal(err)
	}
	flip := func(pos int) []byte {
		b := append([]byte(nil), blockCT...)
		b[pos] ^= 0x01
		return b
	}
	tests := []struct {
		name string
		i    uint64
		ct   []byte
	}{
		{"篡改 nonce", i, flip(0)},
		{"篡改密文体", i, flip(NonceBytes + 2)},
		{"篡改 tag", i, flip(len(blockCT) - 1)},
		{"错位块号(AAD 失败)", i + 1, blockCT},
		{"截断到 tag 边界内", i, blockCT[:NonceBytes+TagBytes-1]},
		{"空密文", i, nil},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if _, err := c.Open(tt.i, tt.ct); err == nil {
				t.Error("operation succeeded, want rejection")
			}
		})
	}
	t.Run("过短密文报 ErrBlockTooShort", func(t *testing.T) {
		if _, err := c.Open(i, blockCT[:NonceBytes]); !errors.Is(err, ErrBlockTooShort) {
			t.Errorf("err = %v, want ErrBlockTooShort", err)
		}
	})
	t.Run("原样 Open 仍成功", func(t *testing.T) {
		if _, err := c.Open(i, blockCT); err != nil {
			t.Errorf("untampered ciphertext should decrypt: %v", err)
		}
	})
}

// 跨密钥(不同 streamKey → 不同 segKey 树)Open 必须失败:同源密钥绑定是
// 完整性论证的第四支柱(§6.5)。
func TestOpenRejectsForeignKey(t *testing.T) {
	c := newTestCodec(t, 1024, patternRNG(NonceBytes))
	blockCT, err := c.Seal(0, []byte("bound to streamKey A"))
	if err != nil {
		t.Fatal(err)
	}
	otherKey := bytes.Repeat([]byte{0xEE}, keyderiv.KeyBytes)
	foreign, err := NewCodec(otherKey, 1024, patternRNG(0))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := foreign.Open(0, blockCT); err == nil {
		t.Error("Open with a different key should fail")
	}
}

// Seal 计数熔断(B12):白盒把计数拨到上限前一步,验证「最后一次成功、
// 再一次熔断、他段不受累」。真实触发需 2³² 次 Seal,不可行也不必要。
func TestSealCountFuse(t *testing.T) {
	c := newTestCodec(t, layout.MaxChunkSize, patternRNG(NonceBytes*3))
	chunksPerSeg := uint64(SegmentBytes / layout.MaxChunkSize)
	st, err := c.segStateLocked(0)
	if err != nil {
		t.Fatal(err)
	}
	st.seals = MaxSealsPerSegKey - 1
	if _, err := c.Seal(0, []byte("最后一枚预算")); err != nil {
		t.Fatalf("last Seal before the limit should succeed: %v", err)
	}
	if _, err := c.Seal(0, []byte("超限")); !errors.Is(err, ErrSealLimit) {
		t.Errorf("err = %v, want ErrSealLimit", err)
	}
	if _, err := c.Seal(chunksPerSeg, []byte("他段不受累")); err != nil {
		t.Errorf("Seal in another segment should succeed: %v", err)
	}
}

// 熔断后 Open 不受影响:接收端续传/重解已有密文与发送端 Seal 预算无关。
func TestOpenUnaffectedByFuse(t *testing.T) {
	c := newTestCodec(t, 1024, patternRNG(NonceBytes))
	blockCT, err := c.Seal(0, []byte("sealed before fuse"))
	if err != nil {
		t.Fatal(err)
	}
	c.segs[0].seals = MaxSealsPerSegKey
	if _, err := c.Open(0, blockCT); err != nil {
		t.Errorf("circuit breaker applies only to Seal; Open should succeed: %v", err)
	}
}

func TestBlockIndexGeometryBoundPrecedesSegmentState(t *testing.T) {
	for _, chunkSize := range []int64{layout.MinChunkSize, layout.MaxChunkSize} {
		t.Run(fmt.Sprintf("chunkSize=%d", chunkSize), func(t *testing.T) {
			rng := patternRNG(NonceBytes)
			c := newTestCodec(t, chunkSize, rng)
			for _, index := range []uint64{layout.MaxChunkCount, math.MaxUint64} {
				if _, err := c.Seal(index, []byte("invalid index")); !errors.Is(err, ErrBlockIndexOutOfRange) {
					t.Errorf("Seal(%d) err = %v, want ErrBlockIndexOutOfRange", index, err)
				}
				if _, err := c.Open(index, make([]byte, NonceBytes+TagBytes)); !errors.Is(err, ErrBlockIndexOutOfRange) {
					t.Errorf("Open(%d) err = %v, want ErrBlockIndexOutOfRange", index, err)
				}
			}
			if len(c.segs) != 0 {
				t.Fatalf("invalid indices retained %d segment states", len(c.segs))
			}
			if rng.Len() != NonceBytes {
				t.Fatalf("invalid indices consumed nonce bytes: remaining=%d", rng.Len())
			}
		})
	}
}

func TestPlaintextTooLong(t *testing.T) {
	c := newTestCodec(t, 1024, patternRNG(NonceBytes))
	if _, err := c.Seal(0, make([]byte, 1025)); !errors.Is(err, ErrPlaintextTooLong) {
		t.Errorf("err = %v, want ErrPlaintextTooLong", err)
	}
}

func TestCiphertextTooLongRejectedBeforeOpen(t *testing.T) {
	c := newTestCodec(t, 1024, patternRNG(NonceBytes))
	oversized := make([]byte, 1024+NonceBytes+TagBytes+1)
	if _, err := c.Open(0, oversized); !errors.Is(err, ErrBlockTooLong) {
		t.Errorf("err = %v, want ErrBlockTooLong", err)
	}
	if len(c.segs) != 0 {
		t.Fatalf("oversized ciphertext retained %d segment states", len(c.segs))
	}
}

func TestOpenAuthenticationFailureDoesNotRetainSegmentState(t *testing.T) {
	const attemptedSegments = 256
	c := newTestCodec(t, layout.MaxChunkSize, patternRNG(0))
	invalid := make([]byte, NonceBytes+TagBytes)
	for segment := range uint64(attemptedSegments) {
		index := segment * c.chunksPerSeg
		if index >= layout.MaxChunkCount {
			t.Fatalf("test segment %d maps outside geometry at block %d", segment, index)
		}
		if _, err := c.Open(index, invalid); err == nil {
			t.Fatalf("unauthenticated block %d unexpectedly opened", index)
		}
	}
	if len(c.segs) != 0 {
		t.Fatalf("unauthenticated segments retained %d cache entries, want 0", len(c.segs))
	}

	nonce := make([]byte, NonceBytes)
	authenticated := manualSeal(t, 0, 0, nonce, []byte("authenticated segment"))
	if _, err := c.Open(0, authenticated); err != nil {
		t.Fatalf("authenticated Open: %v", err)
	}
	if len(c.segs) != 1 {
		t.Fatalf("authenticated segment cache entries = %d, want 1", len(c.segs))
	}
	if _, err := c.Open(c.chunksPerSeg, invalid); err == nil {
		t.Fatal("unauthenticated second segment unexpectedly opened")
	}
	if len(c.segs) != 1 {
		t.Fatalf("failed second segment changed cache size to %d", len(c.segs))
	}
}

// Open 的解析按 suiteByte 分派(B13):未知 suite 明确报错,而非按 0x01 布局硬解。
func TestUnknownSuiteDispatch(t *testing.T) {
	c := newTestCodec(t, 1024, patternRNG(NonceBytes))
	blockCT, err := c.Seal(0, []byte("x"))
	if err != nil {
		t.Fatal(err)
	}
	c.suite = 0x7F // 白盒模拟未来 suite 的 Codec 遇到本实现不认识的布局
	if _, err := c.Open(0, blockCT); !errors.Is(err, ErrUnknownSuite) {
		t.Errorf("err = %v, want ErrUnknownSuite", err)
	}
	if _, _, err := parseBlockCT(0x02, make([]byte, 1024)); !errors.Is(err, ErrUnknownSuite) {
		t.Errorf("parseBlockCT(0x02) err = %v, want ErrUnknownSuite (0x02 signature parsing belongs to M2)", err)
	}
}

// 随机源故障必须让 Seal 失败,绝不退化到可预测 nonce。
func TestSealRNGFailure(t *testing.T) {
	failRNG := errors.New("rng failure")
	tests := []struct {
		name string
		rng  io.Reader
	}{
		{"读即报错", readerFunc(func([]byte) (int, error) { return 0, failRNG })},
		{"随机源耗尽", bytes.NewReader([]byte{1, 2, 3})},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			c := newTestCodec(t, 1024, tt.rng)
			if _, err := c.Seal(0, []byte("x")); err == nil {
				t.Error("Seal should fail when the random source fails")
			}
		})
	}
}

type readerFunc func(p []byte) (int, error)

func (f readerFunc) Read(p []byte) (int, error) { return f(p) }

// infiniteRNG 供并发用例:确定性无关紧要,只验证锁下的计数与缓存不竞争。
type infiniteRNG struct{}

func (infiniteRNG) Read(p []byte) (int, error) {
	for i := range p {
		p[i] = byte(i)
	}
	return len(p), nil
}

func TestConcurrentSealOpen(t *testing.T) {
	const (
		workers = 8
		blocks  = 64
	)
	sealer := newTestCodec(t, 1024, infiniteRNG{})
	opener := newTestCodec(t, 1024, infiniteRNG{})
	var wg sync.WaitGroup
	errCh := make(chan error, workers)
	for w := range workers {
		wg.Go(func() {
			for b := range blocks {
				i := uint64(w*blocks + b)
				pt := fmt.Appendf(nil, "block-%d", i)
				ct, err := sealer.Seal(i, pt)
				if err != nil {
					errCh <- err
					return
				}
				got, err := opener.Open(i, ct)
				if err != nil {
					errCh <- err
					return
				}
				if !bytes.Equal(got, pt) {
					errCh <- fmt.Errorf("block %d round-trip mismatch", i)
					return
				}
			}
		})
	}
	wg.Wait()
	close(errCh)
	for err := range errCh {
		t.Error(err)
	}
	var total uint64
	for _, st := range sealer.segs {
		total += st.seals
	}
	if total != workers*blocks {
		t.Errorf("total Seal count = %d, want %d", total, workers*blocks)
	}
}
