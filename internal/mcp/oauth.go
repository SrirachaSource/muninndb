package mcp

import (
	"encoding/base64"
	"encoding/json"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/scrypster/muninndb/internal/auth"
)

// oauthClientValidator is the subset of auth.Store used by MCP for OAuth
// client credentials validation. Using an interface keeps the mcp package
// testable without a live Pebble store.
type oauthClientValidator interface {
	ValidateOAuthClient(clientID, clientSecret string) (auth.OAuthClient, error)
}

// oauthTokenResponse is the standard OAuth 2.0 token response (RFC 6749 Section 5.1).
type oauthTokenResponse struct {
	AccessToken string `json:"access_token"`
	TokenType   string `json:"token_type"`
	ExpiresIn   int    `json:"expires_in"`
}

// oauthErrorResponse is the standard OAuth 2.0 error response (RFC 6749 Section 5.2).
type oauthErrorResponse struct {
	Error       string `json:"error"`
	Description string `json:"error_description,omitempty"`
}

// oauthTokenExpiresIn is the advertised token lifetime. Since the underlying
// MCP token is long-lived (no actual expiry), this is a hint to clients
// telling them when to re-request. Claude.ai will call the token endpoint
// again before this window expires.
const oauthTokenExpiresIn = 3600 // 1 hour

// rateLimitDelay is applied to failed OAuth token requests to slow brute-force
// attempts. In production this could be replaced with a proper rate limiter.
var rateLimitDelay = 100 * time.Millisecond

// handleOAuthToken implements POST /mcp/oauth/token — the OAuth 2.0 Client
// Credentials token endpoint (RFC 6749 Section 4.4).
//
// Accepts client credentials via:
//  1. Request body: client_id + client_secret (client_secret_post)
//  2. HTTP Basic auth: Authorization: Basic base64(client_id:client_secret) (client_secret_basic)
//
// On success, returns the MCP bearer token that the client is mapped to.
func (s *MCPServer) handleOAuthToken(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		w.Header().Set("Allow", "POST")
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	// Body size limit — token requests are small form-encoded payloads.
	r.Body = http.MaxBytesReader(w, r.Body, 1<<16) // 64KB

	if s.oauthClients == nil {
		sendOAuthError(w, http.StatusBadRequest, "unsupported_grant_type",
			"OAuth is not configured on this server")
		return
	}

	// RFC 6749 Section 4.4.2: request body MUST be application/x-www-form-urlencoded.
	// Reject other content types to prevent credentials leaking via query params.
	ct := r.Header.Get("Content-Type")
	if !strings.HasPrefix(ct, "application/x-www-form-urlencoded") {
		sendOAuthError(w, http.StatusBadRequest, "invalid_request",
			"Content-Type must be application/x-www-form-urlencoded")
		return
	}

	if err := r.ParseForm(); err != nil {
		sendOAuthError(w, http.StatusBadRequest, "invalid_request",
			"could not parse request body")
		return
	}

	// Validate grant_type.
	grantType := r.FormValue("grant_type")
	if grantType != "client_credentials" {
		sendOAuthError(w, http.StatusBadRequest, "unsupported_grant_type",
			"only client_credentials grant is supported")
		return
	}

	// Extract client credentials — try Basic auth first, then form body.
	clientID, clientSecret, ok := extractClientCredentials(r)
	if !ok {
		w.Header().Set("WWW-Authenticate", `Basic realm="muninndb"`)
		sendOAuthError(w, http.StatusUnauthorized, "invalid_client",
			"missing or malformed client credentials")
		return
	}

	// Validate against the store.
	client, err := s.oauthClients.ValidateOAuthClient(clientID, clientSecret)
	if err != nil {
		// Brute-force delay — slows down credential guessing.
		// Do NOT log the client_secret or request body.
		time.Sleep(rateLimitDelay)
		slog.Warn("mcp: OAuth token request failed", "client_id", clientID)
		w.Header().Set("WWW-Authenticate", `Basic realm="muninndb"`)
		sendOAuthError(w, http.StatusUnauthorized, "invalid_client",
			"invalid client_id or client_secret")
		return
	}

	slog.Info("mcp: OAuth token issued", "client_id", clientID, "name", client.Name)

	// Return the MCP token as the access token. The token is already valid
	// for MCP auth — no separate token type needed.
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Pragma", "no-cache")
	json.NewEncoder(w).Encode(oauthTokenResponse{
		AccessToken: client.MCPToken,
		TokenType:   "bearer",
		ExpiresIn:   oauthTokenExpiresIn,
	})
}

// handleOAuthDiscovery implements GET /.well-known/oauth-authorization-server
// per RFC 8414 (OAuth 2.0 Authorization Server Metadata).
// Returns 404 when OAuth is not configured to avoid misleading clients.
func (s *MCPServer) handleOAuthDiscovery(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		w.Header().Set("Allow", "GET")
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	// Don't advertise OAuth when it's not enabled.
	if s.oauthClients == nil {
		http.NotFound(w, r)
		return
	}

	issuer := resolveIssuerURL(r)
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]any{
		"issuer":                 issuer,
		"token_endpoint":         issuer + "/mcp/oauth/token",
		"grant_types_supported":  []string{"client_credentials"},
		"token_endpoint_auth_methods_supported": []string{
			"client_secret_post",
			"client_secret_basic",
		},
		"response_types_supported": []string{},
	})
}

// extractClientCredentials extracts client_id and client_secret from the request.
// Priority: HTTP Basic auth header → form body parameters.
func extractClientCredentials(r *http.Request) (clientID, clientSecret string, ok bool) {
	// 1. Try HTTP Basic auth (client_secret_basic).
	if authHeader := r.Header.Get("Authorization"); strings.HasPrefix(authHeader, "Basic ") {
		// Try standard base64 first, then raw (no padding) for compatibility.
		payload := authHeader[6:]
		decoded, err := base64.StdEncoding.DecodeString(payload)
		if err != nil {
			decoded, err = base64.RawStdEncoding.DecodeString(payload)
		}
		if err == nil {
			if idx := strings.IndexByte(string(decoded), ':'); idx >= 0 {
				clientID = string(decoded[:idx])
				clientSecret = string(decoded[idx+1:])
				if clientID != "" && clientSecret != "" {
					return clientID, clientSecret, true
				}
			}
		}
	}

	// 2. Try form body (client_secret_post).
	clientID = r.FormValue("client_id")
	clientSecret = r.FormValue("client_secret")
	if clientID != "" && clientSecret != "" {
		return clientID, clientSecret, true
	}

	return "", "", false
}

// resolveIssuerURL builds the issuer URL from the request, respecting
// reverse proxy headers (X-Forwarded-Proto, X-Forwarded-Host).
//
// NOTE: In production behind a reverse proxy, X-Forwarded-* headers should be
// set by the proxy and stripped from untrusted client requests. If the server
// is exposed directly, these headers are attacker-controlled. For maximum
// security, configure the issuer URL as a server startup flag instead.
func resolveIssuerURL(r *http.Request) string {
	scheme := "https"
	if proto := r.Header.Get("X-Forwarded-Proto"); proto != "" {
		scheme = proto
	} else if r.TLS == nil {
		scheme = "http"
	}

	host := r.Header.Get("X-Forwarded-Host")
	if host == "" {
		host = r.Host
	}

	return scheme + "://" + host
}

// sendOAuthError writes an RFC 6749 Section 5.2 error response.
func sendOAuthError(w http.ResponseWriter, status int, errCode, description string) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(oauthErrorResponse{
		Error:       errCode,
		Description: description,
	})
}
