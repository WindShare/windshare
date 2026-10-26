package sessionobs

import (
	"github.com/windshare/windshare/core/session/protocolsession"
	"reflect"
	"testing"
	"time"
)

func TestProtocolErrorContentValidatesOnlyWireMeaning(t *testing.T) {
	base := ProtocolErrorContentSpec{WireScope: ProtocolErrorRevision, WireCode: 0x3008}
	value, err := NewProtocolErrorContent(base)
	if err != nil || value.IsZero() || value.WireScope() != base.WireScope || value.WireCode() != base.WireCode || value.Retryable() {
		t.Fatalf("value=%+v err=%v", value, err)
	}
	for name, change := range map[string]func(*ProtocolErrorContentSpec){
		"scope":           func(s *ProtocolErrorContentSpec) { s.WireScope = 255 },
		"retry presence":  func(s *ProtocolErrorContentSpec) { s.Retryable = true },
		"unclaimed retry": func(s *ProtocolErrorContentSpec) { s.RetryAfterMillis = 1 },
		"retry minimum":   func(s *ProtocolErrorContentSpec) { s.Retryable = true; s.HasRetryAfter = true },
		"retry maximum": func(s *ProtocolErrorContentSpec) {
			s.Retryable = true
			s.HasRetryAfter = true
			s.RetryAfterMillis = 30001
		},
	} {
		t.Run(name, func(t *testing.T) {
			spec := base
			change(&spec)
			if _, err := NewProtocolErrorContent(spec); err == nil {
				t.Fatal("invalid content accepted")
			}
		})
	}
	for _, duration := range []time.Duration{protocolsession.MinOperationFailureRetryAfter, protocolsession.MaxOperationFailureRetryAfter} {
		body, err := protocolsession.EncodeOperationFailure(protocolsession.OperationFailure{Scope: protocolsession.OperationScopeRevision, Code: base.WireCode, Retryable: true, RetryAfter: duration, Message: "provider details"})
		if err != nil {
			t.Fatal(err)
		}
		content := ProtocolErrorForResponse(protocolsession.MessageOperationError, body)
		retry, present := content.RetryAfterMillis()
		if content.IsZero() || !present || retry != uint32(duration.Milliseconds()) {
			t.Fatalf("content=%+v", content)
		}
	}
	if !ProtocolErrorForResponse(protocolsession.MessageOperationError, []byte("invalid")).IsZero() ||
		!ProtocolErrorForResponse(protocolsession.MessageOperationComplete, nil).IsZero() {
		t.Fatal("unverified content exposed")
	}
}

func TestProtocolErrorContentRetainsNoContextOrAuthority(t *testing.T) {
	for _, value := range []any{ProtocolErrorContent{}, ProtocolErrorContentSpec{}} {
		for field := range reflect.TypeOf(value).Fields() {
			switch field.Type.Kind() {
			case reflect.String, reflect.Slice, reflect.Interface, reflect.Pointer:
				t.Fatalf("unbounded field %v", field)
			}
		}
	}
}
