// TS 侧黄金向量读取骨架(执行计划 §7,T0.3),与 core/internal/testvec 对偶。
// 跨实现对拍以仓库 checkout 为准:vitest 在 node 环境下经 fs 读取仓库内
// testvectors/;信封格式与编码约定见 testvectors/README.md。cases 的 kind
// 特定字段保持 unknown,由各消费测试自行窄化。
import { readFileSync } from "node:fs";

export const ENVELOPE_VERSION = 1;

export interface VectorCase {
  name: string;
  [key: string]: unknown;
}

export interface VectorFile {
  version: number;
  kind: string;
  description?: string;
  cases: VectorCase[];
}

export function loadVectorFile(url: URL): VectorFile {
  const parsed: unknown = JSON.parse(readFileSync(url, "utf8"));
  if (typeof parsed !== "object" || parsed === null) {
    throw new Error(`向量文件 ${url}: 顶层须为对象`);
  }
  const f = parsed as Partial<VectorFile>;
  if (f.version !== ENVELOPE_VERSION) {
    throw new Error(
      `向量文件 ${url}: 信封版本 ${f.version} 不支持(本实现为 ${ENVELOPE_VERSION})`,
    );
  }
  if (typeof f.kind !== "string" || f.kind === "") {
    throw new Error(`向量文件 ${url}: 缺少 kind`);
  }
  if (!Array.isArray(f.cases) || f.cases.some((c) => typeof c?.name !== "string" || c.name === "")) {
    throw new Error(`向量文件 ${url}: cases 每项须有 name`);
  }
  return f as VectorFile;
}

export function b64ToBytes(b64: string): Uint8Array {
  return Uint8Array.from(Buffer.from(b64, "base64"));
}
