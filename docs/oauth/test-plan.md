# OAuth 2.0 Client Credentials — Test Plan

## Context

MuninnDB now supports OAuth 2.0 Client Credentials grant (RFC 6749 Section 4.4) on its MCP endpoint. This enables Claude.ai account-level connectors, unlocking mobile and cross-device access.

This plan covers four testing phases, bottom-up: compile, local integration, production deploy, and Claude.ai connector.

---

## Phase 1: Compile + Unit Tests (Docker)

Go 1.25 is required. If not installed locally, use the Docker builder stage.

### Build the test image

```bash
cd /path/to/muninndb
docker build --target builder -t muninndb-test .
```

### Run targeted OAuth tests

```bash
# OAuth store tests (Pebble in-memory, bcrypt, timing equalization)
docker run --rm --workdir //src muninndb-test \
  go test ./internal/auth/ -run OAuth -v -timeout 30s

# MCP OAuth endpoint tests (token endpoint, discovery, Content-Type, regression)
docker run --rm --workdir //src muninndb-test \
  go test ./internal/mcp/ -run OAuth -v -timeout 30s
```

### Run full test suite (regression)

```bash
docker run --rm --workdir //src muninndb-test \
  go test -tags localassets ./... -timeout 300s
```

### Pass criteria

- All 6 auth store tests green
- All 14 MCP OAuth tests green
- Full suite compiles and passes (no regressions from `mcp.New()` signature change)

### Files under test

| File | Tests |
|------|-------|
| `internal/auth/oauth_store.go` | `oauth_store_test.go` — create, validate, invalid secret, not found, list/revoke, ID prefix |
| `internal/mcp/oauth.go` | `oauth_test.go` — token endpoint (POST + Basic), discovery, Content-Type enforcement, error codes, regression, end-to-end |

---

## Phase 2: Local Integration Tests

Start MuninnDB with an MCP token configured. Docker Compose or native binary.

### Prerequisites

```bash
# Docker Compose — add MUNINN_MCP_TOKEN to docker-compose.yml environment:
MUNINN_MCP_TOKEN: "mdb_your_test_token"

# Then:
docker compose up -d --build
```

Or natively:

```bash
export MUNINN_MCP_TOKEN="mdb_your_test_token"
muninn start
```

### Step 1 — Admin login + create OAuth client

```bash
# Login (from inside container if using Docker)
curl -s -c cookies.txt -X POST http://127.0.0.1:8476/api/auth/login \
  -H "Content-Type: application/json" \
  -d '{"username":"root","password":"password"}'

# Create OAuth client
curl -s -b cookies.txt -X POST http://127.0.0.1:8475/api/admin/oauth/clients \
  -H "Content-Type: application/json" \
  -d '{"name":"test-client"}'
# Save client_id and client_secret from response
```

### Step 2 — OAuth token exchange

```bash
# client_secret_post method
curl -v -X POST http://127.0.0.1:8750/mcp/oauth/token \
  -H "Content-Type: application/x-www-form-urlencoded" \
  -d "grant_type=client_credentials&client_id=<CLIENT_ID>&client_secret=<CLIENT_SECRET>"
# Expect: 200 {"access_token":"mdb_...","token_type":"bearer","expires_in":3600}

# client_secret_basic method
curl -v -X POST http://127.0.0.1:8750/mcp/oauth/token \
  -H "Content-Type: application/x-www-form-urlencoded" \
  -u "<CLIENT_ID>:<CLIENT_SECRET>" \
  -d "grant_type=client_credentials"
# Expect: same 200 response
```

### Step 3 — Use OAuth-issued token for MCP

```bash
curl -s -X POST http://127.0.0.1:8750/mcp \
  -H "Authorization: Bearer <access_token>" \
  -H "Content-Type: application/json" \
  -d '{"jsonrpc":"2.0","id":1,"method":"tools/list"}'
# Expect: 200 with tool list
```

### Step 4 — Discovery endpoint

```bash
curl -s http://127.0.0.1:8750/.well-known/oauth-authorization-server | jq .
# Expect: issuer, token_endpoint, grant_types_supported
```

### Step 5 — Negative tests

```bash
# Wrong secret -> 401
curl -s -o /dev/null -w "%{http_code}" -X POST http://127.0.0.1:8750/mcp/oauth/token \
  -H "Content-Type: application/x-www-form-urlencoded" \
  -d "grant_type=client_credentials&client_id=<CLIENT_ID>&client_secret=wrong"

# Wrong grant type -> 400
curl -s -o /dev/null -w "%{http_code}" -X POST http://127.0.0.1:8750/mcp/oauth/token \
  -H "Content-Type: application/x-www-form-urlencoded" \
  -d "grant_type=authorization_code&client_id=x&client_secret=y"

# JSON Content-Type -> 400
curl -s -o /dev/null -w "%{http_code}" -X POST http://127.0.0.1:8750/mcp/oauth/token \
  -H "Content-Type: application/json" \
  -d '{"grant_type":"client_credentials"}'

# Bearer auth still works (regression)
curl -s -X POST http://127.0.0.1:8750/mcp \
  -H "Authorization: Bearer <MCP_TOKEN>" \
  -H "Content-Type: application/json" \
  -d '{"jsonrpc":"2.0","id":1,"method":"ping"}'
```

### Step 6 — CLI test

```bash
muninn oauth create-client --name cli-test
muninn oauth list
muninn oauth revoke <client_id>
```

### Step 7 — Web UI test

1. Open http://127.0.0.1:8476, login
2. Settings -> OAuth tab
3. Create client, verify credentials shown once
4. Verify client appears in list
5. Revoke client, verify removed

### Step 8 — Revocation lifecycle

```bash
# Create -> exchange -> revoke -> exchange again
# Final exchange must return 401
```

### Expected results

| Test | Expected |
|------|----------|
| Token exchange (POST) | 200 + access_token |
| Token exchange (Basic) | 200 + access_token |
| Token usable for MCP | 200 + tools list |
| Discovery endpoint | JSON metadata |
| Wrong secret | 401 |
| Wrong grant type | 400 |
| JSON Content-Type | 400 |
| Bearer auth regression | 200 |
| List clients | client visible |
| Revoke client | 200 |
| Post-revoke exchange | 401 |

---

## Phase 3: Deploy to Fly.io + Production Smoke Test

### Deploy

```bash
cd /path/to/muninndb-cloud
# Ensure MUNINN_MCP_TOKEN is set in Fly secrets
fly secrets list  # verify MUNINN_MCP_TOKEN exists
fly deploy --remote-only
```

### Smoke tests

```bash
# Discovery (verify HTTPS issuer via Fly.io proxy)
curl -s https://muninndb.fly.dev/.well-known/oauth-authorization-server | jq .
# Expect: issuer = "https://muninndb.fly.dev"

# Create client via admin API (login first for session cookie)
# Then exchange credentials:
curl -v -X POST https://muninndb.fly.dev/mcp/oauth/token \
  -H "Content-Type: application/x-www-form-urlencoded" \
  -d "grant_type=client_credentials&client_id=<ID>&client_secret=<SECRET>"

# Use token for MCP
curl -s -X POST https://muninndb.fly.dev/mcp \
  -H "Authorization: Bearer <access_token>" \
  -H "Content-Type: application/json" \
  -d '{"jsonrpc":"2.0","id":1,"method":"tools/list"}'
```

### Key validations

- Fly.io reverse proxy sets `X-Forwarded-Proto: https` -> issuer URL is `https://`
- Port 8750 routes correctly through Fly.io services
- `/.well-known/oauth-authorization-server` is accessible at the MCP server root

---

## Phase 4: Claude.ai Connector (End-to-End)

This is the real goal.

### Setup

1. Create a dedicated OAuth client:
   ```bash
   muninn oauth create-client --name "claude-ai-connector"
   ```

2. Go to **claude.ai** -> **Settings** -> **Connectors** -> **Add custom connector**

3. Fill in:
   - **Remote MCP server URL:** `https://muninndb.fly.dev/mcp`
   - **OAuth Client ID:** `oc_<...>` (from step 1)
   - **OAuth Client Secret:** `<...>` (from step 1)

4. Save

### Verification

- MuninnDB tools appear in the Claude.ai tool list
- Test: "Remember that OAuth testing was successful on [today's date]"
- Verify the memory was stored via `muninn exec recall "OAuth testing"`
- Test on mobile (iOS/Android Claude app) -- connector available cross-device

### Pass criteria

MuninnDB tools available in Claude.ai on **web AND mobile**.

---

## Risk Areas

| Risk | Mitigation |
|------|------------|
| Fly.io port 8750 not exposed | Check `fly.toml` services -- MCP health check already targets 8750 |
| Claude.ai discovery path differs | May look for `/.well-known/` relative to MCP URL. Test both paths; may need `/mcp/.well-known/oauth-authorization-server` |
| `MUNINN_MCP_TOKEN` not set on Fly | OAuth client creation fails without it -- verify via `fly secrets list` |
| Admin password unknown for Fly | Check `MUNINN_ADMIN_PASSWORD` in Fly secrets or cloud backup workflow credentials |

---

## Files Modified

### New files
- `internal/auth/oauth_store.go` — OAuth client CRUD (Pebble, bcrypt)
- `internal/auth/oauth_store_test.go` — 6 store tests
- `internal/mcp/oauth.go` — Token + discovery endpoints
- `internal/mcp/oauth_test.go` — 14 endpoint tests
- `internal/transport/rest/admin_oauth_handlers.go` — Admin REST API
- `cmd/muninn/oauth.go` — CLI commands

### Modified files
- `internal/auth/types.go` — `OAuthClient` struct
- `internal/auth/keys.go` — Pebble prefix `0x15`
- `internal/mcp/server.go` — `New()` signature, route registration
- `internal/transport/rest/server.go` — `MCPInfo.Token`, admin routes
- `cmd/muninn/server.go` — `mcp.New()` call site
- `cmd/muninn/main.go` — `oauth` subcommand dispatch
- `cmd/muninn/help.go` — help text
- `web/static/js/app.js` — OAuth tab logic
- `web/templates/index.html` — OAuth tab UI
- ~15 test files — `mcp.New()` signature update

---

## Test Results (2026-04-06)

### Phase 1: Unit Tests
- 6/6 auth store tests **PASS**
- 14/14 MCP endpoint tests **PASS**
- 44/45 packages **PASS** (1 pre-existing failure unrelated to OAuth)

### Phase 2: Integration Tests
- 11/11 curl integration scenarios **PASS**
- Token exchange (POST + Basic auth) **PASS**
- OAuth token -> MCP auth **PASS**
- Discovery endpoint **PASS**
- Error handling (401, 400) **PASS**
- Revocation lifecycle **PASS**
- Bearer auth backward compatibility **PASS**
