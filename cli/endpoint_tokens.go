package main

import (
	"bytes"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strings"
)

func runEndpoint(args []string) {
	if len(args) == 0 || args[0] != "token" {
		printEndpointUsage()
		os.Exit(1)
	}
	runEndpointTokens(args[1:])
}

func printEndpointUsage() {
	fmt.Println("usage:")
	fmt.Println("  neth endpoint token list [--app <APP>] [--service <SERVICE>]")
	fmt.Println("  neth endpoint token create <SERVICE> [--app <APP>] [--expires-at <ISO_TIME>]")
	fmt.Println("  neth endpoint token revoke <TOKEN_ID> [--yes]")
}

func runEndpointTokens(args []string) {
	if len(args) == 0 {
		printEndpointUsage()
		os.Exit(1)
	}
	switch args[0] {
	case "list":
		runEndpointTokensList(args[1:])
	case "create":
		runEndpointTokensCreate(args[1:])
	case "revoke":
		runEndpointTokensRevoke(args[1:])
	default:
		printEndpointUsage()
		os.Exit(1)
	}
}

func runEndpointTokensList(args []string) {
	fs := flag.NewFlagSet("endpoint token list", flag.ExitOnError)
	backendURL := fs.String("backend", defaultBackendURL(), "backend base URL")
	appOverride := fs.String("app", "", "app id or name")
	serviceName := fs.String("service", "", "compose service name")
	fs.Parse(args)

	token, err := loadAuthToken(*backendURL)
	if err != nil {
		fmt.Println(err)
		os.Exit(1)
	}
	app, err := resolveSecretAppContext(*backendURL, token, *appOverride)
	if err != nil {
		fmt.Println(err)
		os.Exit(1)
	}
	tokens, err := listEndpointAccessTokens(*backendURL, token, app.ID, *serviceName)
	if err != nil {
		fmt.Printf("failed to list endpoint tokens: %v\n", err)
		os.Exit(1)
	}
	fmt.Printf("Endpoint tokens for app: %s\n\n", app.Name)
	if len(tokens) == 0 {
		fmt.Println("No endpoint tokens found.")
		return
	}
	fmt.Printf("%-28s %-18s %-10s %-20s %s\n", "NAME", "SERVICE", "STATUS", "CREATED", "EXPIRES")
	for _, token := range tokens {
		status := "active"
		if strings.TrimSpace(token.RevokedAt) != "" {
			status = "revoked"
		}
		expires := "never"
		if strings.TrimSpace(token.ExpiresAt) != "" {
			expires = formatTimestamp(token.ExpiresAt)
		}
		name := strings.TrimSpace(token.Name)
		if name == "" {
			name = "API token for " + token.ServiceName
		}
		fmt.Printf("%-28s %-18s %-10s %-20s %s\n", truncateTokenListName(name), token.ServiceName, status, formatTimestamp(token.CreatedAt), expires)
	}
}

func truncateTokenListName(name string) string {
	const max = 27
	if len(name) <= max {
		return name
	}
	return name[:max-3] + "..."
}

func runEndpointTokensCreate(args []string) {
	fs := flag.NewFlagSet("endpoint token create", flag.ExitOnError)
	backendURL := fs.String("backend", defaultBackendURL(), "backend base URL")
	appOverride := fs.String("app", "", "app id or name")
	expiresAt := fs.String("expires-at", "", "optional future ISO timestamp")
	name := fs.String("name", "", "optional token name")
	fs.Parse(args)
	serviceName := strings.TrimSpace(fs.Arg(0))
	if serviceName == "" {
		fmt.Println("service name is required")
		os.Exit(1)
	}
	token, err := loadAuthToken(*backendURL)
	if err != nil {
		fmt.Println(err)
		os.Exit(1)
	}
	app, err := resolveSecretAppContext(*backendURL, token, *appOverride)
	if err != nil {
		fmt.Println(err)
		os.Exit(1)
	}
	tokenName := strings.TrimSpace(*name)
	for tokenName == "" {
		var promptErr error
		tokenName, promptErr = promptLine("Token name: ")
		if promptErr != nil {
			fmt.Printf("failed to read token name: %v\n", promptErr)
			os.Exit(1)
		}
		if tokenName == "" {
			fmt.Println("token name is required")
		}
	}
	created, err := createEndpointAccessTokenForService(*backendURL, token, app.ID, serviceName, tokenName, *expiresAt)
	if err != nil {
		fmt.Printf("failed to create endpoint token: %v\n", err)
		os.Exit(1)
	}
	fmt.Printf("Endpoint token created for %s (%s):\n", created.ServiceName, created.Name)
	fmt.Println(created.Token)
	fmt.Println("Store this token securely. Nethera cannot show it again.")
}

func runEndpointTokensRevoke(args []string) {
	fs := flag.NewFlagSet("endpoint token revoke", flag.ExitOnError)
	backendURL := fs.String("backend", defaultBackendURL(), "backend base URL")
	yes := fs.Bool("yes", false, "revoke without confirmation")
	fs.Parse(args)
	tokenID := strings.TrimSpace(fs.Arg(0))
	if tokenID == "" {
		fmt.Println("token id is required")
		os.Exit(1)
	}
	authToken, err := loadAuthToken(*backendURL)
	if err != nil {
		fmt.Println(err)
		os.Exit(1)
	}
	if !*yes {
		confirmed, confirmErr := promptYesNoDefaultNo(fmt.Sprintf("Revoke endpoint token %s?", tokenID))
		if confirmErr != nil {
			fmt.Printf("failed to read confirmation: %v\n", confirmErr)
			os.Exit(1)
		}
		if !confirmed {
			fmt.Println("Endpoint token unchanged.")
			return
		}
	}
	if err := revokeEndpointAccessToken(*backendURL, authToken, tokenID); err != nil {
		fmt.Printf("failed to revoke endpoint token: %v\n", err)
		os.Exit(1)
	}
	fmt.Println("Endpoint token revoked.")
}

func listEndpointAccessTokens(backendURL, token, appID, serviceName string) ([]endpointAccessTokenResult, error) {
	query := ""
	if strings.TrimSpace(serviceName) != "" {
		query = "?serviceName=" + url.QueryEscape(strings.TrimSpace(serviceName))
	}
	req, err := http.NewRequest(http.MethodGet, strings.TrimRight(backendURL, "/")+"/api/apps/"+url.PathEscape(appID)+"/endpoint-tokens"+query, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+token)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("%s", formatHTTPError(resp, "endpoint token list failed"))
	}
	var result endpointAccessTokenResponse
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return nil, err
	}
	return result.Tokens, nil
}

func createEndpointAccessTokenForService(backendURL, token, appID, serviceName, name, expiresAt string) (*endpointAccessTokenResult, error) {
	requestBody := map[string]any{"serviceName": serviceName}
	if strings.TrimSpace(name) != "" {
		requestBody["name"] = strings.TrimSpace(name)
	}
	if strings.TrimSpace(expiresAt) != "" {
		requestBody["expiresAt"] = strings.TrimSpace(expiresAt)
	}
	payload, err := json.Marshal(requestBody)
	if err != nil {
		return nil, err
	}
	req, err := http.NewRequest(http.MethodPost, strings.TrimRight(backendURL, "/")+"/api/apps/"+url.PathEscape(appID)+"/endpoint-tokens", bytes.NewReader(payload))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("%s", formatHTTPError(resp, "endpoint token create failed"))
	}
	var result endpointAccessTokenResponse
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return nil, err
	}
	if len(result.Tokens) == 0 || strings.TrimSpace(result.Tokens[0].Token) == "" {
		return nil, fmt.Errorf("backend did not return a token")
	}
	return &result.Tokens[0], nil
}

func revokeEndpointAccessToken(backendURL, token, tokenID string) error {
	req, err := http.NewRequest(http.MethodPost, strings.TrimRight(backendURL, "/")+"/api/endpoint-tokens/"+url.PathEscape(tokenID)+"/revoke", nil)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+token)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		var errorBody struct {
			Error string `json:"error"`
		}
		if json.Unmarshal(body, &errorBody) == nil && strings.TrimSpace(errorBody.Error) != "" {
			return fmt.Errorf("%s", errorBody.Error)
		}
		return fmt.Errorf("request rejected with status %d", resp.StatusCode)
	}
	return nil
}
