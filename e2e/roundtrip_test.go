package e2e

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/windshare/windshare/core/link"
	"github.com/windshare/windshare/internal/testnetwork"
)

// e2eBlockSize 用小块让几 KiB 的树也有多块可切分/选择/续传;字符串形态直接
// 喂 --block-size。
const e2eBlockSize = "4096"

const e2eBlockSizeInt = 4096

// getTimeout 是单个 get 进程的完成上限;只防挂死。
const getTimeout = 60 * time.Second

// richTree 铺一棵覆盖各形态的树:多级目录、非 ASCII(NFC)名、1 字节 / 整块
// 边界 / 跨块 / 多块文件、空文件、空目录。内容以确定性字节填充,便于哈希比对。
func richTree(t *testing.T) treeSpec {
	t.Helper()
	fill := func(n, seed int) []byte {
		b := make([]byte, n)
		for i := range b {
			b[i] = byte(i*31 + seed)
		}
		return b
	}
	return treeSpec{
		files: map[string][]byte{
			"readme.txt":        []byte("hello windshare e2e\n"),
			"one_byte.bin":      {0x5a},                         // 1 字节
			"exact_block.bin":   fill(e2eBlockSizeInt, 1),       // 恰一整块
			"cross_block.bin":   fill(e2eBlockSizeInt+1, 2),     // 跨块 1 字节
			"multi_block.bin":   fill(3*e2eBlockSizeInt+123, 3), // 多块 + 半块尾
			"empty.txt":         {},                             // 空文件
			"sub/nested.bin":    fill(e2eBlockSizeInt-1, 4),     // 整块差 1
			"sub/deep/leaf.txt": []byte("deep leaf\n"),
			"目录/文件.txt":         []byte("非 ASCII 目录与文件名\n"),     // NFC 中文名
			"café/naïve.txt":    []byte("accented NFC names\n"), // café/naïve(NFC)
		},
		dirs: []string{"sub/emptydir"},
	}
}

// shareTreeGetFull 是最常用编排:起 relay+share,取链接,起一个 get 全量下载到
// 新临时目录,返回输出根(<out>/tree)。
func shareTreeGetFull(t *testing.T, spec treeSpec, shareExtra ...string) (srcRoot, outTreeRoot string) {
	t.Helper()
	relayURL, _ := startRelay(t)
	srcRoot = writeTree(t, spec)
	extra := append([]string{"--block-size", e2eBlockSize}, shareExtra...)
	sp := startShare(t, relayURL, []string{srcRoot}, extra...)
	full := sp.waitLine(t, "Link: ", procIOTimeout)
	if _, err := link.Parse(full); err != nil {
		t.Fatalf("share printed an unparseable link: %v (%q)", err, full)
	}
	out := t.TempDir()
	code, _, stderr := runGet(t, getTimeout, full, "-o", out)
	if code != 0 {
		t.Fatalf("get exit code %d, want 0; stderr=%s", code, stderr)
	}
	sp.kill()
	return srcRoot, filepath.Join(out, "tree")
}

// TestRoundTripFullTree:完整往返,SHA-256 全树一致,含多级目录、非 ASCII NFC
// 名、1 字节/整块边界/跨块文件、空文件与空目录物化(§6.11)。
func TestRoundTripFullTree(t *testing.T) {
	testnetwork.RequireOSNetwork(t)
	t.Parallel()
	spec := richTree(t)
	srcRoot, outTree := shareTreeGetFull(t, spec)
	assertTreeEqual(t, srcRoot, outTree)
	// 空文件与空目录不占流,只在收尾物化,单独确认其存在。
	if fi, err := os.Stat(filepath.Join(outTree, "empty.txt")); err != nil || fi.Size() != 0 {
		t.Errorf("empty file empty.txt was not materialized correctly: %v", err)
	}
	assertDirExists(t, filepath.Join(outTree, "sub", "emptydir"))
	assertDirExists(t, filepath.Join(outTree, "目录"))
}

// TestSelectiveOnly:--only 只落所选文件。选中 multi_block.bin(占块 2..5)与
// sub/deep 子树;校验所选逐字节一致,且任何未选中条目都不落盘。共享边界块
// 仍会完整认证,但 TransferPlan 只物化选中 ranges。
func TestSelectiveOnly(t *testing.T) {
	testnetwork.RequireOSNetwork(t)
	t.Parallel()
	relayURL, _ := startRelay(t)
	spec := richTree(t)
	srcRoot := writeTree(t, spec)
	sp := startShare(t, relayURL, []string{srcRoot}, "--block-size", e2eBlockSize)
	full := sp.waitLine(t, "Link: ", procIOTimeout)

	out := t.TempDir()
	code, _, stderr := runGet(t, getTimeout, full, "-o", out,
		"--only", "tree/multi_block.bin", "--only", "tree/sub/deep")
	if code != 0 {
		t.Fatalf("get --only exit code %d; stderr=%s", code, stderr)
	}
	sp.kill()

	outTree := filepath.Join(out, "tree")
	src := hashFiles(t, srcRoot)
	got := hashFiles(t, outTree)
	// 选中的应逐字节一致。
	for _, rel := range []string{"multi_block.bin", "sub/deep/leaf.txt"} {
		if got[rel] == "" || got[rel] != src[rel] {
			t.Errorf("selected file %q is missing or has wrong content", rel)
		}
	}
	// 与所选无任何块重叠的条目不得落盘(可靠保证:它们的块从未被请求)。
	for _, absent := range []string{"café/naïve.txt", "cross_block.bin", "empty.txt", "sub/emptydir", "目录/文件.txt"} {
		if _, err := os.Stat(filepath.Join(outTree, filepath.FromSlash(absent))); err == nil {
			t.Errorf("entry %q with no selected block overlap should not be materialized", absent)
		}
	}
}

// TestSelectiveBoundaryOverfetchDoesNotMaterializeSiblings pins the distinction
// between authenticated overfetch and selected-path materialization.
func TestSelectiveBoundaryOverfetchDoesNotMaterializeSiblings(t *testing.T) {
	testnetwork.RequireOSNetwork(t)
	t.Parallel()
	relayURL, _ := startRelay(t)
	srcRoot := writeTree(t, richTree(t))
	sp := startShare(t, relayURL, []string{srcRoot}, "--block-size", e2eBlockSize)
	full := sp.waitLine(t, "Link: ", procIOTimeout)

	out := t.TempDir()
	code, _, stderr := runGet(t, getTimeout, full, "-o", out, "--only", "tree/multi_block.bin")
	if code != 0 {
		t.Fatalf("get --only exit code %d; stderr=%s", code, stderr)
	}
	sp.kill()

	outTree := filepath.Join(out, "tree")
	// 只应存在 multi_block.bin;任何其他文件的出现即边界过取泄漏。
	var leaked []string
	for _, sibling := range []string{"exact_block.bin", "one_byte.bin", "readme.txt", "sub/deep/leaf.txt", "sub/nested.bin"} {
		if _, err := os.Stat(filepath.Join(outTree, filepath.FromSlash(sibling))); err == nil {
			leaked = append(leaked, sibling)
		}
	}
	if len(leaked) > 0 {
		t.Fatalf("boundary overfetch materialized unselected siblings: %v", leaked)
	}
}

// TestOnlyUnknownRejected:--only 指向清单中不存在的路径 → 用户错误退出(§6.9)。
func TestOnlyUnknownRejected(t *testing.T) {
	t.Parallel()
	relayURL, _ := startRelay(t)
	srcRoot := writeTree(t, richTree(t))
	sp := startShare(t, relayURL, []string{srcRoot}, "--block-size", e2eBlockSize)
	full := sp.waitLine(t, "Link: ", procIOTimeout)

	out := t.TempDir()
	code, _, stderr := runGet(t, getTimeout, full, "-o", out, "--only", "tree/nope.txt")
	if code != 2 {
		t.Fatalf("unknown --only path should exit with 2 (usage error), got %d; stderr=%s", code, stderr)
	}
	if !strings.Contains(stderr, "not present") {
		t.Errorf("error message is unclear: %s", stderr)
	}
}

// TestOutputExistsRejected:非续传时输出目录已有同名文件 → get 明确拒绝,
// 不静默覆盖用户数据(§6.13)。
func TestOutputExistsRejected(t *testing.T) {
	testnetwork.RequireOSNetwork(t)
	t.Parallel()
	relayURL, _ := startRelay(t)
	spec := treeSpec{files: map[string][]byte{"payload.bin": []byte("original share content")}}
	srcRoot := writeTree(t, spec)
	sp := startShare(t, relayURL, []string{srcRoot}, "--block-size", e2eBlockSize)
	full := sp.waitLine(t, "Link: ", procIOTimeout)

	out := t.TempDir()
	// 预置同名文件(注意输出路径含顶层 tree/)。
	preexist := filepath.Join(out, "tree", "payload.bin")
	if err := os.MkdirAll(filepath.Dir(preexist), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(preexist, []byte("pre-existing user data"), 0o644); err != nil {
		t.Fatal(err)
	}

	code, _, stderr := runGet(t, getTimeout, full, "-o", out)
	if code != 2 {
		t.Fatalf("existing path should exit with 2, got %d; stderr=%s", code, stderr)
	}
	// 用户数据未被覆盖。
	got, err := os.ReadFile(preexist)
	if err != nil || string(got) != "pre-existing user data" {
		t.Errorf("existing file was modified: %q (%v)", got, err)
	}
}

// TestSplitKey:--split-key 输出裸链接 + 密钥串;接收端经 --key 补入完成往返。
func TestSplitKey(t *testing.T) {
	testnetwork.RequireOSNetwork(t)
	t.Parallel()
	relayURL, _ := startRelay(t)
	spec := treeSpec{files: map[string][]byte{"secret.txt": []byte("split key payload")}}
	srcRoot := writeTree(t, spec)
	sp := startShare(t, relayURL, []string{srcRoot}, "--block-size", e2eBlockSize, "--split-key")
	bare := sp.waitLine(t, "Bare link: ", procIOTimeout)
	key := sp.waitLine(t, "Key: ", procIOTimeout)
	if strings.Contains(bare, "#") {
		t.Fatalf("bare link should not contain a fragment: %q", bare)
	}

	out := t.TempDir()
	code, _, stderr := runGet(t, getTimeout, bare, "--key", key, "-o", out)
	if code != 0 {
		t.Fatalf("get --key exit code %d; stderr=%s", code, stderr)
	}
	sp.kill()
	assertTreeEqual(t, srcRoot, filepath.Join(out, "tree"))
}
