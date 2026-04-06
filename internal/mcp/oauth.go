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
	AccessToken  string `json:"access_token"`
	TokenType    string `json:"token_type"`
	ExpiresIn    int    `json:"expires_in"`
	RefreshToken string `json:"refresh_token,omitempty"`
}

// oauthErrorResponse is the standard OAuth 2.0 error response (RFC 6749 Section 5.2).
type oauthErrorResponse struct {
	Error       string `json:"error"`
	Description string `json:"error_description,omitempty"`
}

// oauthTokenExpiresIn is the advertised token lifetime. Since the underlying
// MCP token is long-lived (no actual expiry), this is a hint to clients
// telling them when to re-request. Set to 7 days because Claude.ai/mobile
// treats expires_in as a hard deadline and won't auto-refresh reliably
// with short windows (see GitHub issues on Claude OAuth token handling).
const oauthTokenExpiresIn = 604800 // 7 days

// rateLimitDelay is applied to failed OAuth token requests to slow brute-force
// attempts. In production this could be replaced with a proper rate limiter.
var rateLimitDelay = 100 * time.Millisecond

// handleOAuthToken implements POST /mcp/oauth/token — the OAuth 2.0 token
// endpoint supporting both:
//   - client_credentials grant (RFC 6749 Section 4.4) — machine-to-machine
//   - authorization_code grant (RFC 6749 Section 4.1) — browser-based with PKCE
//
// On success, returns the MCP bearer token.
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

	// RFC 6749: request body MUST be application/x-www-form-urlencoded.
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

	grantType := r.FormValue("grant_type")
	switch grantType {
	case "client_credentials":
		s.handleTokenClientCredentials(w, r)
	case "authorization_code":
		s.handleTokenAuthorizationCode(w, r)
	case "refresh_token":
		s.handleTokenRefresh(w, r)
	default:
		sendOAuthError(w, http.StatusBadRequest, "unsupported_grant_type",
			"supported grant types: client_credentials, authorization_code, refresh_token")
	}
}

// handleTokenClientCredentials handles the client_credentials grant type.
func (s *MCPServer) handleTokenClientCredentials(w http.ResponseWriter, r *http.Request) {
	clientID, clientSecret, ok := extractClientCredentials(r)
	if !ok {
		w.Header().Set("WWW-Authenticate", `Basic realm="muninndb"`)
		sendOAuthError(w, http.StatusUnauthorized, "invalid_client",
			"missing or malformed client credentials")
		return
	}

	client, err := s.oauthClients.ValidateOAuthClient(clientID, clientSecret)
	if err != nil {
		time.Sleep(rateLimitDelay)
		slog.Warn("mcp: OAuth token request failed", "client_id", clientID)
		w.Header().Set("WWW-Authenticate", `Basic realm="muninndb"`)
		sendOAuthError(w, http.StatusUnauthorized, "invalid_client",
			"invalid client_id or client_secret")
		return
	}

	slog.Info("mcp: OAuth token issued (client_credentials)", "client_id", clientID, "name", client.Name)
	sendTokenResponse(w, client.MCPToken)
}

// handleTokenAuthorizationCode handles the authorization_code grant type with PKCE.
func (s *MCPServer) handleTokenAuthorizationCode(w http.ResponseWriter, r *http.Request) {
	code := r.FormValue("code")
	clientID := r.FormValue("client_id")
	redirectURI := r.FormValue("redirect_uri")
	codeVerifier := r.FormValue("code_verifier")

	if code == "" || clientID == "" || codeVerifier == "" {
		sendOAuthError(w, http.StatusBadRequest, "invalid_request",
			"missing required parameters: code, client_id, code_verifier")
		return
	}

	mcpToken, err := s.authCodes.exchange(code, clientID, redirectURI, codeVerifier)
	if err != nil {
		time.Sleep(rateLimitDelay)
		slog.Warn("mcp: auth code exchange failed", "client_id", clientID, "error", err.Error())
		sendOAuthError(w, http.StatusBadRequest, "invalid_grant", err.Error())
		return
	}

	slog.Info("mcp: OAuth token issued (authorization_code)", "client_id", clientID)
	sendTokenResponse(w, mcpToken)
}

// handleTokenRefresh handles the refresh_token grant type.
// Since MCP tokens are long-lived, the refresh token IS the access token --
// we just re-issue it. This lets Claude.ai/mobile silently refresh without
// hitting an auth wall when expires_in elapses.
func (s *MCPServer) handleTokenRefresh(w http.ResponseWriter, r *http.Request) {
	refreshToken := r.FormValue("refresh_token")
	if refreshToken == "" {
		sendOAuthError(w, http.StatusBadRequest, "invalid_request",
			"missing refresh_token parameter")
		return
	}

	// Validate the refresh token by checking it against the MCP token.
	// The refresh token is the MCP token itself, so if it's valid for
	// bearer auth it's valid for refresh.
	if s.token == "" || refreshToken != s.token {
		time.Sleep(rateLimitDelay)
		sendOAuthError(w, http.StatusUnauthorized, "invalid_grant",
			"invalid refresh token")
		return
	}

	slog.Info("mcp: OAuth token refreshed")
	sendTokenResponse(w, refreshToken)
}

// sendTokenResponse writes a successful OAuth token response.
// Includes a refresh_token so clients (Claude.ai/mobile) can silently
// re-authenticate when the access token expires.
func sendTokenResponse(w http.ResponseWriter, accessToken string) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Pragma", "no-cache")
	json.NewEncoder(w).Encode(oauthTokenResponse{
		AccessToken:  accessToken,
		TokenType:    "bearer",
		ExpiresIn:    oauthTokenExpiresIn,
		RefreshToken: accessToken, // MCP token is long-lived; refresh = re-issue
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
		"authorization_endpoint": issuer + "/authorize",
		"token_endpoint":         issuer + "/mcp/oauth/token",
		"registration_endpoint":  issuer + "/oauth/register",
		"grant_types_supported":  []string{"authorization_code", "client_credentials", "refresh_token"},
		"token_endpoint_auth_methods_supported": []string{
			"client_secret_post",
			"client_secret_basic",
			"none",
		},
		"response_types_supported":          []string{"code"},
		"code_challenge_methods_supported":  []string{"S256"},
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
