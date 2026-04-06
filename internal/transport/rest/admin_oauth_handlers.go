package rest

import (
	"encoding/json"
	"errors"
	"net/http"

	"github.com/scrypster/muninndb/internal/auth"
)

// handleCreateOAuthClient handles POST /api/admin/oauth/clients.
// Creates a new OAuth 2.0 client_id + client_secret pair mapped to the
// server's MCP token, for use with the Client Credentials grant.
func (s *Server) handleCreateOAuthClient(authStore *auth.Store, mcpToken string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			Name string `json:"name"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			s.sendError(r, w, http.StatusBadRequest, ErrInvalidEngram, "invalid request body")
			return
		}
		if req.Name == "" {
			s.sendError(r, w, http.StatusBadRequest, ErrInvalidEngram, "name is required")
			return
		}
		if len(req.Name) > 256 {
			s.sendError(r, w, http.StatusBadRequest, ErrInvalidEngram, "name too long (max 256)")
			return
		}
		if mcpToken == "" {
			s.sendError(r, w, http.StatusBadRequest, ErrInvalidEngram,
				"cannot create OAuth client: MCP token auth is not configured (set --mcp-token or MUNINN_MCP_TOKEN)")
			return
		}

		clientID, clientSecret, client, err := authStore.CreateOAuthClient(req.Name, mcpToken)
		if err != nil {
			s.sendError(r, w, http.StatusInternalServerError, ErrStorageError, "failed to create OAuth client")
			return
		}

		// SecretHash and MCPToken must never leak to API consumers.
		client.SecretHash = nil
		client.MCPToken = ""

		s.sendJSON(w, http.StatusCreated, map[string]any{
			"client_id":     clientID,
			"client_secret": clientSecret,
			"client":        client,
		})
	}
}

// handleListOAuthClients handles GET /api/admin/oauth/clients.
func (s *Server) handleListOAuthClients(authStore *auth.Store) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		clients, err := authStore.ListOAuthClients()
		if err != nil {
			s.sendError(r, w, http.StatusInternalServerError, ErrStorageError, "failed to list OAuth clients")
			return
		}
		if clients == nil {
			clients = []auth.OAuthClient{}
		}
		s.sendJSON(w, http.StatusOK, map[string]any{"clients": clients})
	}
}

// handleRevokeOAuthClient handles DELETE /api/admin/oauth/clients/{id}.
func (s *Server) handleRevokeOAuthClient(authStore *auth.Store) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		clientID := r.PathValue("id")
		if clientID == "" {
			s.sendError(r, w, http.StatusBadRequest, ErrInvalidEngram, "client_id is required")
			return
		}

		if err := authStore.RevokeOAuthClient(clientID); err != nil {
			if errors.Is(err, auth.ErrOAuthClientNotFound) {
				s.sendError(r, w, http.StatusNotFound, ErrEngramNotFound, "OAuth client not found")
				return
			}
			s.sendError(r, w, http.StatusInternalServerError, ErrStorageError, err.Error())
			return
		}
		s.sendJSON(w, http.StatusOK, map[string]any{"revoked": clientID})
	}
}
