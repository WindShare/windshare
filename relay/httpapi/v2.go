package httpapi

import (
	"net/http"

	"github.com/coder/websocket"

	"github.com/windshare/windshare/relay/signaling/v2endpoint"
)

// ConnectionAdmission keeps the v2 endpoint independent of protocol and route
// policy. The daemon injects its listener-wide connection authority.
type ConnectionAdmission func(source string) (release func(), allowed bool)

type V2Config struct {
	Server          *v2endpoint.Server
	AllowedOrigins  []string
	AllowLocalhost  bool
	SourceIdentity  func(*http.Request) string
	AdmitConnection ConnectionAdmission
}

func ValidateV2Config(config V2Config) error {
	if config.Server == nil || config.AdmitConnection == nil {
		return v2endpoint.ErrConfig
	}
	_, err := normalizeOrigins(config.AllowedOrigins)
	return err
}

// NewV2Handler exposes the share-independent /v2/ws route.
func NewV2Handler(config V2Config) http.Handler {
	if config.SourceIdentity == nil {
		config.SourceIdentity = remoteIP
	}
	if err := ValidateV2Config(config); err != nil {
		panic(err)
	}
	allowed, _ := normalizeOrigins(config.AllowedOrigins)
	mux := http.NewServeMux()
	mux.HandleFunc("GET /v2/ws", func(writer http.ResponseWriter, request *http.Request) {
		origins := request.Header.Values("Origin")
		if len(origins) > 1 || len(origins) == 1 && !originAllowed(origins[0], allowed, config.AllowLocalhost) {
			http.Error(writer, "origin is not allowed", http.StatusForbidden)
			return
		}
		release, admitted := config.AdmitConnection(config.SourceIdentity(request))
		if !admitted {
			http.Error(writer, "relay connection capacity exceeded", http.StatusServiceUnavailable)
			return
		}
		defer release()
		connection, err := websocket.Accept(writer, request, &websocket.AcceptOptions{InsecureSkipVerify: true})
		if err != nil {
			return
		}
		_ = config.Server.Serve(request.Context(), connection)
	})
	return mux
}
