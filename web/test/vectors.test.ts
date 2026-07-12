// 与 core/internal/testvec 消费同一份样例文件,双端共同锁定信封 schema(T0.3)。
import { describe, expect, it } from "vitest";
import { b64ToBytes, loadVectorFile } from "./vectors";

const sampleURL = new URL("../../testvectors/envelope-sample.json", import.meta.url);

describe("golden vector envelope", () => {
  it("parses envelope-sample and decodes its base64 bytes", () => {
    const f = loadVectorFile(sampleURL);
    expect(f.kind).toBe("envelope-sample");
    expect(f.cases).toHaveLength(2);
    const [hello, empty] = f.cases;
    if (hello === undefined || empty === undefined) {
      throw new Error("envelope sample must contain exactly two cases");
    }
    expect(hello.name).toBe("hello");
    expect(new TextDecoder().decode(b64ToBytes(hello.bytesB64 as string))).toBe("hello");
    expect(b64ToBytes(empty.bytesB64 as string)).toHaveLength(0);
  });
});
