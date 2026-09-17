package httpserver

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestHealthEndpointsAndNoBusinessRoute(t *testing.T) {
	handler := Handler()
	for _, path := range []string{"/health/live", "/health/ready"} {
		request := httptest.NewRequest(http.MethodGet, path, nil)
		response := httptest.NewRecorder()
		handler.ServeHTTP(response, request)
		if response.Code != http.StatusOK {
			t.Fatalf("%s returned %d", path, response.Code)
		}
		var body map[string]any
		if err := json.Unmarshal(response.Body.Bytes(), &body); err != nil {
			t.Fatalf("%s did not return JSON: %v", path, err)
		}
		if body["status"] == "" {
			t.Fatalf("%s response has no status", path)
		}
	}
	request := httptest.NewRequest(http.MethodPost, "/health/live", nil)
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != http.StatusMethodNotAllowed {
		t.Fatalf("POST health returned %d, want 405", response.Code)
	}
	request = httptest.NewRequest(http.MethodPost, "/sync", nil)
	response = httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != http.StatusNotFound {
		t.Fatalf("unimplemented sync route returned %d, want 404", response.Code)
	}
}

func TestReadinessDoesNotClaimProductionSync(t *testing.T) {
	request := httptest.NewRequest(http.MethodGet, "/health/ready", nil)
	response := httptest.NewRecorder()
	Handler().ServeHTTP(response, request)
	var body struct {
		Capabilities map[string]bool `json:"capabilities"`
	}
	if err := json.Unmarshal(response.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if body.Capabilities["production_sync"] || body.Capabilities["mcp"] || body.Capabilities["oauth"] {
		t.Fatalf("readiness overstates capability: %#v", body.Capabilities)
	}
}
