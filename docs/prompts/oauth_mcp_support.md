# Task: Add OAuth Client Credentials to MuninnDB's MCP Server

## Problem

Claude.ai's "Add custom connector" (account-level MCP) only supports OAuth Client Credentials flow. MuninnDB's MCP server only supports Bearer token auth. This means MuninnDB can't be added as an account-level connector, blocking mobile access and cross-device availability through Claude.ai.

## Goal

Add OAuth 2.0 Client Credentials grant support to MuninnDB's MCP endpoint (`/mcp`) so it works with Claude.ai's connector system. The existing Bearer token auth must continue working — OAuth is additive, not a replacement.

## Context

### How Claude.ai connectors work

Claude.ai's "Add custom connector" dialog asks for:
- **Remote MCP server URL** (e.g., `https://muninndb.fly.dev/mcp`)
- **OAuth Client ID** (optional)
- **OAuth Client Secret** (optional)

When both OAuth fields are provided, Claude.ai performs the standard OAuth 2.0 Client Credentials flow:
1. POST to the server's token endpoint with `grant_type=client_credentials`, `client_id`, `client_secret`
2. Receive an access token in the response
3. Use that token as `Bearer <token>` in subsequent MCP requests

If OAuth fields are empty, Claude.ai may attempt OAuth discovery (`.well-known/oauth-authorization-server`) and fail if not found.

### Current MuninnDB MCP auth (DO NOT BREAK)

File: `internal/mcp/server.go`

The MCP server checks for a Bearer token in the `Authorization` header. Token source priority:
1. `--mcp-token` flag
2. `MUNINN_MCP_TOKEN` env var  
3. `~/.muninn/mcp.token` file

If no token is configured, auth is disabled (open access). This must continue working exactly as-is.

### What needs to be added

1. **Token endpoint**: `POST /mcp/oauth/token` that accepts:
   - `grant_type=client_credentials`
   - `client_id` + `client_secret` (from request body or Basic auth header)
   - Returns: `{"access_token": "<token>", "token_type": "bearer", "expires_in": 3600}`

2. **OAuth discovery** (optional but recommended): `GET /mcp/.well-known/oauth-authorization-server` that returns:
   ```json
   {
     "issuer": "https://muninndb.fly.dev",
     "token_endpoint": "https://muninndb.fly.dev/mcp/oauth/token",
     "grant_types_supported": ["client_credentials"],
     "token_endpoint_auth_methods_supported": ["client_secret_post", "client_secret_basic"]
   }
   ```

3. **Client credentials storage**: A simple mapping of `client_id` → `client_secret` + `mcp_token`. When a valid client authenticates via OAuth, the returned access token is the existing MCP token (or a short-lived derivative). Stored in the auth database alongside existing admin/vault-key data.

4. **Admin UI**: A section in the Web UI settings to create/revoke OAuth clients. Each client maps to an MCP token scope.

5. **CLI**: `muninn oauth create-client --name "claude-ai"` that generates a client_id + client_secret pair and prints them.

### Key files to modify

- `internal/mcp/server.go` — add `/mcp/oauth/token` and discovery endpoints
- `internal/mcp/handlers.go` — OAuth token exchange handler
- `internal/auth/` — OAuth client storage (store.go or new oauth_store.go)
- `cmd/muninn/server.go` — wire up new endpoints
- `web/templates/index.html` — OAuth client management UI section (optional, can defer)

### Design constraints

- **Bearer token auth stays as-is** — OAuth is a second auth path, not a replacement
- **No external OAuth provider** — MuninnDB IS the authorization server
- **Stateless tokens preferred** — the access token returned can just be the existing MCP token (simplest) or a signed JWT with expiry
- **Keep it simple** — this is Client Credentials only (machine-to-machine), not Authorization Code flow. No redirects, no user consent screens, no PKCE.

### Testing

- Existing MCP Bearer token auth still works (regression)
- `curl -X POST /mcp/oauth/token -d "grant_type=client_credentials&client_id=X&client_secret=Y"` returns a valid access token
- The returned token works as `Authorization: Bearer <token>` on `/mcp` endpoints
- Invalid client_id/secret returns 401
- Discovery endpoint returns valid JSON with correct URLs
- Claude.ai connector can be added with the client_id + client_secret and successfully calls MuninnDB tools

### Reference

- OAuth 2.0 Client Credentials: RFC 6749 Section 4.4
- MCP specification auth: https://modelcontextprotocol.io/specification/2025-03-26/basic/transports#authentication
- Existing MuninnDB auth code: `internal/auth/`
- Existing MCP server: `internal/mcp/server.go`

### After implementation

Once this works:
1. Create an OAuth client: `muninn oauth create-client --name "claude-ai"`
2. Add to Claude.ai: Settings → Connectors → Add custom connector → paste URL + client_id + client_secret
3. MuninnDB available on mobile, desktop app, web — everywhere Claude.ai runs
