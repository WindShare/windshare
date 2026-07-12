package osfs

import (
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"

	"github.com/windshare/windshare/core/manifest"
)

// FileMeta 与 share 契约的 FileMeta(§6.6)字段同名同型同序——share 落地后
// 可整体转换(Go 允许忽略 tag 的同构 struct conversion),osfs 不 import share,
// 维持"接口在消费侧定义、实现在 IO 边缘"的依赖方向。
type FileMeta struct {
	Path  string // 清单 canonical 相对路径:/ 分隔、NFC(§6.13)
	Size  int64
	MTime int64 // Unix epoch 毫秒(§6.4)
	IsDir bool
}

// SkippedEntry 记录被遍历跳过的对象,供上层告警展示——跳过必须可观测,
// 否则"分享到的内容比预期少"对用户毫无线索(§6.13)。
type SkippedEntry struct {
	Path   string // OS 绝对路径(定位问题用,非清单相对路径)
	Reason string
}

// 跳过原因(具名,便于上层分类展示)。
const (
	// SkipReasonReparsePoint:符号链接与一切 reparse point(junction 等)
	// 不跟随、不打包——跟随会引入越权包含与环路(§6.13)。
	SkipReasonReparsePoint = "symbolic link or reparse point; not followed"
	// SkipReasonIrregular:设备/socket/FIFO 等无法按字节流读取的对象。
	SkipReasonIrregular = "irregular file cannot be read as a byte stream"
)

// fileRef 是快照对磁盘的定位与复核基线:osPath 保留磁盘原始形态(如 NFD 名)
// 以便打开,canonical 形态只进清单;identity 防止同元数据对象替换,size/mtime
// 检测所选对象的原地漂移(§6.3)。
type fileRef struct {
	osPath   string
	size     int64
	mtime    int64
	identity fs.FileInfo
}

// Snapshot 是一次 Walk 的结果:Entries 供构建清单,files 供 Source 定位与
// 漂移复核。Source 只认快照——快照之外的路径一律不可读。
type Snapshot struct {
	Entries []FileMeta
	Skipped []SkippedEntry

	files map[string]fileRef
}

// Walk 对 roots(文件与目录可混合)做元数据快照:只 stat 不读内容,出链接
// 耗时因此与内容体积无关(§6.4)。目录根以自身 basename 作顶层条目(分享
// 文件夹 = 对端收到同名文件夹);无法安全表达的路径报错而非静默改写(§6.13)。
func Walk(roots []string) (*Snapshot, error) {
	snap := &Snapshot{files: make(map[string]fileRef)}
	seen := make(map[string]string)
	for _, root := range roots {
		abs, err := filepath.Abs(root)
		if err != nil {
			return nil, pathFailure("osfs: resolve source root", root, err)
		}
		info, err := os.Lstat(abs)
		if err != nil {
			return nil, filesystemPathFailure("osfs: access source root", root, err)
		}
		switch {
		case isReparsePoint(info):
			snap.Skipped = append(snap.Skipped, SkippedEntry{Path: abs, Reason: SkipReasonReparsePoint})
		case info.IsDir():
			if err := snap.walkDir(abs, info, seen); err != nil {
				return nil, err
			}
		case info.Mode().IsRegular():
			stable, err := snapshotRegularFile(abs, info, func() (*os.File, error) {
				return os.Open(abs)
			})
			if err != nil {
				return nil, err
			}
			if err := snap.add(abs, filepath.Base(abs), stable, seen); err != nil {
				return nil, err
			}
		default:
			snap.Skipped = append(snap.Skipped, SkippedEntry{Path: abs, Reason: SkipReasonIrregular})
		}
	}
	// 排序只为快照结果确定可复现(多根间序与遍历序解耦);打包流的规范序
	// 职责在 core/layout(§6.4),不在此。
	sort.Slice(snap.Entries, func(i, j int) bool { return snap.Entries[i].Path < snap.Entries[j].Path })
	return snap, nil
}

func (s *Snapshot) walkDir(rootAbs string, expected fs.FileInfo, seen map[string]string) error {
	root, err := os.OpenRoot(rootAbs)
	if err != nil {
		return pathFailure("osfs: open source directory", rootAbs, err)
	}
	rootInfo, err := root.Stat(".")
	if err != nil {
		_ = root.Close()
		return pathFailure("osfs: inspect source directory", rootAbs, err)
	}
	if !rootInfo.IsDir() || !os.SameFile(expected, rootInfo) {
		_ = root.Close()
		return fmt.Errorf("%w: source directory %s changed while opening it", ErrDrift, manifest.QuotePathForDiagnostic(rootAbs))
	}

	base := filepath.Base(rootAbs)
	walkErr := fs.WalkDir(root.FS(), ".", func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return pathFailure("osfs: walk source entry", filepath.Join(rootAbs, filepath.FromSlash(p)), err)
		}

		info := rootInfo
		osPath := rootAbs
		rel := base
		if p != "." {
			info, err = d.Info()
			if err != nil {
				return pathFailure("osfs: read source metadata for", filepath.Join(rootAbs, filepath.FromSlash(p)), err)
			}
			osPath = filepath.Join(rootAbs, filepath.FromSlash(p))
			rel = base + "/" + p
		}

		if isReparsePoint(info) {
			s.Skipped = append(s.Skipped, SkippedEntry{Path: osPath, Reason: SkipReasonReparsePoint})
			// junction 在部分 Go 版本下仍报告为目录:显式 SkipDir 保证
			// 无论哪种报告形态都不下潜。
			if d.IsDir() || info.IsDir() {
				return filepath.SkipDir
			}
			return nil
		}
		if d.IsDir() != info.IsDir() {
			return fmt.Errorf("%w: source path %s changed type during traversal", ErrDrift, manifest.QuotePathForDiagnostic(osPath))
		}
		if !info.IsDir() && !info.Mode().IsRegular() {
			s.Skipped = append(s.Skipped, SkippedEntry{Path: osPath, Reason: SkipReasonIrregular})
			return nil
		}
		if info.Mode().IsRegular() {
			info, err = snapshotRegularFile(osPath, info, func() (*os.File, error) {
				return root.Open(p)
			})
			if err != nil {
				return err
			}
		}
		return s.add(osPath, rel, info, seen)
	})
	closeErr := root.Close()
	if walkErr != nil {
		return walkErr
	}
	if closeErr != nil {
		return pathFailure("osfs: close source directory", rootAbs, closeErr)
	}
	return nil
}

// snapshotRegularFile opens without reading content and binds the snapshot to
// the selected filesystem object. The identity check closes the lstat/open race
// that could otherwise replace a selected file with a symlink or hard link to
// unrelated content having the same size and timestamp.
func snapshotRegularFile(path string, expected fs.FileInfo, open func() (*os.File, error)) (fs.FileInfo, error) {
	file, err := open()
	if err != nil {
		return nil, pathFailure("osfs: open source for snapshot", path, err)
	}
	actual, statErr := file.Stat()
	closeErr := file.Close()
	if statErr != nil {
		return nil, pathFailure("osfs: inspect source for snapshot", path, statErr)
	}
	if closeErr != nil {
		return nil, pathFailure("osfs: close source after snapshot", path, closeErr)
	}
	if !actual.Mode().IsRegular() || !os.SameFile(expected, actual) ||
		actual.Size() != expected.Size() || actual.ModTime().UnixMilli() != expected.ModTime().UnixMilli() {
		return nil, fmt.Errorf("%w: source file %s changed while opening it", ErrDrift, manifest.QuotePathForDiagnostic(path))
	}
	return actual, nil
}

// add 录入一个磁盘对象:相对路径经 manifest.CanonicalPath 归一(NFC),发送端
// 与接收端由此共用同一套 canonical 规则(§6.13,B7)。
func (s *Snapshot) add(osPath, relSlash string, info fs.FileInfo, seen map[string]string) error {
	canonical, err := manifest.CanonicalPath(relSlash)
	if err != nil {
		return fmt.Errorf("osfs: %s cannot be represented safely: %w", manifest.QuotePathForDiagnostic(osPath), err)
	}
	key, err := manifest.CurrentPathPolicy().CollisionKey(canonical)
	if err != nil {
		return fmt.Errorf("osfs: derive path identity for %s: %w", manifest.QuotePathForDiagnostic(osPath), err)
	}
	if first, ok := seen[key]; ok {
		if first == canonical {
			return fmt.Errorf("%w: %s (source %s)", manifest.ErrDuplicatePath, manifest.QuotePathForDiagnostic(canonical), manifest.QuotePathForDiagnostic(osPath))
		}
		return fmt.Errorf("%w: %s aliases %s (source %s)", manifest.ErrPathCollision, manifest.QuotePathForDiagnostic(canonical), manifest.QuotePathForDiagnostic(first), manifest.QuotePathForDiagnostic(osPath))
	}
	seen[key] = canonical
	m := FileMeta{Path: canonical, MTime: info.ModTime().UnixMilli(), IsDir: info.IsDir()}
	if !m.IsDir {
		// 目录 size 因平台而异(NTFS 0、ext4 4096…)且不占流,统一记 0。
		m.Size = info.Size()
		s.files[canonical] = fileRef{osPath: osPath, size: m.Size, mtime: m.MTime, identity: info}
	}
	s.Entries = append(s.Entries, m)
	return nil
}
