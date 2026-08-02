package main

import (
	"bytes"
	"encoding/json"
	"flag"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"runtime"
	"strings"
	"time"
)

func runLogin(args []string) {
	fs := flag.NewFlagSet("login", flag.ExitOnError)
	backendURL := fs.String("backend", defaultBackendURL(), "backend base URL")
	configPath := fs.String("config", defaultAuthConfigPath(), "auth config path")
	sessionPath := fs.String("session", defaultSessionConfigPath(), "session config path")
	noBrowser := fs.Bool("no-browser", false, "print login URL instead of opening a browser")
	fs.Parse(args)

	hostname, _ := os.Hostname()
	payload, _ := json.Marshal(map[string]string{"deviceName": hostname})
	resp, err := http.Post(*backendURL+"/api/cli/login/start", "application/json", bytes.NewReader(payload))
	if err != nil {
		fmt.Printf("login failed: %v\n", err)
		os.Exit(1)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		fmt.Printf("login failed with status %d\n", resp.StatusCode)
		os.Exit(1)
	}

	var start cliLoginStartResponse
	if err := json.NewDecoder(resp.Body).Decode(&start); err != nil {
		fmt.Printf("failed to decode login start response: %v\n", err)
		os.Exit(1)
	}
	if start.RequestID == "" || start.PollToken == "" || start.BrowserURL == "" {
		fmt.Println("login start returned an incomplete response")
		os.Exit(1)
	}

	if *noBrowser {
		fmt.Println("Continue login in your browser:")
		fmt.Println(start.BrowserURL)
	} else {
		fmt.Println("Opening browser to continue login...")
		if err := openBrowser(start.BrowserURL); err != nil {
			fmt.Printf("Could not open a browser automatically: %v\n", err)
			fmt.Println("Continue login in your browser:")
			fmt.Println(start.BrowserURL)
		} else {
			fmt.Println("If the browser did not open, visit:")
			fmt.Println(start.BrowserURL)
		}
	}

	result, err := pollCliLogin(*backendURL, start)
	if err != nil {
		fmt.Printf("login failed: %v\n", err)
		os.Exit(1)
	}
	if result.Token == "" {
		fmt.Println("login returned an empty token")
		os.Exit(1)
	}

	_ = configPath
	if err := saveSessionConfig(*sessionPath, sessionConfig{
		BackendURL:  *backendURL,
		Token:       result.Token,
		Email:       result.User.Email,
		Workspace:   result.Workspace.Name,
		Plan:        result.Plan.Name,
		Role:        result.Role,
		Environment: currentEnvironmentName(),
	}); err != nil {
		fmt.Printf("failed to save auth config: %v\n", err)
		os.Exit(1)
	}
	printHumanAuth(*result)
}

func pollCliLogin(backendURL string, start cliLoginStartResponse) (*humanAuthResponse, error) {
	interval := time.Duration(start.PollIntervalSeconds) * time.Second
	if interval <= 0 {
		interval = 2 * time.Second
	}
	deadline := time.Now().Add(10 * time.Minute)
	if parsed, err := time.Parse(time.RFC3339, start.ExpiresAt); err == nil {
		deadline = parsed
	}

	for {
		if time.Now().After(deadline) {
			return nil, fmt.Errorf("browser login timed out")
		}
		body, _ := json.Marshal(map[string]string{
			"requestId": start.RequestID,
			"pollToken": start.PollToken,
		})
		resp, err := http.Post(strings.TrimRight(backendURL, "/")+"/api/cli/login/poll", "application/json", bytes.NewReader(body))
		if err != nil {
			return nil, err
		}
		if resp.StatusCode == http.StatusAccepted {
			_ = resp.Body.Close()
			time.Sleep(interval)
			continue
		}
		if resp.StatusCode != http.StatusOK {
			message := formatHTTPError(resp, "login poll failed")
			return nil, fmt.Errorf("%s", message)
		}
		var result humanAuthResponse
		if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
			_ = resp.Body.Close()
			return nil, err
		}
		_ = resp.Body.Close()
		return &result, nil
	}
}

func openBrowser(target string) error {
	var cmd *exec.Cmd
	switch runtime.GOOS {
	case "darwin":
		cmd = exec.Command("open", target)
	case "windows":
		cmd = exec.Command("rundll32", "url.dll,FileProtocolHandler", target)
	default:
		if _, err := exec.LookPath("wslview"); err == nil {
			cmd = exec.Command("wslview", target)
		} else if _, err := exec.LookPath("xdg-open"); err == nil {
			cmd = exec.Command("xdg-open", target)
		} else {
			return fmt.Errorf("no browser opener found")
		}
	}
	return cmd.Run()
}

func runWhoami(args []string) {
	fs := flag.NewFlagSet("whoami", flag.ExitOnError)
	backendURL := fs.String("backend", defaultBackendURL(), "backend base URL")
	fs.Parse(args)

	token, err := loadAuthToken(*backendURL)
	if err != nil {
		fmt.Println(err)
		os.Exit(1)
	}
	req, err := http.NewRequest(http.MethodGet, *backendURL+"/api/cli/whoami", nil)
	if err != nil {
		fmt.Printf("whoami failed: %v\n", err)
		os.Exit(1)
	}
	req.Header.Set("Authorization", "Bearer "+token)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		fmt.Printf("whoami failed: %v\n", err)
		os.Exit(1)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		fmt.Printf("whoami failed with status %d\n", resp.StatusCode)
		os.Exit(1)
	}
	var result humanAuthResponse
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		fmt.Printf("failed to decode whoami response: %v\n", err)
		os.Exit(1)
	}
	printHumanAuthWithEnvironment(result, *backendURL)
}

func runLogout(args []string) {
	fs := flag.NewFlagSet("logout", flag.ExitOnError)
	backendURL := fs.String("backend", defaultBackendURL(), "backend base URL")
	configPath := fs.String("config", defaultAuthConfigPath(), "auth config path")
	sessionPath := fs.String("session", defaultSessionConfigPath(), "session config path")
	fs.Parse(args)

	session, err := loadSessionConfig(*sessionPath)
	if err == nil && session.Token != "" && (session.BackendURL == "" || session.BackendURL == *backendURL) {
		req, reqErr := http.NewRequest(http.MethodPost, *backendURL+"/api/cli/logout", nil)
		if reqErr == nil {
			req.Header.Set("Authorization", "Bearer "+session.Token)
			resp, doErr := http.DefaultClient.Do(req)
			if doErr == nil {
				_ = resp.Body.Close()
			}
		}
	}
	_ = configPath
	if err := os.Remove(*sessionPath); err != nil && !os.IsNotExist(err) {
		fmt.Printf("failed to remove local login: %v\n", err)
		os.Exit(1)
	}
	fmt.Println("Logged out")
}

func printHumanAuth(result humanAuthResponse) {
	printHumanAuthWithEnvironment(result, "")
}

func printHumanAuthWithEnvironment(result humanAuthResponse, backendURL string) {
	if backendURL == "" {
		backendURL = defaultBackendURL()
	}
	if currentEnvironmentName() != "prod" {
		fmt.Printf("Environment: %s\n", currentEnvironmentName())
		fmt.Printf("API: %s\n", backendURL)
	}
	fmt.Printf("Logged in as %s\n", result.User.Email)
	if strings.TrimSpace(result.Workspace.Name) != "" {
		fmt.Printf("Workspace: %s\n", result.Workspace.Name)
	}
	if currentEnvironmentName() != "prod" && strings.TrimSpace(result.Role) != "" {
		fmt.Printf("Role: %s\n", result.Role)
	}
	if strings.TrimSpace(result.Plan.Name) != "" {
		fmt.Printf("Plan: %s\n", result.Plan.Name)
	}
}
