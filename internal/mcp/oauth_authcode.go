package mcp

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"html/template"
	"log/slog"
	"net/http"
	"net/url"
	"sync"
	"time"

	"github.com/scrypster/muninndb/internal/auth"
)

// authCodeEntry stores a pending authorization code with its PKCE challenge
// and metadata. Codes are single-use and expire after authCodeTTL.
type authCodeEntry struct {
	Code                string
	ClientID            string
	RedirectURI         string
	CodeChallenge       string
	CodeChallengeMethod string
	MCPToken            string // the MCP bearer token to issue on exchange
	CreatedAt           time.Time
}

const authCodeTTL = 5 * time.Minute
const authCodeLen = 32 // 32 bytes = 43 chars base64url

// authCodeStore is a simple in-memory store for pending authorization codes.
// Codes are single-use (deleted on exchange) and expire after authCodeTTL.
// This is adequate for a single-instance server; clustered deployments would
// need a shared store (Pebble or Redis).
type authCodeStore struct {
	mu    sync.Mutex
	codes map[string]*authCodeEntry
}

func newAuthCodeStore() *authCodeStore {
	s := &authCodeStore{codes: make(map[string]*authCodeEntry)}
	go s.sweepLoop()
	return s
}

// issue creates a new authorization code and stores it.
func (s *authCodeStore) issue(clientID, redirectURI, codeChallenge, codeChallengeMethod, mcpToken string) (string, error) {
	raw := make([]byte, authCodeLen)
	if _, err := rand.Read(raw); err != nil {
		return "", fmt.Errorf("generate auth code: %w", err)
	}
	code := base64.RawURLEncoding.EncodeToString(raw)

	s.mu.Lock()
	defer s.mu.Unlock()
	s.codes[code] = &authCodeEntry{
		Code:                code,
		ClientID:            clientID,
		RedirectURI:         redirectURI,
		CodeChallenge:       codeChallenge,
		CodeChallengeMethod: codeChallengeMethod,
		MCPToken:            mcpToken,
		CreatedAt:           time.Now(),
	}
	return code, nil
}

// exchange validates and consumes an authorization code. Single-use: the code
// is deleted regardless of whether PKCE validation succeeds.
func (s *authCodeStore) exchange(code, clientID, redirectURI, codeVerifier string) (string, error) {
	s.mu.Lock()
	entry, ok := s.codes[code]
	if ok {
		delete(s.codes, code) // single-use: always consume
	}
	s.mu.Unlock()

	if !ok {
		return "", fmt.Errorf("invalid or expired authorization code")
	}
	if time.Since(entry.CreatedAt) > authCodeTTL {
		return "", fmt.Errorf("authorization code expired")
	}
	if entry.ClientID != clientID {
		return "", fmt.Errorf("client_id mismatch")
	}
	if entry.RedirectURI != redirectURI {
		return "", fmt.Errorf("redirect_uri mismatch")
	}

	// PKCE verification (RFC 7636 Section 4.6)
	if !verifyPKCE(entry.CodeChallenge, entry.CodeChallengeMethod, codeVerifier) {
		return "", fmt.Errorf("PKCE verification failed")
	}

	return entry.MCPToken, nil
}

// sweepLoop removes expired codes every 60 seconds.
func (s *authCodeStore) sweepLoop() {
	ticker := time.NewTicker(60 * time.Second)
	defer ticker.Stop()
	for range ticker.C {
		s.mu.Lock()
		now := time.Now()
		for code, entry := range s.codes {
			if now.Sub(entry.CreatedAt) > authCodeTTL {
				delete(s.codes, code)
			}
		}
		s.mu.Unlock()
	}
}

// verifyPKCE checks that SHA256(codeVerifier) matches the stored codeChallenge.
func verifyPKCE(codeChallenge, method, codeVerifier string) bool {
	if codeVerifier == "" || codeChallenge == "" {
		return false
	}
	switch method {
	case "S256":
		h := sha256.Sum256([]byte(codeVerifier))
		computed := base64.RawURLEncoding.EncodeToString(h[:])
		return computed == codeChallenge
	case "plain":
		return codeVerifier == codeChallenge
	default:
		return false
	}
}

// consentPageHTML is a minimal, self-contained consent page. No external
// dependencies — works on any device. The user sees the client name and
// clicks Authorize to approve.
var consentPageTmpl = template.Must(template.New("consent").Parse(`<!DOCTYPE html>
<html lang="en">
<head>
<meta charset="UTF-8">
<meta name="viewport" content="width=device-width, initial-scale=1.0">
<title>Authorize - MuninnDB</title>
<style>
  * { margin: 0; padding: 0; box-sizing: border-box; }
  body { font-family: -apple-system, BlinkMacSystemFont, 'Segoe UI', Roboto, sans-serif;
         background: #0f1117; color: #e4e4e7; display: flex; align-items: center;
         justify-content: center; min-height: 100vh; padding: 1rem; }
  .card { background: #1a1b23; border: 1px solid #2a2b35; border-radius: 12px;
          padding: 2rem; max-width: 420px; width: 100%; }
  h1 { font-size: 1.25rem; margin-bottom: 0.5rem; }
  .sub { color: #a1a1aa; font-size: 0.875rem; margin-bottom: 1.5rem; }
  .client { background: #252630; border-radius: 8px; padding: 1rem; margin-bottom: 1.5rem; }
  .client-name { font-weight: 600; font-size: 1rem; }
  .client-id { color: #71717a; font-size: 0.75rem; font-family: monospace; margin-top: 0.25rem; }
  .scope { color: #a1a1aa; font-size: 0.8125rem; margin-top: 0.75rem; }
  .actions { display: flex; gap: 0.75rem; }
  .btn { flex: 1; padding: 0.625rem 1rem; border: none; border-radius: 8px;
         font-size: 0.875rem; font-weight: 500; cursor: pointer; text-align: center;
         text-decoration: none; display: inline-block; }
  .btn-primary { background: #06b6d4; color: #000; }
  .btn-primary:hover { background: #22d3ee; }
  .btn-secondary { background: #2a2b35; color: #e4e4e7; }
  .btn-secondary:hover { background: #353640; }
</style>
</head>
<body>
<div class="card">
  <h1>Authorize Access</h1>
  <p class="sub">An application wants to access your MuninnDB memories.</p>
  <div class="client">
    <div class="client-name">{{.ClientName}}</div>
    <div class="client-id">{{.ClientID}}</div>
  </div>
  <p class="scope">This will grant full read and write access to your memories.</p>
  <form method="POST" action="/authorize/approve" style="margin-top:1.5rem;">
    <input type="hidden" name="client_id" value="{{.ClientID}}">
    <input type="hidden" name="redirect_uri" value="{{.RedirectURI}}">
    <input type="hidden" name="code_challenge" value="{{.CodeChallenge}}">
    <input type="hidden" name="code_challenge_method" value="{{.CodeChallengeMethod}}">
    <input type="hidden" name="state" value="{{.State}}">
    <input type="hidden" name="response_type" value="{{.ResponseType}}">
    <div class="actions">
      <a href="{{.DenyURL}}" class="btn btn-secondary">Deny</a>
      <button type="submit" class="btn btn-primary">Authorize</button>
    </div>
  </form>
</div>
</body>
</html>`))

type consentData struct {
	ClientName          string
	ClientID            string
	RedirectURI         string
	CodeChallenge       string
	CodeChallengeMethod string
	State               string
	ResponseType        string
	DenyURL             string
}

// handleAuthorize implements GET /authorize — the OAuth 2.0 Authorization
// endpoint (RFC 6749 Section 3.1). Shows a consent page to the user.
func (s *MCPServer) handleAuthorize(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	q := r.URL.Query()
	responseType := q.Get("response_type")
	clientID := q.Get("client_id")
	redirectURI := q.Get("redirect_uri")
	codeChallenge := q.Get("code_challenge")
	codeChallengeMethod := q.Get("code_challenge_method")
	state := q.Get("state")

	// Validate required parameters.
	if responseType != "code" {
		http.Error(w, "unsupported response_type: must be 'code'", http.StatusBadRequest)
		return
	}
	if clientID == "" {
		http.Error(w, "missing client_id", http.StatusBadRequest)
		return
	}
	if redirectURI == "" {
		http.Error(w, "missing redirect_uri", http.StatusBadRequest)
		return
	}
	if codeChallenge == "" || codeChallengeMethod == "" {
		http.Error(w, "PKCE required: missing code_challenge or code_challenge_method", http.StatusBadRequest)
		return
	}
	if codeChallengeMethod != "S256" {
		http.Error(w, "unsupported code_challenge_method: must be 'S256'", http.StatusBadRequest)
		return
	}

	// Look up client name for the consent page. If the client doesn't exist
	// in our OAuth store, we still show the consent page with just the ID —
	// Claude.ai uses dynamic client registration patterns where the client_id
	// may not be pre-registered.
	clientName := clientID
	if s.oauthClients != nil {
		// Try to look up a friendly name. ValidateOAuthClient needs a secret
		// which we don't have here, so we use a separate lookup if available.
		// For now, just show the client_id.
		clientName = clientID
	}

	// Build deny URL — redirect back with error=access_denied.
	denyParams := url.Values{
		"error":             {"access_denied"},
		"error_description": {"user denied the authorization request"},
	}
	if state != "" {
		denyParams.Set("state", state)
	}
	denyURL := redirectURI + "?" + denyParams.Encode()

	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	consentPageTmpl.Execute(w, consentData{
		ClientName:          clientName,
		ClientID:            clientID,
		RedirectURI:         redirectURI,
		CodeChallenge:       codeChallenge,
		CodeChallengeMethod: codeChallengeMethod,
		State:               state,
		ResponseType:        responseType,
		DenyURL:             denyURL,
	})
}

// handleAuthorizeApprove implements POST /authorize/approve — processes the
// user's consent form submission and redirects back with an authorization code.
func (s *MCPServer) handleAuthorizeApprove(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	r.Body = http.MaxBytesReader(w, r.Body, 1<<16)
	if err := r.ParseForm(); err != nil {
		http.Error(w, "invalid form", http.StatusBadRequest)
		return
	}

	clientID := r.FormValue("client_id")
	redirectURI := r.FormValue("redirect_uri")
	codeChallenge := r.FormValue("code_challenge")
	codeChallengeMethod := r.FormValue("code_challenge_method")
	state := r.FormValue("state")

	if clientID == "" || redirectURI == "" || codeChallenge == "" {
		http.Error(w, "missing required parameters", http.StatusBadRequest)
		return
	}

	// Issue an authorization code. The MCP token comes from the server's
	// configured static token — same as the Client Credentials flow.
	code, err := s.authCodes.issue(clientID, redirectURI, codeChallenge, codeChallengeMethod, s.token)
	if err != nil {
		slog.Error("mcp: failed to issue auth code", "error", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}

	slog.Info("mcp: authorization code issued", "client_id", clientID)

	// Redirect back to the client with the code.
	params := url.Values{"code": {code}}
	if state != "" {
		params.Set("state", state)
	}
	http.Redirect(w, r, redirectURI+"?"+params.Encode(), http.StatusFound)
}

// handleProtectedResourceMetadata implements GET /.well-known/oauth-protected-resource
// per RFC 9728. This tells Claude.ai where to find the authorization server.
func (s *MCPServer) handleProtectedResourceMetadata(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		w.Header().Set("Allow", "GET")
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	issuer := resolveIssuerURL(r)
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]any{
		"resource":              issuer + "/mcp",
		"authorization_servers": []string{issuer},
	})
}

// handleOAuthClientRegistration implements POST /oauth/register — dynamic
// client registration (RFC 7591). Claude.ai may send client metadata here
// when using URL-based client_id.
func (s *MCPServer) handleOAuthClientRegistration(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		w.Header().Set("Allow", "POST")
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	r.Body = http.MaxBytesReader(w, r.Body, 1<<16)

	var req struct {
		ClientName             string   `json:"client_name"`
		RedirectURIs           []string `json:"redirect_uris"`
		GrantTypes             []string `json:"grant_types"`
		ResponseTypes          []string `json:"response_types"`
		TokenEndpointAuthMethod string  `json:"token_endpoint_auth_method"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		sendOAuthError(w, http.StatusBadRequest, "invalid_request", "invalid JSON body")
		return
	}

	if req.ClientName == "" {
		req.ClientName = "dynamic-client"
	}

	// Create an OAuth client mapped to the server's MCP token.
	if s.oauthClients == nil || s.token == "" {
		sendOAuthError(w, http.StatusBadRequest, "invalid_request",
			"OAuth not configured on this server")
		return
	}

	// Use the oauthClientCreator interface if available.
	creator, ok := s.oauthClients.(oauthClientCreator)
	if !ok {
		sendOAuthError(w, http.StatusBadRequest, "invalid_request",
			"dynamic registration not supported")
		return
	}

	clientID, clientSecret, _, err := creator.CreateOAuthClient(req.ClientName, s.token)
	if err != nil {
		slog.Error("mcp: dynamic client registration failed", "error", err)
		sendOAuthError(w, http.StatusInternalServerError, "server_error",
			"failed to create client")
		return
	}

	slog.Info("mcp: dynamic client registered", "client_id", clientID, "name", req.ClientName)

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusCreated)
	json.NewEncoder(w).Encode(map[string]any{
		"client_id":                clientID,
		"client_secret":            clientSecret,
		"client_name":              req.ClientName,
		"redirect_uris":            req.RedirectURIs,
		"grant_types":              req.GrantTypes,
		"response_types":           req.ResponseTypes,
		"token_endpoint_auth_method": "client_secret_post",
	})
}

// oauthClientCreator extends oauthClientValidator with the ability to create
// new clients (used for dynamic registration).
type oauthClientCreator interface {
	oauthClientValidator
	CreateOAuthClient(name, mcpToken string) (clientID, clientSecret string, client auth.OAuthClient, err error)
}
