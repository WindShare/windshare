package testvec

import (
	"encoding/base64"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
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
		name       string
		content    string
		wantErr    error
		wantSyntax bool
	}{
		{"unknown envelope version", `{"version": 99, "kind": "x", "cases": []}`, ErrUnsupportedEnvelopeVersion, false},
		{"missing kind", `{"version": 1, "cases": []}`, ErrMissingKind, false},
		{"case missing name", `{"version": 1, "kind": "x", "cases": [{"foo": 1}]}`, ErrMissingCaseName, false},
		{"invalid JSON", `{`, nil, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "vec.json")
			if err := os.WriteFile(path, []byte(tc.content), 0o644); err != nil {
				t.Fatal(err)
			}
			_, err := Load(path)
			if tc.wantErr != nil && !errors.Is(err, tc.wantErr) {
				t.Fatalf("Load err = %v, want errors.Is %v", err, tc.wantErr)
			}
			if tc.wantSyntax {
				if _, ok := errors.AsType[*json.SyntaxError](err); !ok {
					t.Fatalf("Load err = %v, want *json.SyntaxError", err)
				}
			}
		})
	}
}

func TestLoadMissingFile(t *testing.T) {
	if _, err := Load(filepath.Join(t.TempDir(), "missing.json")); err == nil {
		t.Fatal("Load should fail for a nonexistent file")
	}
}
