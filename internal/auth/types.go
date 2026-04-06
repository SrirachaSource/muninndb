package auth

import "time"

type AdminUser struct {
	Username  string    `json:"username"`
	PassHash  []byte    `json:"pass_hash"`
	CreatedAt time.Time `json:"created_at"`
}

type APIKey struct {
	ID          string     `json:"id"`
	Vault       string     `json:"vault"`
	Label       string     `json:"label"`
	Mode        string     `json:"mode"`      // "full", "observe", or "write" (ingest-only)
	CreatedAt   time.Time  `json:"created_at"`
	StorageHash []byte     `json:"storage_hash"`
	ExpiresAt   *time.Time `json:"expires_at,omitempty"` // nil = never expires
}

type VaultConfig struct {
	Name       string           `json:"name"`
	Public     bool             `json:"public"`
	Plasticity *PlasticityConfig `json:"plasticity,omitempty"` // per-vault cognitive pipeline config
}

// API key mode constants.
const (
	ModeFull    = "full"    // full read + write access
	ModeObserve = "observe" // read-only; cognitive mutations suppressed at engine layer
	ModeWrite   = "write"   // ingest-only; read endpoints blocked at middleware layer
)

// OAuthClient represents an OAuth 2.0 Client Credentials grant client.
// Each client maps to an MCP token — when a valid client authenticates via
// the token endpoint, the returned access token is the existing MCP token.
type OAuthClient struct {
	ID           string    `json:"id"`            // client_id (public identifier)
	Name         string    `json:"name"`           // human-readable label
	SecretHash   []byte    `json:"secret_hash"`    // bcrypt hash of client_secret
	MCPToken     string    `json:"mcp_token"`      // the MCP bearer token this client maps to
	CreatedAt    time.Time `json:"created_at"`
}

type contextKey string

const (
	ContextVault  contextKey = "auth_vault"
	ContextMode   contextKey = "auth_mode"
	ContextAPIKey contextKey = "auth_apikey"
)
