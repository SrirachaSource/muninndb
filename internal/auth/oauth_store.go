package auth

import (
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/cockroachdb/pebble"
	"golang.org/x/crypto/bcrypt"
)

// ErrOAuthClientNotFound is returned when a client_id does not exist.
var ErrOAuthClientNotFound = errors.New("oauth client not found")

// ErrOAuthInvalidCredentials is returned when client_secret is wrong.
var ErrOAuthInvalidCredentials = errors.New("invalid client credentials")

// CreateOAuthClient generates a new OAuth client_id + client_secret pair.
// The mcpToken is the existing MCP bearer token that will be returned when
// the client authenticates via the token endpoint.
// Returns the raw client_secret (shown once) and the client metadata.
func (s *Store) CreateOAuthClient(name, mcpToken string) (clientID, clientSecret string, client OAuthClient, err error) {
	// Generate client_id: 16 random bytes, base64url-encoded, prefixed with "oc_".
	idBytes := make([]byte, 16)
	if _, err = rand.Read(idBytes); err != nil {
		err = fmt.Errorf("generate client_id: %w", err)
		return
	}
	clientID = "oc_" + base64.RawURLEncoding.EncodeToString(idBytes)

	// Generate client_secret: 32 random bytes, base64url-encoded.
	secretBytes := make([]byte, 32)
	if _, err = rand.Read(secretBytes); err != nil {
		err = fmt.Errorf("generate client_secret: %w", err)
		return
	}
	clientSecret = base64.RawURLEncoding.EncodeToString(secretBytes)

	// Hash the secret with bcrypt for storage.
	secretHash, hashErr := bcrypt.GenerateFromPassword([]byte(clientSecret), bcrypt.DefaultCost)
	if hashErr != nil {
		err = fmt.Errorf("hash client_secret: %w", hashErr)
		return
	}

	client = OAuthClient{
		ID:         clientID,
		Name:       name,
		SecretHash: secretHash,
		MCPToken:   mcpToken,
		CreatedAt:  time.Now(),
	}

	data, marshalErr := json.Marshal(client)
	if marshalErr != nil {
		err = fmt.Errorf("marshal oauth client: %w", marshalErr)
		return
	}

	if setErr := s.db.Set(oauthClientKey(clientID), data, pebble.Sync); setErr != nil {
		err = fmt.Errorf("persist oauth client: %w", setErr)
		return
	}
	return
}

// dummyBcryptHash is used to equalize timing when a client_id is not found.
// Without this, an attacker can enumerate valid client_ids by measuring
// response time (not-found = fast, wrong-secret = slow bcrypt).
var dummyBcryptHash, _ = bcrypt.GenerateFromPassword([]byte("timing-equalization"), bcrypt.DefaultCost)

// ValidateOAuthClient authenticates a client_id + client_secret pair.
// Returns the client metadata (including the MCP token to issue) on success.
func (s *Store) ValidateOAuthClient(clientID, clientSecret string) (OAuthClient, error) {
	data, closer, err := s.db.Get(oauthClientKey(clientID))
	if err != nil {
		// Perform a dummy bcrypt comparison to equalize timing with the
		// "found but wrong secret" path, preventing client_id enumeration.
		bcrypt.CompareHashAndPassword(dummyBcryptHash, []byte(clientSecret))
		return OAuthClient{}, ErrOAuthClientNotFound
	}
	defer closer.Close()

	var client OAuthClient
	if err := json.Unmarshal(data, &client); err != nil {
		return OAuthClient{}, fmt.Errorf("corrupt oauth client record: %w", err)
	}

	if err := bcrypt.CompareHashAndPassword(client.SecretHash, []byte(clientSecret)); err != nil {
		return OAuthClient{}, ErrOAuthInvalidCredentials
	}
	return client, nil
}

// ListOAuthClients returns all OAuth client metadata (secrets not included).
func (s *Store) ListOAuthClients() ([]OAuthClient, error) {
	prefix := []byte{prefixOAuthClient}
	iter, err := s.db.NewIter(&pebble.IterOptions{
		LowerBound: prefix,
		UpperBound: oauthClientUpperBound(),
	})
	if err != nil {
		return nil, fmt.Errorf("new iter: %w", err)
	}
	defer iter.Close()

	var clients []OAuthClient
	for iter.First(); iter.Valid(); iter.Next() {
		var client OAuthClient
		if jsonErr := json.Unmarshal(iter.Value(), &client); jsonErr == nil {
			client.SecretHash = nil // never leak the hash
			client.MCPToken = ""   // never leak the token
			clients = append(clients, client)
		}
	}
	return clients, iter.Error()
}

// RevokeOAuthClient removes an OAuth client by client_id.
func (s *Store) RevokeOAuthClient(clientID string) error {
	key := oauthClientKey(clientID)
	_, closer, err := s.db.Get(key)
	if err != nil {
		return ErrOAuthClientNotFound
	}
	closer.Close()
	return s.db.Delete(key, pebble.Sync)
}
