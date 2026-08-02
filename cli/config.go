package main

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

func defaultBackendURL() string {
	if value := strings.TrimSpace(os.Getenv("NETH_API_URL")); value != "" {
		return value
	}
	if value := strings.TrimSpace(os.Getenv("NETHERA_API_URL")); value != "" {
		return value
	}
	if value := strings.TrimSpace(os.Getenv("NETH_BACKEND_URL")); value != "" {
		return value
	}
	if env, err := currentEnvironment(); err == nil && strings.TrimSpace(env.APIURL) != "" {
		return strings.TrimRight(env.APIURL, "/")
	}
	return "http://127.0.0.1:8081"
}

func defaultAuthConfigPath() string {
	home, err := os.UserHomeDir()
	if err != nil {
		return filepath.Join(".", ".nethera-auth.json")
	}
	return filepath.Join(home, ".config", "nethera", "config.json")
}

func normalizeEnvironmentName(value string) string {
	value = strings.TrimSpace(value)
	if value == "" {
		return "local"
	}
	return value
}

func currentEnvironmentName() string {
	cfg, err := loadAuthConfig(defaultAuthConfigPath())
	if err == nil && strings.TrimSpace(cfg.CurrentEnvironment) != "" {
		return normalizeEnvironmentName(cfg.CurrentEnvironment)
	}
	if value := strings.TrimSpace(os.Getenv("NETHERA_ENV")); value != "" {
		return normalizeEnvironmentName(value)
	}
	if value := strings.TrimSpace(os.Getenv("NETH_ENV")); value != "" {
		return normalizeEnvironmentName(value)
	}
	return "local"
}

func currentEnvironment() (environmentConfig, error) {
	cfg, err := loadAuthConfig(defaultAuthConfigPath())
	if err != nil {
		return environmentConfig{}, err
	}
	name := normalizeEnvironmentName(cfg.CurrentEnvironment)
	if cfg.Environments != nil {
		if env, ok := cfg.Environments[name]; ok {
			return env, nil
		}
	}
	return environmentConfig{}, nil
}

func defaultSessionConfigPath() string {
	home, err := os.UserHomeDir()
	if err != nil {
		return filepath.Join(".", ".nethera-session.json")
	}
	return filepath.Join(home, ".config", "nethera", currentEnvironmentName(), "session.json")
}

func loadAuthToken(backendURL string) (string, error) {
	if value := strings.TrimSpace(os.Getenv("NETH_TOKEN")); value != "" {
		return value, nil
	}
	session, err := loadSessionConfig(defaultSessionConfigPath())
	if err != nil {
		return "", err
	}
	if session.Token != "" {
		if session.BackendURL != "" && session.BackendURL != backendURL {
			return "", fmt.Errorf("saved login is for a different backend")
		}
		return session.Token, nil
	}
	return "", fmt.Errorf("not logged in; run 'neth login' first")
}

func loadAuthConfig(path string) (authConfig, error) {
	var cfg authConfig
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return authConfig{}, nil
		}
		return authConfig{}, err
	}
	if err := json.Unmarshal(data, &cfg); err != nil {
		return authConfig{}, err
	}
	return cfg, nil
}

func saveAuthConfig(path string, cfg authConfig) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	data, err := json.MarshalIndent(cfg, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(path, data, 0o644)
}

func loadSessionConfig(path string) (sessionConfig, error) {
	var cfg sessionConfig
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return sessionConfig{}, nil
		}
		return sessionConfig{}, err
	}
	if err := json.Unmarshal(data, &cfg); err != nil {
		return sessionConfig{}, err
	}
	return cfg, nil
}

func saveSessionConfig(path string, cfg sessionConfig) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	data, err := json.MarshalIndent(cfg, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(path, data, 0o600)
}

func promptInitialCompose(absDir string) (string, string, error) {
	for _, candidate := range []string{"docker-compose.yml", "compose.yml"} {
		candidatePath := filepath.Join(absDir, candidate)
		if _, err := os.Stat(candidatePath); err == nil {
			useImport, err := promptYesNo(fmt.Sprintf("Found %s. Import it into nethera.yml?", candidate))
			if err != nil {
				return "", "", err
			}
			if useImport {
				data, err := os.ReadFile(candidatePath)
				if err != nil {
					return "", "", err
				}
				normalizedCompose, err := normalizeComposeYAML(string(data))
				if err != nil {
					return "", "", err
				}
				return normalizedCompose, fmt.Sprintf("imported and normalized %s", candidate), nil
			}
			break
		}
	}
	return placeholderHelloWorldCompose(), "generated hello world placeholder", nil
}

func promptYesNo(question string) (bool, error) {
	reader := bufio.NewReader(os.Stdin)
	for {
		fmt.Printf("%s [Y/n]: ", question)
		answer, err := reader.ReadString('\n')
		if err != nil {
			return false, err
		}
		trimmed := strings.ToLower(strings.TrimSpace(answer))
		switch trimmed {
		case "", "y", "yes":
			return true, nil
		case "n", "no":
			return false, nil
		default:
			fmt.Println("please answer y or n")
		}
	}
}

func promptYesNoDefaultNo(question string) (bool, error) {
	reader := bufio.NewReader(os.Stdin)
	for {
		fmt.Printf("%s [y/N]: ", question)
		answer, err := reader.ReadString('\n')
		if err != nil {
			return false, err
		}
		trimmed := strings.ToLower(strings.TrimSpace(answer))
		switch trimmed {
		case "", "n", "no":
			return false, nil
		case "y", "yes":
			return true, nil
		default:
			fmt.Println("please answer y or n")
		}
	}
}

func promptLine(prompt string) (string, error) {
	fmt.Print(prompt)
	reader := bufio.NewReader(os.Stdin)
	value, err := reader.ReadString('\n')
	if err != nil && err != io.EOF {
		return "", err
	}
	return strings.TrimSpace(value), nil
}

func promptSecretValue(prompt string) (string, error) {
	fmt.Print(prompt)
	if isTerminalInput() {
		setEcho := exec.Command("stty", "-echo")
		setEcho.Stdin = os.Stdin
		_ = setEcho.Run()
		defer func() {
			restoreEcho := exec.Command("stty", "echo")
			restoreEcho.Stdin = os.Stdin
			_ = restoreEcho.Run()
			fmt.Println()
		}()
	}
	reader := bufio.NewReader(os.Stdin)
	value, err := reader.ReadString('\n')
	if err != nil && err != io.EOF {
		return "", err
	}
	return strings.TrimRight(value, "\r\n"), nil
}

func isTerminalInput() bool {
	info, err := os.Stdin.Stat()
	if err != nil {
		return false
	}
	return info.Mode()&os.ModeCharDevice != 0
}
