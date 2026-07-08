// Package testvec 读取仓库 testvectors/ 下的黄金向量文件(执行计划 §7,T0.3)。
//
// 跨实现对拍以仓库 checkout 为准:core 的模块 zip 不含 testvectors/,故本包
// 不内置路径,只按调用方(core 各包的测试)传入的相对路径读取。信封格式与
// 编码约定见 testvectors/README.md;cases 保持 json.RawMessage,由消费测试
// 按 kind 解成具体结构——信封解析器不理解具体 kind,新增向量类别无需改动本包。
package testvec

import (
	"encoding/json"
	"fmt"
	"os"
)

// EnvelopeVersion 是信封格式版本(与线协议版本、suiteByte 无关)。
const EnvelopeVersion = 1

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
		return fmt.Errorf("向量用例缺少 name 字段")
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
		return nil, fmt.Errorf("解析向量文件 %s: %w", path, err)
	}
	if f.Version != EnvelopeVersion {
		return nil, fmt.Errorf("向量文件 %s: 信封版本 %d 不支持(本实现为 %d)", path, f.Version, EnvelopeVersion)
	}
	if f.Kind == "" {
		return nil, fmt.Errorf("向量文件 %s: 缺少 kind", path)
	}
	return &f, nil
}
