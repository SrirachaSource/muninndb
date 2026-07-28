package mcp

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// newVersionTestServer builds an MCPServer with no engine. /version and
// /mcp/health must not touch the engine — they answer while the database is
// degraded, which is exactly when an operator most needs to know what is
// running.
func newVersionTestServer() *MCPServer {
	return New(":0", nil, "", nil, nil, nil)
}

func doGet(t *testing.T, path string) (*http.Response, map[string]any) {
	t.Helper()
	s := newVersionTestServer()
	req := httptest.NewRequest(http.MethodGet, path, nil)
	rec := httptest.NewRecorder()
	s.srv.Handler.ServeHTTP(rec, req)
	res := rec.Result()
	var body map[string]any
	if err := json.NewDecoder(res.Body).Decode(&body); err != nil {
		t.Fatalf("GET %s: decoding body: %v", path, err)
	}
	return res, body
}

func assertBuildReceipt(t *testing.T, path string, b map[string]any) {
	t.Helper()
	for _, key := range []string{"version", "revision", "short_revision", "build_time", "modified", "go_version"} {
		v, ok := b[key]
		if !ok {
			t.Fatalf("GET %s: build receipt missing %q (got %v)", path, key, b)
		}
		// A blank string reads to an operator as "no data" while looking like
		// an answer. buildinfo reports "unknown" instead; assert that holds
		// across the wire.
		if s, isStr := v.(string); isStr && s == "" {
			t.Fatalf("GET %s: %q is empty; want a value or \"unknown\"", path, key)
		}
	}
}

func TestVersionEndpointServesBuildReceipt(t *testing.T) {
	res, body := doGet(t, "/version")
	if res.StatusCode != http.StatusOK {
		t.Fatalf("GET /version status = %d, want 200", res.StatusCode)
	}
	if ct := res.Header.Get("Content-Type"); ct != "application/json" {
		t.Fatalf("GET /version content-type = %q, want application/json", ct)
	}
	assertBuildReceipt(t, "/version", body)
}

// The deploy image publishes only the MCP port, so the receipt has to be
// reachable under the /mcp prefix too.
func TestMCPVersionEndpointServesBuildReceipt(t *testing.T) {
	res, body := doGet(t, "/mcp/version")
	if res.StatusCode != http.StatusOK {
		t.Fatalf("GET /mcp/version status = %d, want 200", res.StatusCode)
	}
	assertBuildReceipt(t, "/mcp/version", body)
}

// The version endpoints are deliberately unauthenticated: a box-watcher must
// be able to identify a running build without holding a vault credential.
func TestVersionEndpointsRequireNoAuth(t *testing.T) {
	for _, path := range []string{"/version", "/mcp/version", "/mcp/health"} {
		s := New(":0", nil, "mdb_secret_token", nil, nil, nil)
		req := httptest.NewRequest(http.MethodGet, path, nil) // no Authorization header
		rec := httptest.NewRecorder()
		s.srv.Handler.ServeHTTP(rec, req)
		if rec.Code != http.StatusOK {
			t.Fatalf("GET %s with auth configured = %d, want 200 (must not require auth)", path, rec.Code)
		}
	}
}

// A box-watcher may grep the raw health body rather than parse it. Adding the
// build receipt must not reorder what came before it, so the response still
// has to BEGIN with the original status field. (Encoding the payload as a map
// instead of a struct would sort "build" ahead of "status" and break exactly
// that caller.)
func TestHealthBodyStillStartsWithStatusOK(t *testing.T) {
	s := newVersionTestServer()
	req := httptest.NewRequest(http.MethodGet, "/mcp/health", nil)
	rec := httptest.NewRecorder()
	s.srv.Handler.ServeHTTP(rec, req)

	const wantPrefix = `{"status":"ok"`
	if got := rec.Body.String(); !strings.HasPrefix(got, wantPrefix) {
		t.Fatalf("health body = %s\nwant prefix %s (raw-substring pollers depend on it)", got, wantPrefix)
	}
}

func TestHealthKeepsStatusOKAndAddsBuild(t *testing.T) {
	res, body := doGet(t, "/mcp/health")
	if res.StatusCode != http.StatusOK {
		t.Fatalf("GET /mcp/health status = %d, want 200", res.StatusCode)
	}
	// Existing pollers match on this field; adding the receipt must not move it.
	if got := body["status"]; got != "ok" {
		t.Fatalf("health status = %v, want \"ok\" (existing pollers depend on it)", got)
	}
	build, ok := body["build"].(map[string]any)
	if !ok {
		t.Fatalf("health response missing build receipt: %v", body)
	}
	assertBuildReceipt(t, "/mcp/health", build)
}
