package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"
)

func printOAuthUsage() {
	fmt.Println("Usage: muninn oauth <command> [flags]")
	fmt.Println()
	fmt.Println("Commands:")
	fmt.Println("  create-client  --name <name>            Create a new OAuth client (credentials shown once)")
	fmt.Println("  list                                    List OAuth clients")
	fmt.Println("  revoke         <client-id>              Revoke an OAuth client")
	fmt.Println()
	fmt.Println("Auth flags (MySQL-style, optional):")
	fmt.Println("  -u <user>         Admin username (default: root)")
	fmt.Println("  -p                Prompt for password")
	fmt.Println("  -p<password>      Inline password (no space)")
	fmt.Println("  -h <host:port>    Server host:port (default: 127.0.0.1:8475)")
	fmt.Println()
	fmt.Println("Example:")
	fmt.Println("  muninn oauth create-client --name claude-ai")
	fmt.Println("  # Then paste client_id + client_secret into Claude.ai connector settings")
}

func runOAuth(args []string) {
	if len(args) == 0 {
		printOAuthUsage()
		return
	}

	remaining, username, password, prompted := parseAdminFlags(args)
	if len(remaining) == 0 {
		printOAuthUsage()
		return
	}

	sub := remaining[0]
	subArgs := remaining[1:]

	switch sub {
	case "create-client":
	case "list":
	case "revoke":
	default:
		fmt.Printf("Unknown oauth command: %q\n", sub)
		printOAuthUsage()
		return
	}

	if err := authenticateAdmin(username, password, prompted); err != nil {
		fmt.Fprintf(os.Stderr, "Authentication failed: %v\n", err)
		fmt.Fprintln(os.Stderr, "Is muninn running? Try: muninn status")
		osExit(1)
		return
	}

	switch sub {
	case "create-client":
		runOAuthCreateClient(subArgs)
	case "list":
		runOAuthList()
	case "revoke":
		runOAuthRevoke(subArgs)
	}
}

func runOAuthCreateClient(args []string) {
	var name string

	for i := 0; i < len(args); i++ {
		a := args[i]
		switch {
		case a == "--name" || a == "-n":
			if i+1 < len(args) {
				i++
				name = args[i]
			}
		case strings.HasPrefix(a, "--name="):
			name = strings.TrimPrefix(a, "--name=")
		case !strings.HasPrefix(a, "-") && name == "":
			name = a
		}
	}

	if name == "" {
		fmt.Println("Usage: muninn oauth create-client --name <name>")
		return
	}

	body := map[string]any{"name": name}
	bodyBytes, err := json.Marshal(body)
	if err != nil {
		fmt.Printf("Error: %v\n", err)
		return
	}

	client := &http.Client{Timeout: 10 * time.Second}
	req, err := http.NewRequest("POST",
		fmt.Sprintf("%s/api/admin/oauth/clients", vaultAdminBase),
		bytes.NewReader(bodyBytes))
	if err != nil {
		fmt.Printf("Error: %v\n", err)
		return
	}
	req.Header.Set("Content-Type", "application/json")
	addSessionCookie(req)

	resp, err := client.Do(req)
	if err != nil {
		fmt.Printf("Error connecting to MuninnDB: %v\n", err)
		fmt.Println("Is muninn running? Try: muninn status")
		return
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusCreated {
		printHTTPError(resp)
		return
	}

	var result struct {
		ClientID     string `json:"client_id"`
		ClientSecret string `json:"client_secret"`
		Client       struct {
			ID        string    `json:"id"`
			Name      string    `json:"name"`
			CreatedAt time.Time `json:"created_at"`
		} `json:"client"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		fmt.Printf("Error parsing response: %v\n", err)
		return
	}

	fmt.Println()
	fmt.Println("  OAuth client created.")
	fmt.Println()
	fmt.Printf("  Client ID    : %s\n", result.ClientID)
	fmt.Printf("  Client Secret: %s\n", result.ClientSecret)
	fmt.Println()
	fmt.Println("  IMPORTANT: The client secret will NOT be shown again. Copy it now.")
	fmt.Println()
	fmt.Printf("  Name   : %s\n", result.Client.Name)
	fmt.Printf("  Created: %s\n", result.Client.CreatedAt.Format(time.RFC3339))
	fmt.Println()
	fmt.Println("  To add as a Claude.ai connector:")
	fmt.Println("    1. Go to Settings -> Connectors -> Add custom connector")
	fmt.Println("    2. URL: https://<your-muninndb-host>/mcp")
	fmt.Printf("    3. Client ID: %s\n", result.ClientID)
	fmt.Println("    4. Client Secret: <paste the secret above>")
	fmt.Println()
}

func runOAuthList() {
	client := &http.Client{Timeout: 10 * time.Second}
	req, err := http.NewRequest("GET",
		fmt.Sprintf("%s/api/admin/oauth/clients", vaultAdminBase), nil)
	if err != nil {
		fmt.Printf("Error: %v\n", err)
		return
	}
	addSessionCookie(req)

	resp, err := client.Do(req)
	if err != nil {
		fmt.Printf("Error connecting to MuninnDB: %v\n", err)
		fmt.Println("Is muninn running? Try: muninn status")
		return
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		printHTTPError(resp)
		return
	}

	var result struct {
		Clients []struct {
			ID        string    `json:"id"`
			Name      string    `json:"name"`
			CreatedAt time.Time `json:"created_at"`
		} `json:"clients"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		fmt.Printf("Error parsing response: %v\n", err)
		return
	}

	if len(result.Clients) == 0 {
		fmt.Println("  No OAuth clients found.")
		return
	}

	fmt.Println()
	fmt.Printf("  %-28s  %-20s  %s\n", "Client ID", "Name", "Created")
	fmt.Printf("  %s\n", strings.Repeat("-", 72))
	for _, c := range result.Clients {
		fmt.Printf("  %-28s  %-20s  %s\n",
			c.ID,
			c.Name,
			c.CreatedAt.Format("2006-01-02 15:04:05"),
		)
	}
	fmt.Printf("\n  %d client(s)\n", len(result.Clients))
}

func runOAuthRevoke(args []string) {
	var clientID string

	for i := 0; i < len(args); i++ {
		a := args[i]
		if !strings.HasPrefix(a, "-") && clientID == "" {
			clientID = a
		}
	}

	if clientID == "" {
		fmt.Println("Usage: muninn oauth revoke <client-id>")
		return
	}

	client := &http.Client{Timeout: 10 * time.Second}
	req, err := http.NewRequest("DELETE",
		fmt.Sprintf("%s/api/admin/oauth/clients/%s", vaultAdminBase, url.PathEscape(clientID)), nil)
	if err != nil {
		fmt.Printf("Error: %v\n", err)
		return
	}
	addSessionCookie(req)

	resp, err := client.Do(req)
	if err != nil {
		fmt.Printf("Error connecting to MuninnDB: %v\n", err)
		fmt.Println("Is muninn running? Try: muninn status")
		return
	}
	defer resp.Body.Close()

	switch resp.StatusCode {
	case http.StatusOK:
		fmt.Println("  OAuth client revoked.")
	case http.StatusNotFound:
		fmt.Printf("  OAuth client %q not found.\n", clientID)
	case http.StatusUnauthorized:
		fmt.Println("  Not authenticated. Use -u <user> -p to authenticate.")
	default:
		printHTTPError(resp)
	}
}
