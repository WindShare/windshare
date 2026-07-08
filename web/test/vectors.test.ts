// 与 core/internal/testvec 消费同一份样例文件,双端共同锁定信封 schema(T0.3)。
import { describe, expect, it } from "vitest";
import { b64ToBytes, loadVectorFile } from "./vectors";

const sampleURL = new URL("../../testvectors/envelope-sample.json", import.meta.url);

describe("黄金向量信封", () => {
  it("解析 envelope-sample 并解码 base64 字节串", () => {
    const f = loadVectorFile(sampleURL);
    expect(f.kind).toBe("envelope-sample");
    expect(f.cases).toHaveLength(2);
    expect(f.cases[0].name).toBe("hello");
    expect(new TextDecoder().decode(b64ToBytes(f.cases[0].bytesB64 as string))).toBe("hello");
    expect(b64ToBytes(f.cases[1].bytesB64 as string)).toHaveLength(0);
  });
});
