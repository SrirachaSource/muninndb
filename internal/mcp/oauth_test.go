package mcp

import (
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/scrypster/muninndb/internal/auth"
)

func init() {
	// Zero out the brute-force delay so tests don't sleep unnecessarily.
	rateLimitDelay = 0
}

// fakeOAuthStore implements oauthClientValidator for tests.
type fakeOAuthStore struct {
	clients map[string]auth.OAuthClient // keyed by clientID
	secrets map[string]string           // clientID → plaintext secret
}

func newFakeOAuthStore() *fakeOAuthStore {
	return &fakeOAuthStore{
		clients: make(map[string]auth.OAuthClient),
		secrets: make(map[string]string),
	}
}

func (s *fakeOAuthStore) addClient(clientID, clientSecret, name, mcpToken string) {
	s.clients[clientID] = auth.OAuthClient{
		ID:       clientID,
		Name:     name,
		MCPToken: mcpToken,
	}
	s.secrets[clientID] = clientSecret
}

func (s *fakeOAuthStore) ValidateOAuthClient(clientID, clientSecret string) (auth.OAuthClient, error) {
	client, ok := s.clients[clientID]
	if !ok {
		return auth.OAuthClient{}, auth.ErrOAuthClientNotFound
	}
	if s.secrets[clientID] != clientSecret {
		return auth.OAuthClient{}, auth.ErrOAuthInvalidCredentials
	}
	return client, nil
}

// ── Token endpoint tests ────────────────────────────────────────────────

func TestOAuthToken_ValidClientSecretPost(t *testing.T) {
	store := newFakeOAuthStore()
	store.addClient("oc_test", "secret123", "test-client", "mdb_mytoken")

	srv := New(":0", &fakeEngine{}, "mdb_mytoken", nil, store, nil)

	body := "grant_type=client_credentials&client_id=oc_test&client_secret=secret123"
	req := httptest.NewRequest("POST", "/mcp/oauth/token", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	w := httptest.NewRecorder()
	srv.srv.Handler.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}

	var resp oauthTokenResponse
	if err := json.NewDecoder(w.Body).Decode(&resp); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if resp.AccessToken != "mdb_mytoken" {
		t.Errorf("expected access_token 'mdb_mytoken', got %q", resp.AccessToken)
	}
	if resp.TokenType != "bearer" {
		t.Errorf("expected token_type 'bearer', got %q", resp.TokenType)
	}
	if resp.ExpiresIn <= 0 {
		t.Errorf("expected positive expires_in, got %d", resp.ExpiresIn)
	}
	// Cache-Control must be no-store per RFC 6749.
	if cc := w.Header().Get("Cache-Control"); cc != "no-store" {
		t.Errorf("expected Cache-Control 'no-store', got %q", cc)
	}
}

func TestOAuthToken_ValidClientSecretBasic(t *testing.T) {
	store := newFakeOAuthStore()
	store.addClient("oc_basic", "basicsecret", "basic-client", "mdb_basictoken")

	srv := New(":0", &fakeEngine{}, "mdb_basictoken", nil, store, nil)

	body := "grant_type=client_credentials"
	req := httptest.NewRequest("POST", "/mcp/oauth/token", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	creds := base64.StdEncoding.EncodeToString([]byte("oc_basic:basicsecret"))
	req.Header.Set("Authorization", "Basic "+creds)
	w := httptest.NewRecorder()
	srv.srv.Handler.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}

	var resp oauthTokenResponse
	json.NewDecoder(w.Body).Decode(&resp)
	if resp.AccessToken != "mdb_basictoken" {
		t.Errorf("expected access_token 'mdb_basictoken', got %q", resp.AccessToken)
	}
}

func TestOAuthToken_InvalidSecret(t *testing.T) {
	store := newFakeOAuthStore()
	store.addClient("oc_test", "correct", "test-client", "mdb_tok")

	srv := New(":0", &fakeEngine{}, "mdb_tok", nil, store, nil)

	body := "grant_type=client_credentials&client_id=oc_test&client_secret=wrong"
	req := httptest.NewRequest("POST", "/mcp/oauth/token", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	w := httptest.NewRecorder()
	srv.srv.Handler.ServeHTTP(w, req)

	if w.Code != http.StatusUnauthorized {
		t.Fatalf("expected 401, got %d", w.Code)
	}

	var resp oauthErrorResponse
	json.NewDecoder(w.Body).Decode(&resp)
	if resp.Error != "invalid_client" {
		t.Errorf("expected error 'invalid_client', got %q", resp.Error)
	}
}

func TestOAuthToken_UnknownClient(t *testing.T) {
	store := newFakeOAuthStore()
	srv := New(":0", &fakeEngine{}, "mdb_tok", nil, store, nil)

	body := "grant_type=client_credentials&client_id=oc_nope&client_secret=any"
	req := httptest.NewRequest("POST", "/mcp/oauth/token", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	w := httptest.NewRecorder()
	srv.srv.Handler.ServeHTTP(w, req)

	if w.Code != http.StatusUnauthorized {
		t.Fatalf("expected 401, got %d", w.Code)
	}
}

func TestOAuthToken_WrongGrantType(t *testing.T) {
	store := newFakeOAuthStore()
	srv := New(":0", &fakeEngine{}, "mdb_tok", nil, store, nil)

	body := "grant_type=authorization_code&client_id=oc_test&client_secret=sec"
	req := httptest.NewRequest("POST", "/mcp/oauth/token", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	w := httptest.NewRecorder()
	srv.srv.Handler.ServeHTTP(w, req)

	if w.Code != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d", w.Code)
	}

	var resp oauthErrorResponse
	json.NewDecoder(w.Body).Decode(&resp)
	if resp.Error != "unsupported_grant_type" {
		t.Errorf("expected error 'unsupported_grant_type', got %q", resp.Error)
	}
}

func TestOAuthToken_MissingCredentials(t *testing.T) {
	store := newFakeOAuthStore()
	srv := New(":0", &fakeEngine{}, "mdb_tok", nil, store, nil)

	body := "grant_type=client_credentials"
	req := httptest.NewRequest("POST", "/mcp/oauth/token", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	w := httptest.NewRecorder()
	srv.srv.Handler.ServeHTTP(w, req)

	if w.Code != http.StatusUnauthorized {
		t.Fatalf("expected 401, got %d", w.Code)
	}
}

func TestOAuthToken_MethodNotAllowed(t *testing.T) {
	store := newFakeOAuthStore()
	srv := New(":0", &fakeEngine{}, "mdb_tok", nil, store, nil)

	req := httptest.NewRequest("GET", "/mcp/oauth/token", nil)
	w := httptest.NewRecorder()
	srv.srv.Handler.ServeHTTP(w, req)

	if w.Code != http.StatusMethodNotAllowed {
		t.Fatalf("expected 405, got %d", w.Code)
	}
}

func TestOAuthToken_NoOAuthConfigured(t *testing.T) {
	// oauthClients is nil — server has no OAuth support.
	srv := New(":0", &fakeEngine{}, "mdb_tok", nil, nil, nil)

	body := "grant_type=client_credentials&client_id=x&client_secret=y"
	req := httptest.NewRequest("POST", "/mcp/oauth/token", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	w := httptest.NewRecorder()
	srv.srv.Handler.ServeHTTP(w, req)

	if w.Code != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d: %s", w.Code, w.Body.String())
	}
}

// ── Discovery endpoint tests ────────────────────────────────────────────

func TestOAuthDiscovery_ReturnsMetadata(t *testing.T) {
	store := newFakeOAuthStore()
	srv := New(":0", &fakeEngine{}, "mdb_tok", nil, store, nil)

	req := httptest.NewRequest("GET", "/.well-known/oauth-authorization-server", nil)
	req.Host = "muninndb.fly.dev"
	w := httptest.NewRecorder()
	srv.srv.Handler.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}

	var meta map[string]any
	if err := json.NewDecoder(w.Body).Decode(&meta); err != nil {
		t.Fatalf("decode: %v", err)
	}

	// HTTPS is inferred because r.TLS is nil but we check the fallback.
	// (httptest doesn't set TLS, so scheme defaults to http)
	if meta["issuer"] != "http://muninndb.fly.dev" {
		t.Errorf("unexpected issuer: %v", meta["issuer"])
	}
	if meta["token_endpoint"] != "http://muninndb.fly.dev/mcp/oauth/token" {
		t.Errorf("unexpected token_endpoint: %v", meta["token_endpoint"])
	}
	grants, ok := meta["grant_types_supported"].([]any)
	if !ok || len(grants) != 1 || grants[0] != "client_credentials" {
		t.Errorf("unexpected grant_types_supported: %v", meta["grant_types_supported"])
	}
}

func TestOAuthDiscovery_RespectsXForwardedHeaders(t *testing.T) {
	store := newFakeOAuthStore()
	srv := New(":0", &fakeEngine{}, "", nil, store, nil)

	req := httptest.NewRequest("GET", "/.well-known/oauth-authorization-server", nil)
	req.Header.Set("X-Forwarded-Proto", "https")
	req.Header.Set("X-Forwarded-Host", "custom.example.com")
	w := httptest.NewRecorder()
	srv.srv.Handler.ServeHTTP(w, req)

	var meta map[string]any
	json.NewDecoder(w.Body).Decode(&meta)

	if meta["issuer"] != "https://custom.example.com" {
		t.Errorf("unexpected issuer: %v", meta["issuer"])
	}
}

func TestOAuthDiscovery_Returns404WhenOAuthDisabled(t *testing.T) {
	srv := New(":0", &fakeEngine{}, "mdb_tok", nil, nil, nil)

	req := httptest.NewRequest("GET", "/.well-known/oauth-authorization-server", nil)
	req.Host = "muninndb.fly.dev"
	w := httptest.NewRecorder()
	srv.srv.Handler.ServeHTTP(w, req)

	if w.Code != http.StatusNotFound {
		t.Fatalf("expected 404 when OAuth not configured, got %d", w.Code)
	}
}

func TestOAuthToken_RejectsWrongContentType(t *testing.T) {
	store := newFakeOAuthStore()
	store.addClient("oc_test", "secret", "test", "mdb_tok")
	srv := New(":0", &fakeEngine{}, "mdb_tok", nil, store, nil)

	body := `{"grant_type":"client_credentials","client_id":"oc_test","client_secret":"secret"}`
	req := httptest.NewRequest("POST", "/mcp/oauth/token", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	srv.srv.Handler.ServeHTTP(w, req)

	if w.Code != http.StatusBadRequest {
		t.Fatalf("expected 400 for wrong Content-Type, got %d", w.Code)
	}
	var resp oauthErrorResponse
	json.NewDecoder(w.Body).Decode(&resp)
	if resp.Error != "invalid_request" {
		t.Errorf("expected error 'invalid_request', got %q", resp.Error)
	}
}

// ── Regression: existing Bearer auth still works ────────────────────────

func TestOAuthDoesNotBreakBearerAuth(t *testing.T) {
	store := newFakeOAuthStore()
	store.addClient("oc_test", "secret", "test", "mdb_tok")

	// Server with both static token and OAuth.
	srv := New(":0", &fakeEngine{}, "mdb_tok", nil, store, nil)

	// Regular Bearer auth should still work.
	body := `{"jsonrpc":"2.0","id":1,"method":"tools/list"}`
	req := httptest.NewRequest("POST", "/mcp", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer mdb_tok")
	w := httptest.NewRecorder()
	srv.srv.Handler.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("Bearer auth should still work, got %d: %s", w.Code, w.Body.String())
	}
}

// ── OAuth token can be used for MCP requests ────────────────────────────

func TestOAuthTokenUsableForMCP(t *testing.T) {
	store := newFakeOAuthStore()
	store.addClient("oc_test", "secret", "test", "mdb_mytoken")

	srv := New(":0", &fakeEngine{}, "mdb_mytoken", nil, store, nil)

	// Step 1: Get token via OAuth.
	tokenBody := "grant_type=client_credentials&client_id=oc_test&client_secret=secret"
	tokenReq := httptest.NewRequest("POST", "/mcp/oauth/token", strings.NewReader(tokenBody))
	tokenReq.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	tokenW := httptest.NewRecorder()
	srv.srv.Handler.ServeHTTP(tokenW, tokenReq)

	var tokenResp oauthTokenResponse
	json.NewDecoder(tokenW.Body).Decode(&tokenResp)

	// Step 2: Use that token for an MCP request.
	mcpBody := `{"jsonrpc":"2.0","id":1,"method":"tools/list"}`
	mcpReq := httptest.NewRequest("POST", "/mcp", strings.NewReader(mcpBody))
	mcpReq.Header.Set("Content-Type", "application/json")
	mcpReq.Header.Set("Authorization", "Bearer "+tokenResp.AccessToken)
	mcpW := httptest.NewRecorder()
	srv.srv.Handler.ServeHTTP(mcpW, mcpReq)

	if mcpW.Code != http.StatusOK {
		t.Fatalf("OAuth-issued token should work for MCP, got %d: %s", mcpW.Code, mcpW.Body.String())
	}
}
