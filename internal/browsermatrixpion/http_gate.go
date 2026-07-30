package browsermatrixpion

import (
	"net/http"
	"net/url"
	"strings"
)

type exactRequestShape struct {
	method  string
	hasBody bool
}

func (service *Service) exactHTTPRequestStatus(request *http.Request) (int, string) {
	if request == nil || request.URL == nil || request.TLS == nil ||
		request.URL.Scheme != "" || request.URL.Host != "" || request.URL.User != nil ||
		request.URL.RawPath != "" || request.URL.RawQuery != "" || request.URL.ForceQuery ||
		request.URL.Fragment != "" || request.URL.RawFragment != "" ||
		request.RequestURI != request.URL.Path || len(request.TransferEncoding) != 0 ||
		len(request.Header.Values("Transfer-Encoding")) != 0 ||
		len(request.Header.Values("Content-Encoding")) != 0 {
		return http.StatusBadRequest, "malformed-request"
	}
	origin, err := url.Parse(service.fixture.ControllerOrigin)
	if err != nil || request.Host != origin.Host {
		return http.StatusBadRequest, "authority-origin-mismatch"
	}
	shape, known := exactPionRequestShape(request.URL.Path)
	if !known {
		return http.StatusNotFound, "not-found"
	}
	if shape.method != "" && request.Method != shape.method {
		return http.StatusMethodNotAllowed, "method-not-allowed"
	}
	if shape.hasBody {
		if request.ContentLength <= 0 || request.ContentLength > maximumBodyBytes ||
			!exactPionHeader(request.Header, "Content-Type", "application/json") {
			return http.StatusBadRequest, "malformed-request"
		}
	} else if request.ContentLength != 0 || len(request.Header.Values("Content-Type")) != 0 {
		return http.StatusBadRequest, "malformed-request"
	}
	return 0, ""
}

func exactPionRequestShape(path string) (exactRequestShape, bool) {
	switch path {
	case authorityProbePath, probePath, turnCredentialPath, attemptsPath:
		return exactRequestShape{method: http.MethodPost, hasBody: true}, true
	}
	if !strings.HasPrefix(path, attemptsPath+"/") {
		return exactRequestShape{}, false
	}
	parts := strings.Split(strings.TrimPrefix(path, attemptsPath+"/"), "/")
	if len(parts) == 2 && validOpaqueID(parts[0]) && parts[1] == "offer" {
		return exactRequestShape{method: http.MethodPost, hasBody: true}, true
	}
	if len(parts) != 1 || !validOpaqueID(parts[0]) {
		return exactRequestShape{}, false
	}
	// Read and delete share a target. The method is resolved explicitly here so
	// malformed alternatives are rejected before bearer authentication.
	return exactRequestShape{}, true
}

func (service *Service) exactAttemptRequestStatus(request *http.Request) (int, string) {
	shape, known := exactPionRequestShape(request.URL.Path)
	if !known || shape.method != "" {
		return 0, ""
	}
	if request.Method != http.MethodGet && request.Method != http.MethodDelete {
		return http.StatusMethodNotAllowed, "method-not-allowed"
	}
	if request.ContentLength != 0 || len(request.Header.Values("Content-Type")) != 0 {
		return http.StatusBadRequest, "malformed-request"
	}
	return 0, ""
}

func exactPionHeader(header http.Header, name string, expected string) bool {
	values := header.Values(name)
	return len(values) == 1 && values[0] == expected
}
