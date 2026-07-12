package osfs

import (
	"errors"
	"fmt"
	"io/fs"
	"os"

	"github.com/windshare/windshare/core/manifest"
)

// Source 按快照实现 share 契约的 FileSource(§6.6):只有快照内的路径可读,
// 每次读后复核,消除"检查与使用"之间的空窗(§6.3 漂移处理)。
type Source struct {
	snap *Snapshot
}

func NewSource(snap *Snapshot) *Source { return &Source{snap: snap} }

// ReadRange 读 path 的 [off, off+n)。次序固定为:打开 → 身份/元数据复核 → 读取
// → 同句柄复核 → 交付。预读身份复核阻止路径被替换为同 size/mtime 的另一对象;
// 读后复核仍用同一文件句柄,关闭读取期间的路径调包空窗。同一 inode 内同 size、
// 同 mtime 的原地改写是文档化残余,非安全事件(§6.3)。
func (s *Source) ReadRange(path string, off, n int64) ([]byte, error) {
	ref, ok := s.snap.files[path]
	if !ok {
		// 快照映射即白名单:未知/穿越/绝对路径全都不在映射内,无需另做
		// 形态校验即天然限制在快照根内。
		return nil, categorizedPathFailure("osfs: locate snapshot path", path, ErrNotInSnapshot, nil)
	}
	// off > ref.size-n 的写法对 off+n 溢出安全(off、n 均已非负)。
	if off < 0 || n < 0 || off > ref.size-n {
		return nil, fmt.Errorf("%w: %s off=%d n=%d size=%d", ErrOutOfRange, manifest.QuotePathForDiagnostic(path), off, n, ref.size)
	}
	f, err := os.Open(ref.osPath)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil, categorizedPathFailure("osfs: open source", path, ErrDrift, err)
		}
		return nil, pathFailure("osfs: open source", ref.osPath, err)
	}
	defer f.Close()
	opened, err := f.Stat()
	if err != nil {
		return nil, pathFailure("osfs: inspect opened source", ref.osPath, err)
	}
	if !matchesSnapshot(ref, opened) {
		return nil, sourceDriftError(path, ref, opened)
	}
	buf := make([]byte, n)
	_, readErr := f.ReadAt(buf, off)
	fi, err := f.Stat()
	if err != nil {
		return nil, pathFailure("osfs: verify source metadata for", ref.osPath, err)
	}
	// 漂移判定先于短读报错:文件被截短导致的 ReadAt EOF,根因是漂移,
	// 报 ErrDrift 才指向正确的处置(重新分享)。
	if !matchesSnapshot(ref, fi) {
		return nil, sourceDriftError(path, ref, fi)
	}
	if readErr != nil {
		return nil, pathFailure("osfs: read source", ref.osPath, readErr)
	}
	return buf, nil
}

func matchesSnapshot(ref fileRef, current os.FileInfo) bool {
	return current.Mode().IsRegular() && os.SameFile(ref.identity, current) &&
		current.Size() == ref.size && current.ModTime().UnixMilli() == ref.mtime
}

func sourceDriftError(path string, ref fileRef, current os.FileInfo) error {
	return fmt.Errorf("%w: %s source identity or metadata changed (snapshot size=%d mtime=%d, current size=%d mtime=%d)",
		ErrDrift, manifest.QuotePathForDiagnostic(path), ref.size, ref.mtime, current.Size(), current.ModTime().UnixMilli())
}
