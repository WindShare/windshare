package testvec

import (
	"encoding/base64"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// 与 web/test/vectors.test.ts 消费同一份样例文件,双端共同锁定信封 schema。
const sampleRelPath = "../../../testvectors/envelope-sample.json"

func TestLoadEnvelopeSample(t *testing.T) {
	f, err := Load(sampleRelPath)
	if err != nil {
		t.Fatalf("Load(%s): %v", sampleRelPath, err)
	}
	if f.Kind != "envelope-sample" {
		t.Fatalf("kind = %q, want envelope-sample", f.Kind)
	}
	if len(f.Cases) != 2 {
		t.Fatalf("len(cases) = %d, want 2", len(f.Cases))
	}
	if f.Cases[0].Name != "hello" {
		t.Fatalf("cases[0].name = %q, want hello", f.Cases[0].Name)
	}

	var body struct {
		BytesB64 string `json:"bytesB64"`
	}
	if err := f.Cases[0].Decode(&body); err != nil {
		t.Fatalf("Decode: %v", err)
	}
	raw, err := base64.StdEncoding.DecodeString(body.BytesB64)
	if err != nil {
		t.Fatalf("base64: %v", err)
	}
	if string(raw) != "hello" {
		t.Fatalf("bytes = %q, want hello", raw)
	}
}

func TestLoadRejectsBadEnvelope(t *testing.T) {
	cases := []struct {
		name    string
		content string
		wantErr string
	}{
		{"未知信封版本", `{"version": 99, "kind": "x", "cases": []}`, "信封版本"},
		{"缺少 kind", `{"version": 1, "cases": []}`, "缺少 kind"},
		{"用例缺少 name", `{"version": 1, "kind": "x", "cases": [{"foo": 1}]}`, "缺少 name"},
		{"非法 JSON", `{`, "解析向量文件"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "vec.json")
			if err := os.WriteFile(path, []byte(tc.content), 0o644); err != nil {
				t.Fatal(err)
			}
			_, err := Load(path)
			if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
				t.Fatalf("Load err = %v, want含 %q", err, tc.wantErr)
			}
		})
	}
}

func TestLoadMissingFile(t *testing.T) {
	if _, err := Load(filepath.Join(t.TempDir(), "不存在.json")); err == nil {
		t.Fatal("Load 对不存在的文件应报错")
	}
}
