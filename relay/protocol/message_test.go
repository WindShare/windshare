package protocol

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"strings"
	"testing"
)

// 测试用合法样例:12 字符 base64url shareId(9B 句柄的线形态)与固定 token。
var (
	tShareID   = "AAAAAAAAAAAA"
	tToken     = bytes.Repeat([]byte{0x42}, ResumeTokenBytes)
	tTokenB64  = EncodeResumeToken(tToken)
	tTokenHash = HashResumeToken(tToken)
	tSessionID = SessionID{1, 2, 3, 4, 5, 6, 7, 8}
)

func TestEncodeDecodeRoundTrip(t *testing.T) {
	msgs := []Message{
		NewRegister(tShareID, tTokenHash),
		NewResumeRegister(tShareID, tTokenHash, tTokenB64),
		NewRegistered(tShareID),
		NewKeepalive(),
		NewJoin(tShareID),
		NewManifest(tSessionID.String()),
		NewNotFound(),
		NewSignal(tSessionID.String(), SignalKindOffer, json.RawMessage(`{"sdp":"v=0"}`)),
		NewBye(tSessionID.String()),
		NewError(ErrCodeRateLimited, "joins too fast"),
		NewSessionError(tSessionID.String(), ErrCodeSessionOverflow, "queue full"),
	}
	for _, m := range msgs {
		data, err := Encode(m)
		if err != nil {
			t.Fatalf("Encode(%T): %v", m, err)
		}
		got, err := Decode(data)
		if err != nil {
			t.Fatalf("Decode(%T): %v", m, err)
		}
		gotJSON, _ := json.Marshal(got)
		wantJSON, _ := json.Marshal(m)
		if !bytes.Equal(gotJSON, wantJSON) {
			t.Errorf("round-trip mismatch for %T:\n got %s\nwant %s", m, gotJSON, wantJSON)
		}
	}
}

func TestDecodeRejects(t *testing.T) {
	cases := []struct {
		name string
		data string
	}{
		{"非 JSON", `{{{`},
		{"未知类型", `{"type":"teleport"}`},
		{"空类型", `{"type":""}`},
		{"register 缺 shareId", `{"type":"register","resumeTokenHash":"` + tTokenHash + `"}`},
		{"register 坏 hash 长度", `{"type":"register","shareId":"` + tShareID + `","resumeTokenHash":"AAAA"}`},
		{"register 坏 hash 编码", `{"type":"register","shareId":"` + tShareID + `","resumeTokenHash":"!!!"}`},
		{"register hash 非规范尾位", `{"type":"register","shareId":"` + tShareID + `","resumeTokenHash":"` + nonCanonicalBase64URLAlias(tTokenHash) + `"}`},
		{"register 坏 token 原像", `{"type":"register","shareId":"` + tShareID + `","resumeTokenHash":"` + tTokenHash + `","resumeToken":"AA"}`},
		{"register token 原像非规范尾位", `{"type":"register","shareId":"` + tShareID + `","resumeTokenHash":"` + tTokenHash + `","resumeToken":"` + nonCanonicalBase64URLAlias(tTokenB64) + `"}`},
		{"join shareId 非法字符", `{"type":"join","shareId":"a/b"}`},
		{"join shareId 超长", `{"type":"join","shareId":"` + strings.Repeat("a", MaxShareIDChars+1) + `"}`},
		{"manifest 坏 sessionId", `{"type":"manifest","sessionId":"short"}`},
		{"signal 缺 kind", `{"type":"signal","sessionId":"` + tSessionID.String() + `","payload":{}}`},
		{"signal 缺 payload", `{"type":"signal","sessionId":"` + tSessionID.String() + `","kind":"offer"}`},
		{"signal 缺 sessionId", `{"type":"signal","kind":"offer","payload":{}}`},
		{"signal 非标量 kind", `{"type":"signal","sessionId":"` + tSessionID.String() + `","kind":"\ud800","payload":{}}`},
		{"bye 缺 sessionId", `{"type":"bye"}`},
		{"error 缺 code", `{"type":"error","message":"x"}`},
		{"error 坏 sessionId", `{"type":"error","code":"x","sessionId":"@@"}`},
		{"error 空 message", `{"type":"error","code":"x","message":""}`},
		{"error null sessionId", `{"type":"error","code":"x","sessionId":null}`},
		{"字段名大小写别名", `{"Type":"join","ShareId":"` + tShareID + `"}`},
		{"register 空 resumeToken", `{"type":"register","shareId":"` + tShareID + `","resumeTokenHash":"` + tTokenHash + `","resumeToken":""}`},
	}
	for _, c := range cases {
		if _, err := Decode([]byte(c.data)); err == nil {
			t.Errorf("%s: expected rejection", c.name)
		}
	}
}

func TestDecodeRejectsInvalidUTF8(t *testing.T) {
	wire := append([]byte(`{"type":"error","code":"`), 0xff)
	wire = append(wire, []byte(`"}`)...)
	if _, err := Decode(wire); err == nil {
		t.Fatal("invalid UTF-8 signaling should be rejected before JSON normalization")
	}
}

func TestDecodeRejectsOversize(t *testing.T) {
	big := `{"type":"keepalive","pad":"` + strings.Repeat("x", MaxSignalingMessageBytes) + `"}`
	if _, err := Decode([]byte(big)); err == nil {
		t.Fatal("oversize signaling message should be rejected")
	}
}

func TestSignalingJSONDepthContract(t *testing.T) {
	payloadAtLimit := nestedJSONValue(MaxSignalingJSONDepth - 1)
	payloadAboveLimit := nestedJSONValue(MaxSignalingJSONDepth)
	prefix := `{"type":"signal","sessionId":"` + tSessionID.String() + `","kind":"offer","payload":`

	if _, err := Decode([]byte(prefix + payloadAtLimit + `}`)); err != nil {
		t.Fatalf("decode signaling at depth limit: %v", err)
	}
	if _, err := Decode([]byte(prefix + payloadAboveLimit + `}`)); err == nil {
		t.Fatal("signaling above the structural depth limit was accepted")
	}
	if _, err := Encode(NewSignal(tSessionID.String(), SignalKindOffer, json.RawMessage(payloadAtLimit))); err != nil {
		t.Fatalf("encode signaling at depth limit: %v", err)
	}
	if _, err := Encode(NewSignal(tSessionID.String(), SignalKindOffer, json.RawMessage(payloadAboveLimit))); err == nil {
		t.Fatal("encode accepted a payload above the structural depth limit")
	}

	unknownAtLimit := `{"type":"keepalive","future":` + payloadAtLimit + `}`
	if _, err := Decode([]byte(unknownAtLimit)); err != nil {
		t.Fatalf("decode unknown field at depth limit: %v", err)
	}
	unknownAboveLimit := `{"type":"keepalive","future":` + payloadAboveLimit + `}`
	if _, err := Decode([]byte(unknownAboveLimit)); err == nil {
		t.Fatal("unknown field bypassed the signaling depth limit")
	}
}

func nestedJSONValue(depth int) string {
	return strings.Repeat("[", depth) + "null" + strings.Repeat("]", depth)
}

func TestEncodeRejectsInvalid(t *testing.T) {
	if _, err := Encode(nil); err == nil {
		t.Error("nil signaling message should be rejected")
	}
	var nilJoin *Join
	if _, err := Encode(nilJoin); err == nil {
		t.Error("typed nil signaling message should be rejected")
	}
	if _, err := Encode(&Join{Type: TypeJoin, ShareID: ""}); err == nil {
		t.Error("empty shareId should be rejected")
	}
	// Type 标签被手工改错:Encode 须拦下,防止绕过构造函数造出错标签消息。
	if _, err := Encode(&Join{Type: TypeBye, ShareID: tShareID}); err == nil {
		t.Error("Type tag mismatch should be rejected")
	}
	if _, err := Encode(NewSignal(tSessionID.String(), SignalKindOffer, json.RawMessage(`nope`))); err == nil {
		t.Error("invalid JSON payload should be rejected")
	}
	oversize := json.RawMessage(`"` + strings.Repeat("x", MaxSignalingMessageBytes) + `"`)
	if _, err := Encode(NewSignal(tSessionID.String(), SignalKindOffer, oversize)); err == nil {
		t.Error("oversize signaling should be rejected on encode")
	}
}

func TestEncodeMatchesJavaScriptStringEscaping(t *testing.T) {
	message := NewError("<>&", "\u2028\u2029"+`\u2028`)
	encoded, err := Encode(message)
	if err != nil {
		t.Fatal(err)
	}
	want := `{"type":"error","code":"<>&","message":"` + "\u2028\u2029" + `\\u2028"}`
	if string(encoded) != want {
		t.Fatalf("encoded signaling = %q, want %q", encoded, want)
	}
}

func TestEncodeAppliesLimitToJavaScriptCompatibleWireBytes(t *testing.T) {
	const prefix = `{"type":"error","code":"`
	const suffix = `"}`
	code := strings.Repeat("<", MaxSignalingMessageBytes-len(prefix)-len(suffix))
	encoded, err := Encode(NewError(code, ""))
	if err != nil {
		t.Fatalf("encode exact signaling limit: %v", err)
	}
	if len(encoded) != MaxSignalingMessageBytes {
		t.Fatalf("encoded size = %d, want %d", len(encoded), MaxSignalingMessageBytes)
	}
	if _, err := Encode(NewError(code+"<", "")); err == nil {
		t.Fatal("signaling message above the JavaScript-compatible wire limit was accepted")
	}
}

func TestResumeTokenHelpers(t *testing.T) {
	if !VerifyResumeToken(tTokenB64, tTokenHash) {
		t.Error("correct token preimage should pass")
	}
	other := EncodeResumeToken(bytes.Repeat([]byte{0x43}, ResumeTokenBytes))
	if VerifyResumeToken(other, tTokenHash) {
		t.Error("wrong token preimage should fail")
	}
	if VerifyResumeToken("!!!", tTokenHash) {
		t.Error("invalid encoded preimage should fail")
	}
	if VerifyResumeToken(tTokenB64, "AAAA") {
		t.Error("invalid hash should fail")
	}
	if VerifyResumeToken(nonCanonicalBase64URLAlias(tTokenB64), tTokenHash) {
		t.Error("non-canonical token preimage should fail")
	}
	if VerifyResumeToken(tTokenB64, nonCanonicalBase64URLAlias(tTokenHash)) {
		t.Error("non-canonical token hash should fail")
	}
	// KAT:固定 token 的哈希是确定值,钉住线格式(base64url 无填充)。
	if strings.ContainsAny(tTokenHash, "+/=") {
		t.Errorf("wire hash should be unpadded base64url, got %q", tTokenHash)
	}
	raw, err := base64.RawURLEncoding.DecodeString(tTokenHash)
	if err != nil || len(raw) != 32 {
		t.Errorf("hash should decode to 32 bytes, err=%v len=%d", err, len(raw))
	}
}

func nonCanonicalBase64URLAlias(encoded string) string {
	const alphabet = "ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789-_"
	last := strings.IndexByte(alphabet, encoded[len(encoded)-1])
	// Both fixed fields leave trailing pad bits (16B -> four, 32B -> two).
	// Canonical encoders zero them, so setting the low bit preserves decoded bytes.
	return encoded[:len(encoded)-1] + string(alphabet[last+1])
}

func TestValidateShareID(t *testing.T) {
	for _, ok := range []string{"a", "AZaz09-_", strings.Repeat("x", MaxShareIDChars)} {
		if err := ValidateShareID(ok); err != nil {
			t.Errorf("%q should be valid: %v", ok, err)
		}
	}
	for _, bad := range []string{"", "a b", "a+b", "a=b", "汉", strings.Repeat("x", MaxShareIDChars+1)} {
		if err := ValidateShareID(bad); err == nil {
			t.Errorf("%q should be invalid", bad)
		}
	}
}
