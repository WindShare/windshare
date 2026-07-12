package share

import (
	"crypto/rand"
	"encoding/base64"
	"fmt"
	"io"
	"slices"

	"github.com/windshare/windshare/core/chunk"
	"github.com/windshare/windshare/core/internal/keyderiv"
	"github.com/windshare/windshare/core/layout"
	"github.com/windshare/windshare/core/link"
	"github.com/windshare/windshare/core/manifest"
	"github.com/windshare/windshare/core/session"
)

// Sharer 是发送侧门面(§6.6):对一份固定的元数据快照提供链接、加密名片
// (sealedManifest)与按需加密块。密钥派生只在构造期经 keyderiv 接线一次,
// 之后 manifest/chunk 只见派生好的 key(§6.3)。
//
// 并发:发送端扇出时多个会话共享同一 Sharer(§6.6)。Chunk/BlockStore 的
// 并发安全性随注入的 FileSource(osfs.Source 满足);Codec 内部有锁,
// Seal 计数熔断(B12)因此按分享全局计。Link/SealedManifest 为只读。
type Sharer struct {
	lnk    link.Link
	src    FileSource
	lay    *layout.Layout
	codec  *chunk.Codec
	sealed []byte
}

// NewSharer 组装一次分享:定身份(readSecret/shareId)→ 规范序 → 派生几何
// 与密钥 → Seal 清单。清单在此仅 Seal 一次(含 MaxManifestSize 预检,出链接
// 前即报错,§6.9),SealedManifest 复用这份字节(§6.3)。
func NewSharer(files []FileMeta, src FileSource, opt Options) (*Sharer, error) {
	if src == nil {
		return nil, fmt.Errorf("%w:src", ErrNilDependency)
	}
	chunkSize := opt.ChunkSize
	if chunkSize == 0 {
		chunkSize = chunk.DefaultChunkSize
	}
	rng := opt.Rand
	if rng == nil {
		rng = rand.Reader
	}
	secret, shareID, err := resolveIdentity(opt, rng)
	if err != nil {
		return nil, err
	}
	// 规范序在门面落地(§6.4 发送端职责):克隆后排序,不动调用方切片;
	// 清单 entries 数组顺序 = 流顺序,自此统一。
	metas := slices.Clone(files)
	slices.SortFunc(metas, func(a, b FileMeta) int { return layout.CompareCanonical(a.Path, b.Path) })
	entries := make([]manifest.Entry, len(metas))
	layEntries := make([]layout.Entry, len(metas))
	for i, f := range metas {
		size := f.Size
		if f.IsDir {
			// 目录 size 因平台而异且不占流(§6.4),清单统一记 0(与 osfs.Walk 同规)。
			size = 0
		}
		entries[i] = manifest.Entry{Path: f.Path, Size: size, MTime: f.MTime, IsDir: f.IsDir}
		layEntries[i] = layout.Entry{Path: f.Path, Size: size, IsDir: f.IsDir}
	}
	lay, err := layout.New(layEntries, chunkSize)
	if err != nil {
		return nil, err
	}
	sealed, err := manifest.Seal(keyderiv.ManifestKey(secret), manifest.New(chunkSize, entries), rng)
	if err != nil {
		return nil, err
	}
	codec, err := chunk.NewCodec(keyderiv.StreamKey(secret), chunkSize, rng)
	if err != nil {
		return nil, err
	}
	return &Sharer{
		lnk:    link.Link{Suite: link.SuiteAESGCM, ReadSecret: secret, ShareID: shareID},
		src:    src,
		lay:    lay,
		codec:  codec,
		sealed: sealed,
	}, nil
}

// resolveIdentity 定出分享身份:未注入即从 rng 生成。生成顺序 readSecret →
// shareId 是金标向量依赖的消耗次序(B11),不可对调。
func resolveIdentity(opt Options, rng io.Reader) (secret []byte, shareID string, err error) {
	if opt.ReadSecret == nil {
		if secret, err = link.NewReadSecret(rng); err != nil {
			return nil, "", err
		}
	} else {
		if len(opt.ReadSecret) != link.ReadSecretBytes {
			return nil, "", fmt.Errorf("%w: readSecret must be %d bytes, got %d", ErrBadOptions, link.ReadSecretBytes, len(opt.ReadSecret))
		}
		// 克隆:readSecret 决定整棵密钥树,不能被调用方后续改动波及。
		secret = slices.Clone(opt.ReadSecret)
	}
	if opt.ShareID == "" {
		if shareID, err = link.NewShareID(rng); err != nil {
			return nil, "", err
		}
	} else {
		raw, decErr := base64.RawURLEncoding.DecodeString(opt.ShareID)
		if decErr != nil || len(raw) != link.ShareIDBytes {
			return nil, "", fmt.Errorf("%w: shareId must be %d-byte base64url, got %q", ErrBadOptions, link.ShareIDBytes, opt.ShareID)
		}
		shareID = opt.ShareID
	}
	return secret, shareID, nil
}

// Link 返回能力链接的语义结构(§6.6;构造期只做过元数据工作,秒级)。
// Relays 由注册流程按实际中转另填——?r= 是路由提示,不归加密门面。
func (s *Sharer) Link() link.Link {
	l := s.lnk
	// 克隆防调用方经返回值改动门面持有的密钥。
	l.ReadSecret = slices.Clone(s.lnk.ReadSecret)
	return l
}

// SealedManifest 返回交给中转的加密名片。每分享仅 Seal 一次(NewSharer 内),
// 此处只复用字节——GCM tag 即清单指纹,续传 journal 锚定它,重 Seal 会令
// 指纹漂移(§6.3)。返回克隆,缓存字节不可被调用方改动。
func (s *Sharer) SealedManifest() ([]byte, error) {
	return slices.Clone(s.sealed), nil
}

// Chunk 按需产出块 i 的密文(nonce‖ct‖tag):ReadRange 拼块明文 →
// chunk.Seal(随机 nonce)。块号统一 uint64,与数据面帧对齐(§6.6)。
func (s *Sharer) Chunk(i uint64) ([]byte, error) {
	plaintext, err := s.plaintext(i)
	if err != nil {
		return nil, err
	}
	return s.codec.Seal(i, plaintext)
}

// NumChunks 返回块总数 N = ⌈streamLen/chunkSize⌉。
func (s *Sharer) NumChunks() uint64 { return s.lay.NumChunks() }

// BlockStore 返回发送会话的明文块来源(session.NewSendSession 的 store)。
// 与 Sealer 拆开注入:调度器只见密文与块号、不 import 加密包(§6.2)。
func (s *Sharer) BlockStore() session.BlockStore { return sharerStore{s} }

// Sealer 返回发送会话的加密面:chunk.Codec 直用(T1.5 衔接点)。全部发送
// 会话共享同一实例,Seal 计数熔断按分享全局累计(B12)。
func (s *Sharer) Sealer() session.Sealer { return s.codec }

// plaintext 按流几何拼出块 i 的明文:ChunkToRanges → 逐段 ReadRange,
// 段序即流序。逐段长度复核见 ErrShortRead。
func (s *Sharer) plaintext(i uint64) ([]byte, error) {
	ranges, err := s.lay.ChunkToRanges(i)
	if err != nil {
		return nil, err
	}
	var total int64
	for _, fr := range ranges {
		total += fr.N
	}
	buf := make([]byte, 0, total)
	for _, fr := range ranges {
		seg, err := s.src.ReadRange(fr.Path, fr.Off, fr.N)
		if err != nil {
			operation := fmt.Sprintf("read block %d at +%d (%d bytes) from", i, fr.Off, fr.N)
			return nil, wrapPathOperation(operation, fr.Path, err)
		}
		if int64(len(seg)) != fr.N {
			return nil, fmt.Errorf("%w: %s requested %d bytes, got %d", ErrShortRead, manifest.QuotePathForDiagnostic(fr.Path), fr.N, len(seg))
		}
		buf = append(buf, seg...)
	}
	return buf, nil
}

// sharerStore 把 Sharer 的按块明文投影成 session.BlockStore(§6.6 发送侧)。
type sharerStore struct{ s *Sharer }

func (b sharerStore) ReadBlock(index uint64) ([]byte, error) { return b.s.plaintext(index) }
func (b sharerStore) BlockCount() uint64                     { return b.s.NumChunks() }
