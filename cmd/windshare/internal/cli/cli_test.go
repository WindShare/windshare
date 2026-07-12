package cli

import (
	"bytes"
	"context"
	"encoding/base64"
	"errors"
	"flag"
	"fmt"
	"io"
	"strings"
	"sync"
	"testing"

	"github.com/windshare/windshare/connectivity"
	"github.com/windshare/windshare/core/link"
	"github.com/windshare/windshare/core/osfs"
	"github.com/windshare/windshare/core/session"
)

// newTestApp 组装注入缓冲的 App;stdin 内容由各用例按需传入。
func newTestApp(stdin string) (*App, *bytes.Buffer, *bytes.Buffer) {
	var out, errBuf bytes.Buffer
	return &App{Stdout: &out, Stderr: &errBuf, Stdin: strings.NewReader(stdin)}, &out, &errBuf
}

func TestAppSerializesConcurrentDiagnostics(t *testing.T) {
	app, _, stderr := newTestApp("")
	const (
		writers = 16
		lines   = 100
	)
	var group sync.WaitGroup
	for range writers {
		group.Go(func() {
			for range lines {
				app.logf("peer path diagnostic")
			}
		})
	}
	group.Wait()
	if got, want := strings.Count(stderr.String(), "\n"), writers*lines; got != want {
		t.Fatalf("diagnostic lines = %d, want %d", got, want)
	}
}

// testLink 造一条结构合法的能力链接(确定字节,便于比对)。
func testLink() link.Link {
	secret := make([]byte, link.ReadSecretBytes)
	for i := range secret {
		secret[i] = byte(i + 1)
	}
	id := make([]byte, link.ShareIDBytes)
	for i := range id {
		id[i] = byte(0xa0 + i)
	}
	return link.Link{
		Suite:      link.SuiteAESGCM,
		ReadSecret: secret,
		ShareID:    base64.RawURLEncoding.EncodeToString(id),
	}
}

func TestRunDispatch(t *testing.T) {
	cases := []struct {
		name string
		args []string
		want int
	}{
		{"无参数", nil, ExitUsage},
		{"未知子命令", []string{"frobnicate"}, ExitUsage},
		{"help", []string{"help"}, ExitOK},
		{"share 缺路径", []string{"share"}, ExitUsage},
		{"share 路径不存在", []string{"share", "no-such-path-xyz"}, ExitUsage},
		{"get 缺链接", []string{"get"}, ExitUsage},
		{"get 多余参数", []string{"get", "a", "b"}, ExitUsage},
		{"get 链接格式非法", []string{"get", "not-a-link"}, ExitUsage},
		{"get 未知 flag", []string{"get", "--bogus"}, ExitUsage},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			app, _, _ := newTestApp("")
			if got := app.Run(context.Background(), tc.args); got != tc.want {
				t.Errorf("Run(%v) = %d, want %d", tc.args, got, tc.want)
			}
		})
	}
}

func TestShareRejectsBadBlockSize(t *testing.T) {
	app, _, errBuf := newTestApp("")
	dir := t.TempDir()
	writeFileT(t, dir, "a.txt", []byte("x"))
	// 块大小非 2 的幂在本地构建期即失败,不触网。
	if got := app.Run(context.Background(), []string{"share", dir, "--block-size", "3"}); got != ExitUsage {
		t.Fatalf("invalid block size should return usage exit code %d, got %d; stderr=%s", ExitUsage, got, errBuf.String())
	}
}

func TestGetRequiresRelayHint(t *testing.T) {
	full, err := testLink().URL(DefaultFrontURL)
	if err != nil {
		t.Fatal(err)
	}
	app, _, errBuf := newTestApp("")
	if got := app.Run(context.Background(), []string{"get", full}); got != ExitUsage {
		t.Fatalf("link without ?r= should return a usage error, got %d", got)
	}
	if !strings.Contains(errBuf.String(), "?r=") {
		t.Errorf("error should identify the missing relay address, got: %s", errBuf.String())
	}
}

// TestResolveLinkMatrix 覆盖 §6.9/§6.10 的链接与分离密钥合并矩阵。
func TestResolveLinkMatrix(t *testing.T) {
	l := testLink()
	l.Relays = []string{"ws://127.0.0.1:8484"}
	full, err := l.URL(DefaultFrontURL)
	if err != nil {
		t.Fatal(err)
	}
	bare, key, err := l.SplitURL(DefaultFrontURL)
	if err != nil {
		t.Fatal(err)
	}
	wrongKey, err := link.Link{Suite: link.SuiteAESGCM, ReadSecret: make([]byte, link.ReadSecretBytes)}.KeyString()
	if err != nil {
		t.Fatal(err)
	}

	cases := []struct {
		name    string
		raw     string
		key     string
		stdin   string
		wantErr bool
	}{
		{name: "完整链接", raw: full},
		{name: "裸链接+key", raw: bare, key: key},
		{name: "裸链接+交互输入", raw: bare, stdin: key + "\n"},
		{name: "裸链接+带#的交互输入", raw: bare, stdin: "#" + key + "\n"},
		{name: "完整链接+一致 key", raw: full, key: key},
		{name: "完整链接+冲突 key", raw: full, key: wrongKey, wantErr: true},
		{name: "裸链接无 key 无输入", raw: bare, stdin: "\n", wantErr: true},
		{name: "裸链接 stdin EOF", raw: bare, stdin: "", wantErr: true},
		{name: "坏密钥串", raw: bare, key: "!!!", wantErr: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			app, _, _ := newTestApp(tc.stdin)
			got, err := app.resolveLink(tc.raw, tc.key)
			if tc.wantErr {
				if err == nil {
					t.Fatal("expected error, got success")
				}
				return
			}
			if err != nil {
				t.Fatalf("resolveLink: %v", err)
			}
			if !bytes.Equal(got.ReadSecret, l.ReadSecret) || got.ShareID != l.ShareID {
				t.Errorf("merged result mismatch: %+v", got)
			}
			if len(got.Relays) == 0 || got.Relays[0] != l.Relays[0] {
				t.Errorf("relay hint was lost: %v", got.Relays)
			}
		})
	}
}

func TestParseInterleaved(t *testing.T) {
	fs := flag.NewFlagSet("t", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	x := fs.String("x", "", "")
	pos, err := parseInterleaved(fs, []string{"a", "-x", "1", "b", "c"})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Join(pos, ",") != "a,b,c" || *x != "1" {
		t.Errorf("pos=%v x=%q", pos, *x)
	}
}

func TestReportGetErr(t *testing.T) {
	cases := []struct {
		name string
		err  error
		want int
	}{
		{"发送端漂移中止", &session.Error{Code: session.ErrCodeBlockRead, Msg: "drift"}, ExitDrift},
		{"发送端熔断", fmt.Errorf("w: %w", &session.Error{Code: session.ErrCodeSeal, Msg: "fuse"}), ExitFailure},
		{"用户中断", context.Canceled, ExitFailure},
		{"重连失败", fmt.Errorf("%w:x", connectivity.ErrRelayRecoveryFailed), ExitNetwork},
		{"块重试耗尽", fmt.Errorf("w: %w", session.ErrBlockExhausted), ExitNetwork},
		{"同名文件拒绝覆盖", fmt.Errorf("w: %w", osfs.ErrAlreadyExists), ExitUsage},
		{"其他", errors.New("boom"), ExitFailure},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			app, _, _ := newTestApp("")
			if got := app.reportGetErr(tc.err); got != tc.want {
				t.Errorf("reportGetErr(%v) = %d, want %d", tc.err, got, tc.want)
			}
		})
	}
}
