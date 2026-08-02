package main

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
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
	return "http://127.0.0.1:8081"
}

func currentAgentEnvironment() string {
	if value := strings.TrimSpace(os.Getenv("NETHERA_ENV")); value != "" {
		return value
	}
	return "prod"
}

func agentEnvironmentForBackend(backendURL string) string {
	if value := strings.TrimSpace(os.Getenv("NETHERA_ENV")); value != "" {
		return value
	}
	normalized := strings.ToLower(strings.TrimSpace(backendURL))
	if strings.Contains(normalized, "api.staging.nethera.io") || strings.Contains(normalized, ".staging.nethera.io") {
		return "staging"
	}
	if strings.Contains(normalized, "api.nethera.io") {
		return "prod"
	}
	return currentAgentEnvironment()
}

func defaultRegionCode() string {
	if value := strings.TrimSpace(os.Getenv("NETH_REGION")); value != "" {
		return value
	}
	return "sg"
}

func defaultAutoRepairAuth() bool {
	value := strings.TrimSpace(os.Getenv("NETHERA_AGENT_AUTO_REPAIR_AUTH"))
	if value == "" {
		return false
	}
	parsed, err := strconv.ParseBool(value)
	if err != nil {
		return false
	}
	return parsed
}

func defaultMachineConfigPath() string {
	if configDir := strings.TrimSpace(os.Getenv("NETHERA_AGENT_CONFIG_DIR")); configDir != "" {
		return filepath.Join(configDir, "machine.json")
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return filepath.Join(".", ".nethera-machine.json")
	}
	return filepath.Join(home, ".config", "nethera", "machine.json")
}

func defaultSystemWGConfigPath() string {
	return filepath.Join(string(os.PathSeparator), "etc", "nethera", "wg0.conf")
}

func defaultUserWGConfigPath() string {
	home, err := os.UserHomeDir()
	if err != nil {
		return filepath.Join(".", ".config", "nethera", "wg0.conf")
	}
	return filepath.Join(home, ".config", "nethera", "wg0.conf")
}

func mustHostname() string {
	if host, err := os.Hostname(); err == nil && strings.TrimSpace(host) != "" {
		return host
	}
	return "unknown"
}

func loadMachineConfigFile(path string) (machineConfig, error) {
	var cfg machineConfig
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return machineConfig{}, nil
		}
		return machineConfig{}, err
	}
	if err := json.Unmarshal(data, &cfg); err != nil {
		return machineConfig{}, err
	}
	return cfg, nil
}

func loadMachineConfig(path string) (machineConfig, error) {
	cfg, err := loadMachineConfigFile(path)
	if err != nil {
		return machineConfig{}, err
	}
	if cfg.Environment != "" && cfg.Environment != currentAgentEnvironment() {
		return machineConfig{}, fmt.Errorf("this machine is paired with the %s environment, but the agent is configured for %s; re-pair or reinstall the agent for the intended environment", cfg.Environment, currentAgentEnvironment())
	}
	if cfg.APIURL != "" && cfg.BackendURL != "" && strings.TrimRight(cfg.APIURL, "/") != strings.TrimRight(cfg.BackendURL, "/") {
		return machineConfig{}, fmt.Errorf("machine state has mismatched API URLs")
	}
	return cfg, nil
}

func saveMachineConfig(path string, cfg machineConfig) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	data, err := json.MarshalIndent(cfg, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(path, data, 0o600)
}

func defaultDaemonLockPath() string {
	home, err := os.UserHomeDir()
	if err != nil {
		return filepath.Join(".", ".nethera-machine.lock")
	}
	return filepath.Join(home, ".config", "nethera", "machine.lock")
}

func acquireDaemonLock() (func(), error) {
	lockPath := defaultDaemonLockPath()
	if err := os.MkdirAll(filepath.Dir(lockPath), 0o755); err != nil {
		return nil, err
	}

	for attempts := 0; attempts < 2; attempts += 1 {
		file, err := os.OpenFile(lockPath, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
		if err == nil {
			_, _ = file.WriteString(strconv.Itoa(os.Getpid()))
			_ = file.Close()
			return func() {
				_ = os.Remove(lockPath)
			}, nil
		}
		if !os.IsExist(err) {
			return nil, err
		}

		content, readErr := os.ReadFile(lockPath)
		if readErr != nil {
			return nil, readErr
		}
		pidText := strings.TrimSpace(string(content))
		pid, parseErr := strconv.Atoi(pidText)
		if parseErr != nil || pid <= 0 {
			_ = os.Remove(lockPath)
			continue
		}
		if processExists(pid) {
			return nil, fmt.Errorf("another machine process is already running (pid %d)", pid)
		}
		_ = os.Remove(lockPath)
	}

	return nil, fmt.Errorf("failed to acquire daemon lock")
}

func processExists(pid int) bool {
	process, err := os.FindProcess(pid)
	if err != nil {
		return false
	}
	err = process.Signal(syscall.Signal(0))
	return err == nil
}
