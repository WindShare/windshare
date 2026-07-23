package httpapi

import (
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"

	"github.com/windshare/windshare/relay/signaling/v2endpoint"
)

func TestV2HandlerRejectsBeforeUpgradeAndReleasesAdmission(t *testing.T) {
	if ValidateV2Config(V2Config{}) == nil {
		t.Fatal("invalid v2 configuration was accepted")
	}
	server := new(v2endpoint.Server)
	if err := ValidateV2Config(V2Config{
		Server: server, AllowedOrigins: []string{"://bad"},
		AdmitConnection: func(string) (func(), bool) { return func() {}, true },
	}); err == nil {
		t.Fatal("invalid v2 origin was accepted")
	}
	var admissions atomic.Int32
	var releases atomic.Int32
	var source string
	allowed := NewV2Handler(V2Config{
		Server: server, AllowedOrigins: []string{"https://app.example"},
		AdmitConnection: func(value string) (func(), bool) {
			source = value
			admissions.Add(1)
			return func() { releases.Add(1) }, true
		},
	})

	request := httptest.NewRequest(http.MethodGet, "http://relay.example/v2/ws", nil)
	request.RemoteAddr = "192.0.2.7:1234"
	request.Header.Set("Origin", "https://evil.example")
	response := httptest.NewRecorder()
	allowed.ServeHTTP(response, request)
	if response.Code != http.StatusForbidden || admissions.Load() != 0 {
		t.Fatalf("origin rejection = %d admissions=%d", response.Code, admissions.Load())
	}

	request = httptest.NewRequest(http.MethodGet, "http://relay.example/v2/ws", nil)
	request.RemoteAddr = "192.0.2.7:1234"
	request.Header.Set("Origin", "https://app.example")
	response = httptest.NewRecorder()
	allowed.ServeHTTP(response, request)
	if admissions.Load() != 1 || releases.Load() != 1 || source != "192.0.2.7" {
		t.Fatalf("upgrade admission/release/source = %d/%d/%q", admissions.Load(), releases.Load(), source)
	}

	for _, test := range []struct {
		method string
		path   string
		status int
	}{
		{http.MethodPost, "/v2/ws", http.StatusMethodNotAllowed},
		{http.MethodGet, "/v2/ws/extra", http.StatusNotFound},
	} {
		response = httptest.NewRecorder()
		allowed.ServeHTTP(response, httptest.NewRequest(test.method, "http://relay.example"+test.path, nil))
		if response.Code != test.status {
			t.Fatalf("%s %s = %d", test.method, test.path, response.Code)
		}
	}
}

func TestV2HandlerAdmission(t *testing.T) {
	server := new(v2endpoint.Server)
	denied := NewV2Handler(V2Config{
		Server:          server,
		AdmitConnection: func(string) (func(), bool) { return func() {}, false },
	})
	response := httptest.NewRecorder()
	denied.ServeHTTP(response, httptest.NewRequest(http.MethodGet, "http://relay.example/v2/ws", nil))
	if response.Code != http.StatusServiceUnavailable {
		t.Fatalf("admission rejection = %d", response.Code)
	}
}
