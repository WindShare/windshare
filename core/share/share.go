package share

import (
	"errors"
	"fmt"
	"io"

	"github.com/windshare/windshare/core/manifest"
)

// FileMeta 是构建分享的最小元数据快照(§6.6):出链接只 stat 不读内容,
// 门面据此建清单与流几何。Path 须已是清单 canonical 形态(/ 分隔、NFC,
// §6.13)——osfs.Walk 的产出即此形态,门面只校验不改写。
type FileMeta struct {
	Path  string
	Size  int64 // 字节数;MTime 为 Unix epoch 毫秒(§6.4)
	MTime int64
	IsDir bool
}

// FileSource 是 host 侧按需读(§6.6):门面把块号反查成文件字节段后逐段读取。
// 实现负责读后复核快照(size/mtime),漂移即返回错误(osfs.Source,§6.3)。
type FileSource interface {
	ReadRange(path string, off, n int64) ([]byte, error)
}

// FileSink 是 receiver 侧重建树/按段落盘(§6.6);落盘前路径校验在实现侧
// (osfs.Sink,§6.13)。
type FileSink interface {
	EnsureDir(path string) error
	WriteRange(path string, off int64, data []byte) error
	SetMTime(path string, mtime int64) error
}

// Options 配置一次分享(§6.6,B11)。零值即生产默认:随机 readSecret/shareId、
// 默认块大小、crypto/rand 随机源。
type Options struct {
	// ChunkSize must satisfy layout's power-of-two MinChunkSize/MaxChunkSize
	// contract; zero selects chunk.DefaultChunkSize.
	ChunkSize int64

	// ReadSecret 注入固定读密钥(16B;金标向量/测试);nil 则从 Rand 生成。
	// 它是链接中唯一秘密,整棵密钥树由它派生(§6.3)。
	ReadSecret []byte

	// ShareID 注入固定路由句柄(9B 的 base64url 无填充);空则从 Rand 生成。
	ShareID string

	// Rand 是分享的唯一随机源(B11):驱动 readSecret/shareId 的生成、清单
	// nonce 与每块 nonce——金标向量的确定性以此为命脉(§7)。nil 取
	// crypto/rand.Reader。消耗顺序固定:readSecret(16B,如未注入)→
	// shareId(9B,如未注入)→ 清单 nonce(12B,NewSharer 内 Seal-once)→
	// 逐块 nonce(12B/次,按 Seal 调用顺序)。
	Rand io.Reader
}

// 错误按类别提供 errors.Is 锚点(与兄弟包同规);路径选择器未命中复用
// layout.ErrUnknownPath,块号越界复用 layout.ErrChunkOutOfRange。
var (
	// ErrNilDependency:门面全部 IO 经接口注入,缺 src/dst 即无法运行;
	// 构造期失败比运行期 panic 诚实(DfT)。
	ErrNilDependency = errors.New("share: dependency must not be nil")

	// ErrBadOptions:Options 字段越出定义域(readSecret 长度、shareId 编码)。
	ErrBadOptions = errors.New("share: invalid options")

	// ErrBadLink:交给 NewReceiver 的链接结构不完整(readSecret 长度不符)。
	ErrBadLink = errors.New("share: invalid link")

	// ErrShortRead:FileSource 返回字节数与请求不符。静默短块会在接收端
	// 变成难以归因的 AEAD 失败,必须在发送侧指名拒绝。
	ErrShortRead = errors.New("share: FileSource returned a different byte count than requested")

	// ErrBlockLength:块明文长度与流几何不符。AEAD 只认证"密文出自密钥
	// 持有者",不约束明文长度——持链者可 Seal 出错长块,落盘会静默错位,
	// 几何一致性在接收门面收口(§6.5)。
	ErrBlockLength = errors.New("share: block plaintext length does not match stream geometry")

	// ErrMissingBlocks:收尾物化要求选中内容覆盖的块全部到位——mtime 恢复
	// 之后再有写入会把它冲掉,完成条件 = 所选块全通过且物化完成(§6.6)。
	ErrMissingBlocks = errors.New("share: selected blocks are incomplete; cannot finalize materialization")

	// ErrChunkNotSelected prevents callers from mutating have-state with a block that
	// is outside the immutable transfer plan.
	ErrChunkNotSelected = errors.New("share: chunk is outside the transfer plan")
)

// pathOperationError keeps dependency failures available to errors.Is/As without
// rendering their Error text. FileSource/FileSink are injection boundaries, so a
// caller-provided error can otherwise re-expand a bounded hostile path or expose
// sensitive implementation details in the session terminal.
type pathOperationError struct {
	operation string
	path      string
	cause     error
}

func (e *pathOperationError) Error() string {
	return fmt.Sprintf("share: %s %s: operation failed", e.operation, manifest.QuotePathForDiagnostic(e.path))
}

func (e *pathOperationError) Unwrap() error { return e.cause }

func wrapPathOperation(operation, path string, cause error) error {
	return &pathOperationError{operation: operation, path: path, cause: cause}
}
