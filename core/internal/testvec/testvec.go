// Package testvec reads the canonical cross-runtime fixtures in core/testvectors.
//
// Keeping the sole vector authority inside the core module makes released module
// tests self-contained while Go and TypeScript still consume the exact same bytes.
// Callers provide paths because the envelope parser intentionally knows nothing
// about kind-specific schemas.
package testvec

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
)

// EnvelopeVersion 是信封格式版本(与线协议版本、suiteByte 无关)。
const EnvelopeVersion = 1

var (
	ErrUnsupportedEnvelopeVersion = errors.New("testvec: unsupported envelope version")
	ErrMissingKind                = errors.New("testvec: envelope is missing kind")
	ErrMissingCaseName            = errors.New("testvec: case is missing name")
)

// File 是一个黄金向量文件的通用信封;Kind 决定 Cases 的具体 schema。
type File struct {
	Version     int    `json:"version"`
	Kind        string `json:"kind"`
	Description string `json:"description"`
	Cases       []Case `json:"cases"`
}

// Case 提取公共的用例名,其余 kind 特定字段保留为原始 JSON 由消费方解码。
type Case struct {
	Name string
	raw  json.RawMessage
}

func (c *Case) UnmarshalJSON(data []byte) error {
	var head struct {
		Name string `json:"name"`
	}
	if err := json.Unmarshal(data, &head); err != nil {
		return err
	}
	if head.Name == "" {
		return ErrMissingCaseName
	}
	c.Name = head.Name
	c.raw = append(json.RawMessage(nil), data...)
	return nil
}

// Decode 把用例的完整 JSON(含 name)解码进 kind 特定结构。
func (c *Case) Decode(v any) error {
	return json.Unmarshal(c.raw, v)
}

// Load 读取并校验一个向量文件的信封;kind 特定内容不在此校验。
func Load(path string) (*File, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var f File
	if err := json.Unmarshal(data, &f); err != nil {
		return nil, fmt.Errorf("parse vector file %s: %w", path, err)
	}
	if f.Version != EnvelopeVersion {
		return nil, fmt.Errorf("%w: vector file %s has version %d, implementation version %d", ErrUnsupportedEnvelopeVersion, path, f.Version, EnvelopeVersion)
	}
	if f.Kind == "" {
		return nil, fmt.Errorf("%w: vector file %s", ErrMissingKind, path)
	}
	return &f, nil
}
