package auth

import (
	"testing"
)

func TestOAuthClient_CreateAndValidate(t *testing.T) {
	s := NewStore(openAuthTestDB(t))

	clientID, clientSecret, client, err := s.CreateOAuthClient("test-client", "mdb_test_token")
	if err != nil {
		t.Fatalf("CreateOAuthClient: %v", err)
	}
	if clientID == "" {
		t.Fatal("expected non-empty client_id")
	}
	if clientSecret == "" {
		t.Fatal("expected non-empty client_secret")
	}
	if client.Name != "test-client" {
		t.Errorf("expected name 'test-client', got %q", client.Name)
	}
	if client.MCPToken != "mdb_test_token" {
		t.Errorf("expected mcp_token 'mdb_test_token', got %q", client.MCPToken)
	}

	// Validate with correct credentials.
	got, err := s.ValidateOAuthClient(clientID, clientSecret)
	if err != nil {
		t.Fatalf("ValidateOAuthClient: %v", err)
	}
	if got.MCPToken != "mdb_test_token" {
		t.Errorf("expected mcp_token 'mdb_test_token', got %q", got.MCPToken)
	}
}

func TestOAuthClient_InvalidSecret(t *testing.T) {
	s := NewStore(openAuthTestDB(t))

	clientID, _, _, err := s.CreateOAuthClient("test-client", "mdb_test_token")
	if err != nil {
		t.Fatalf("CreateOAuthClient: %v", err)
	}

	_, err = s.ValidateOAuthClient(clientID, "wrong-secret")
	if err == nil {
		t.Fatal("expected error for wrong secret")
	}
	if err != ErrOAuthInvalidCredentials {
		t.Errorf("expected ErrOAuthInvalidCredentials, got %v", err)
	}
}

func TestOAuthClient_NotFound(t *testing.T) {
	s := NewStore(openAuthTestDB(t))

	_, err := s.ValidateOAuthClient("oc_nonexistent", "any-secret")
	if err == nil {
		t.Fatal("expected error for non-existent client")
	}
	if err != ErrOAuthClientNotFound {
		t.Errorf("expected ErrOAuthClientNotFound, got %v", err)
	}
}

func TestOAuthClient_ListAndRevoke(t *testing.T) {
	s := NewStore(openAuthTestDB(t))

	// Create two clients.
	id1, _, _, _ := s.CreateOAuthClient("client-1", "mdb_tok1")
	s.CreateOAuthClient("client-2", "mdb_tok2")

	clients, err := s.ListOAuthClients()
	if err != nil {
		t.Fatalf("ListOAuthClients: %v", err)
	}
	if len(clients) != 2 {
		t.Fatalf("expected 2 clients, got %d", len(clients))
	}
	// Verify secrets and tokens are not leaked.
	for _, c := range clients {
		if c.SecretHash != nil {
			t.Error("SecretHash should be nil in list output")
		}
		if c.MCPToken != "" {
			t.Error("MCPToken should be empty in list output")
		}
	}

	// Revoke first client.
	if err := s.RevokeOAuthClient(id1); err != nil {
		t.Fatalf("RevokeOAuthClient: %v", err)
	}

	clients, err = s.ListOAuthClients()
	if err != nil {
		t.Fatalf("ListOAuthClients after revoke: %v", err)
	}
	if len(clients) != 1 {
		t.Fatalf("expected 1 client after revoke, got %d", len(clients))
	}
}

func TestOAuthClient_RevokeNotFound(t *testing.T) {
	s := NewStore(openAuthTestDB(t))

	err := s.RevokeOAuthClient("oc_nonexistent")
	if err != ErrOAuthClientNotFound {
		t.Errorf("expected ErrOAuthClientNotFound, got %v", err)
	}
}

func TestOAuthClient_IDPrefix(t *testing.T) {
	s := NewStore(openAuthTestDB(t))

	clientID, _, _, err := s.CreateOAuthClient("prefix-test", "mdb_tok")
	if err != nil {
		t.Fatalf("CreateOAuthClient: %v", err)
	}
	if len(clientID) < 4 || clientID[:3] != "oc_" {
		t.Errorf("expected client_id to start with 'oc_', got %q", clientID)
	}
}
