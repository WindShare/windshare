package manifest

import (
	"errors"
	"strings"
	"testing"
	"unicode"
	"unicode/utf8"

	"golang.org/x/text/unicode/norm"
)

func TestPathPolicyVersionPinsUnicodeTables(t *testing.T) {
	policy := CurrentPathPolicy()
	if policy.Version() != PathPolicyVersion {
		t.Fatalf("Version() = %q, want %q", policy.Version(), PathPolicyVersion)
	}
	if policy.UnicodeVersion() != PathPolicyUnicodeVersion {
		t.Fatalf("UnicodeVersion() = %q, want %q", policy.UnicodeVersion(), PathPolicyUnicodeVersion)
	}
	if norm.Version != PathPolicyUnicodeVersion || unicode.Version != PathPolicyUnicodeVersion {
		t.Fatalf("path policy pins Unicode %s, but x/text=%s and stdlib=%s; bump the policy version before updating tables",
			PathPolicyUnicodeVersion, norm.Version, unicode.Version)
	}
}

func TestCanonicalPath(t *testing.T) {
	tests := []struct {
		name string
		in   string
		want string // 空串 = 期望拒绝
	}{
		// NFC 归一是唯一允许的转换:macOS 产出的 NFD 文件名须落成 NFC。
		// 组合序列一律写显式 \u 转义,防源文件自身的归一化形态引入二义。
		{name: "NFD 转 NFC", in: "café.txt", want: "café.txt"},
		{name: "NFD 目录段转 NFC", in: "résumé/a", want: "résumé/a"},
		{name: "已 canonical 原样返回", in: "docs/café-文件.txt", want: "docs/café-文件.txt"},
		{name: "非法 UTF-8 拒绝", in: "a\xff\xfeb"},
		{name: "校验失败传播", in: "../escape"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := CanonicalPath(tt.in)
			if tt.want == "" {
				if !errors.Is(err, ErrInvalidPath) {
					t.Fatalf("want ErrInvalidPath, got %v(%q)", err, got)
				}
				return
			}
			if err != nil {
				t.Fatalf("expected success, got: %v", err)
			}
			if got != tt.want {
				t.Fatalf("got %q, want %q", got, tt.want)
			}
		})
	}
}

func TestValidatePath(t *testing.T) {
	tests := []struct {
		name string
		p    string
		ok   bool
	}{
		{name: "单段", p: "a", ok: true},
		{name: "多段", p: "a/b/c.txt", ok: true},
		{name: "Unicode NFC", p: "文件夹/图 片.png", ok: true},
		{name: "前导空格合法", p: " a", ok: true},
		{name: "隐藏文件", p: ".hidden", ok: true},
		{name: "多个点", p: "a.b.c", ok: true},
		{name: "保留名是前缀但非主干", p: "console.log", ok: true},
		{name: "com10 不在保留集", p: "com10", ok: true},
		{name: "子目录下 wsresume 名合法", p: "x/.wsresume-y", ok: true},

		{name: "空路径", p: ""},
		{name: "绝对路径", p: "/abs"},
		{name: "仅斜杠", p: "/"},
		{name: "连续斜杠空段", p: "a//b"},
		{name: "结尾斜杠空段", p: "a/"},
		{name: "当前目录段", p: "."},
		{name: "上级目录段", p: ".."},
		{name: "内嵌上级段", p: "a/../b"},
		{name: "内嵌当前段", p: "a/./b"},
		{name: "盘符冒号", p: "C:/x"},
		{name: "反斜杠", p: `a\b`},
		{name: "UNC 前缀", p: `\\server\share`},
		{name: "小于号", p: "a<b"},
		{name: "大于号", p: "a>b"},
		{name: "竖线", p: "a|b"},
		{name: "问号", p: "a?b"},
		{name: "星号", p: "a*b"},
		{name: "双引号", p: `a"b`},
		{name: "冒号 ADS", p: "a:b"},
		{name: "NUL 控制字符", p: "a\x00b"},
		{name: "C0 控制字符", p: "a\x1fb"},
		{name: "DEL 控制字符", p: "a\x7fb"},
		{name: "C1 控制字符 NEL", p: "ab"},
		{name: "结尾空格", p: "a "},
		{name: "结尾点", p: "a."},
		{name: "三点段", p: "..."},
		{name: "目录段结尾空格", p: "dir /f"},
		{name: "目录段结尾点", p: "seg./x"},
		{name: "非 NFC(NFD)", p: "café"},
		{name: "非法 UTF-8", p: "\xff"},
		{name: "journal 保留前缀本体", p: ".wsresume"},
		{name: "journal 保留前缀带指纹", p: ".wsresume-abc"},
		{name: "journal 保留前缀延展", p: ".wsresumefoo"},
		{name: "journal 保留前缀作目录", p: ".wsresume/x"},
		{name: "journal 保留前缀大小写折叠", p: ".WSRESUME-abc"},
		{name: "journal 保留前缀混合大小写", p: ".WsReSuMe/x"},
		{name: "journal 保留前缀 Unicode full fold", p: ".wſresume-journal"},
		{name: "Unicode RLO 格式控制", p: "photo\u202etxt.exe"},
		{name: "Unicode LRI 格式控制", p: "a\u2066b"},
		{name: "Unicode ZWJ 格式控制", p: "a\u200db"},
		{name: "Unicode BOM 格式控制", p: "a\ufeffb"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := ValidatePath(tt.p)
			if tt.ok && err != nil {
				t.Fatalf("expected valid path, got: %v", err)
			}
			if !tt.ok && !errors.Is(err, ErrInvalidPath) {
				t.Fatalf("want ErrInvalidPath, got %v", err)
			}
		})
	}
}

func TestHostilePathDiagnosticsStayBoundedUTF8(t *testing.T) {
	paths := []string{
		strings.Repeat("/", 1<<20),
		strings.Repeat("a", 1<<20) + "\x00",
		strings.Repeat("\x01", 1<<20),
		strings.Repeat("a", 1<<20) + "e\u0301",
	}
	for _, path := range paths {
		err := ValidatePath(path)
		if !errors.Is(err, ErrInvalidPath) {
			t.Fatalf("hostile path error = %v, want ErrInvalidPath", err)
		}
		if !utf8.ValidString(err.Error()) {
			t.Fatalf("hostile path diagnostic is not valid UTF-8")
		}
		if got, limit := len(err.Error()), 4*maxPathDiagnosticBytes; got > limit {
			t.Fatalf("hostile path diagnostic is %d bytes, limit %d", got, limit)
		}
	}
}

func TestQuotePathForDiagnosticBoundsEscapedOutput(t *testing.T) {
	for _, path := range []string{
		strings.Repeat("\x01", 1<<20),
		strings.Repeat("界", 1<<20/len("界")),
	} {
		got := QuotePathForDiagnostic(path)
		if len(got) > maxPathDiagnosticBytes {
			t.Fatalf("quoted diagnostic is %d bytes, limit %d", len(got), maxPathDiagnosticBytes)
		}
		if !utf8.ValidString(got) {
			t.Fatal("quoted diagnostic is not valid UTF-8")
		}
		if !strings.Contains(got, "…") {
			t.Fatalf("quoted diagnostic does not identify truncation: %q", got)
		}
	}
}

func TestWindowsReservedNames(t *testing.T) {
	reserved := []string{
		"con", "CON", "Con", "prn", "PRN", "aux", "AUX", "nul", "NUL",
		"com1", "COM5", "com9", "lpt1", "LPT5", "lpt9",
		"COM¹", "com²", "Com³", "LPT¹", "lpt²", "Lpt³", "CONIN$", "conout$",
		// 带扩展名与结尾空格主干:Win32 名字解析仍指向设备。
		"CON.txt", "con.tar.gz", "NUL.gz", "prn.a.b", "COM9.log", "CON .txt",
		"COM¹.txt", "LPT³.log", "CONIN$.txt", "CONOUT$.log",
	}
	for _, name := range reserved {
		if err := ValidatePath(name); !errors.Is(err, ErrInvalidPath) {
			t.Errorf("%q should be rejected as a reserved name, got %v", name, err)
		}
		if err := ValidatePath("dir/" + name); !errors.Is(err, ErrInvalidPath) {
			t.Errorf("dir/%q should be rejected as a reserved name, got %v", name, err)
		}
	}
	allowed := []string{
		"con0", "com0", "lpt0", "com10", "lpt10", "conx", "console",
		"aconsole.txt", "xcon", "prn2string", "nulled",
	}
	for _, name := range allowed {
		if err := ValidatePath(name); err != nil {
			t.Errorf("%q should not be rejected, got %v", name, err)
		}
	}
}

func TestFoldPath(t *testing.T) {
	// 折叠键近似 Windows/macOS 对"同一个名字"的判定:大小写与归一化形态归并。
	same := [][2]string{
		{"A.txt", "a.txt"},
		{"ẞ", "ss"}, // 大写 ẞ(U+1E9E)全折叠为 ss
		{"ẞ", "SS"},
		{"café", "CAFÉ"},
		{"Dir/File", "dir/file"},
	}
	for _, pair := range same {
		if foldPath(pair[0]) != foldPath(pair[1]) {
			t.Errorf("foldPath(%q) should equal foldPath(%q)", pair[0], pair[1])
		}
	}
	if foldPath("a") == foldPath("b") {
		t.Errorf("different names should not have equal folded keys")
	}

	policy := CurrentPathPolicy()
	tests := []struct {
		path string
		want string
	}{
		{path: "I", want: "i"},
		{path: "İ", want: "i\u0307"},
		{path: "ı", want: "ı"},
		{path: "ẞ", want: "ss"},
	}
	for _, tc := range tests {
		got, err := policy.CollisionKey(tc.path)
		if err != nil {
			t.Fatalf("CollisionKey(%q): %v", tc.path, err)
		}
		if got != tc.want {
			t.Errorf("CollisionKey(%q) = %q, want %q", tc.path, got, tc.want)
		}
	}
	if _, err := policy.CollisionKey("../escape"); !errors.Is(err, ErrInvalidPath) {
		t.Fatalf("CollisionKey must validate input: %v", err)
	}
}
