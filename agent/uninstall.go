package main

import (
	"bufio"
	"bytes"
	"encoding/json"
	stderrors "errors"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

const (
	defaultAgentEnvPath     = "/etc/nethera/agent.env"
	defaultAgentServicePath = "/etc/systemd/system/nethera-agent.service"
	defaultAgentBinaryPath  = "/usr/local/bin/nethera-agent"
)

func runUninstall(args []string) {
	fs := flag.NewFlagSet("uninstall", flag.ExitOnError)
	backendURL := fs.String("backend", "", "backend base URL")
	configPath := fs.String("config", "", "machine config path")
	envPath := fs.String("env-file", defaultAgentEnvPath, "agent environment file")
	force := fs.Bool("force", false, "skip confirmation prompt")
	keepBinary := fs.Bool("keep-binary", false, "do not remove /usr/local/bin/nethera-agent")
	keepState := fs.Bool("keep-state", false, "do not remove /var/lib/nethera")
	fs.Parse(args)

	if os.Geteuid() != 0 {
		fmt.Fprintln(os.Stderr, "nethera-agent uninstall must be run as root.")
		fmt.Fprintln(os.Stderr, "Try:")
		fmt.Fprintln(os.Stderr, "  sudo nethera-agent uninstall")
		os.Exit(1)
	}

	if err := loadAgentEnvFile(*envPath); err != nil {
		fmt.Fprintf(os.Stderr, "failed to read %s: %v\n", *envPath, err)
		os.Exit(1)
	}
	if strings.TrimSpace(*backendURL) == "" {
		*backendURL = defaultBackendURL()
	}
	if strings.TrimSpace(*configPath) == "" {
		*configPath = defaultMachineConfigPath()
	}

	cfg, err := loadMachineConfigFile(*configPath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "failed to read machine config: %v\n", err)
		os.Exit(1)
	}

	if !*force {
		confirmed, err := confirmAgentUninstall(*backendURL, *configPath, cfg.MachineID, *keepBinary, *keepState)
		if err != nil {
			fmt.Fprintf(os.Stderr, "failed to read confirmation: %v\n", err)
			os.Exit(1)
		}
		if !confirmed {
			fmt.Println("uninstall cancelled")
			os.Exit(1)
		}
	}

	fmt.Println("Stopping Nethera agent service...")
	if err := systemctlIfAvailable("disable", "--now", "nethera-agent"); err != nil {
		fmt.Printf("warning: failed to disable service: %v\n", err)
	}

	fmt.Println("Stopping Nethera-managed Docker Compose projects...")
	cleanupLogs, cleanupErr := cleanupManagedDeployments()
	for _, line := range cleanupLogs {
		fmt.Printf("  %s\n", line)
	}
	if cleanupErr != nil {
		fmt.Fprintf(os.Stderr, "failed to stop Nethera-managed apps: %v\n", cleanupErr)
		fmt.Fprintln(os.Stderr, "No local files were removed. Fix the issue and rerun uninstall, or use Docker manually if needed.")
		os.Exit(1)
	}

	fmt.Println("Removing WireGuard interface/config...")
	if err := uninstallWireGuard(); err != nil {
		fmt.Printf("warning: WireGuard cleanup did not complete: %v\n", err)
	}

	if cfg.MachineID != "" && cfg.MachineToken != "" {
		fmt.Println("Deregistering machine with Nethera...")
		if err := deregisterMachineForUninstall(*backendURL, cfg); err != nil {
			fmt.Fprintf(os.Stderr, "failed to deregister machine with backend: %v\n", err)
			fmt.Fprintln(os.Stderr, "Local apps were stopped, but config was kept so you can rerun uninstall after connectivity is restored.")
			os.Exit(1)
		}
	} else {
		fmt.Println("No machine credentials found; skipping backend deregistration.")
	}

	fmt.Println("Removing systemd service...")
	if err := removeAgentService(); err != nil {
		fmt.Printf("warning: failed to remove systemd service: %v\n", err)
	}

	fmt.Println("Removing Nethera config and state...")
	if err := removeAgentFiles(*keepState); err != nil {
		fmt.Fprintf(os.Stderr, "failed to remove Nethera files: %v\n", err)
		os.Exit(1)
	}

	if !*keepBinary {
		fmt.Println("Removing nethera-agent binary...")
		if err := os.Remove(defaultAgentBinaryPath); err != nil && !os.IsNotExist(err) {
			fmt.Printf("warning: failed to remove %s: %v\n", defaultAgentBinaryPath, err)
		}
	}

	fmt.Println("Nethera agent uninstalled.")
}

func confirmAgentUninstall(backendURL, configPath, machineID string, keepBinary bool, keepState bool) (bool, error) {
	fmt.Println("This will fully uninstall the Nethera agent from this machine.")
	fmt.Println()
	fmt.Println("It will:")
	fmt.Println(" - stop and disable nethera-agent.service")
	fmt.Println(" - stop Nethera-managed Docker Compose projects with docker compose down")
	fmt.Println(" - deregister this machine with the Nethera backend")
	fmt.Println(" - remove WireGuard config managed by Nethera")
	fmt.Println(" - remove /etc/nethera")
	if !keepState {
		fmt.Println(" - remove /var/lib/nethera")
	}
	if !keepBinary {
		fmt.Println(" - remove /usr/local/bin/nethera-agent")
	}
	fmt.Println()
	fmt.Printf("Backend: %s\n", backendURL)
	fmt.Printf("Machine config: %s\n", configPath)
	if strings.TrimSpace(machineID) != "" {
		fmt.Printf("Machine ID: %s\n", machineID)
	}
	fmt.Println()
	fmt.Println("Non-Nethera Docker containers and Docker volumes are not removed.")
	return promptDefaultNo("Continue with full uninstall?")
}

func promptDefaultNo(question string) (bool, error) {
	reader := bufio.NewReader(os.Stdin)
	for {
		fmt.Printf("%s [y/N]: ", question)
		answer, err := reader.ReadString('\n')
		if err != nil {
			return false, err
		}
		switch strings.ToLower(strings.TrimSpace(answer)) {
		case "y", "yes":
			return true, nil
		case "", "n", "no":
			return false, nil
		default:
			fmt.Println("please answer y or n")
		}
	}
}

func loadAgentEnvFile(path string) error {
	if strings.TrimSpace(path) == "" {
		return nil
	}
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return err
	}
	for _, line := range strings.Split(string(data), "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		parts := strings.SplitN(line, "=", 2)
		if len(parts) != 2 {
			continue
		}
		key := strings.TrimSpace(parts[0])
		value := strings.Trim(strings.TrimSpace(parts[1]), `"'`)
		if key == "" || os.Getenv(key) != "" {
			continue
		}
		_ = os.Setenv(key, value)
	}
	return nil
}

func systemctlIfAvailable(args ...string) error {
	if _, err := exec.LookPath("systemctl"); err != nil {
		return nil
	}
	cmd := exec.Command("systemctl", args...)
	output, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("systemctl %s: %w: %s", strings.Join(args, " "), err, summarizeBody(output))
	}
	return nil
}

func uninstallWireGuard() error {
	var errors []string
	if wireGuardInterfaceExists("wg0") {
		configPaths := []string{}
		for _, path := range []string{defaultSystemWGConfigPath(), defaultUserWGConfigPath()} {
			if _, statErr := os.Stat(path); statErr == nil {
				configPaths = append(configPaths, path)
			}
		}
		if wgQuick, err := resolveWGQuickBinary(); err == nil {
			for _, arg := range configPaths {
				cmd := exec.Command(wgQuick, "down", arg)
				output, runErr := cmd.CombinedOutput()
				if runErr == nil {
					break
				}
				errors = append(errors, fmt.Sprintf("wg-quick down %s: %v: %s", arg, runErr, summarizeBody(output)))
			}
		} else if len(configPaths) > 0 {
			errors = append(errors, err.Error())
		}
		if wireGuardInterfaceExists("wg0") {
			cmd := privilegedCommand("ip", "link", "delete", "wg0")
			output, runErr := cmd.CombinedOutput()
			if runErr != nil {
				errors = append(errors, fmt.Sprintf("ip link delete wg0: %v: %s", runErr, summarizeBody(output)))
			}
		}
	}
	for _, path := range []string{defaultSystemWGConfigPath(), defaultUserWGConfigPath()} {
		if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
			errors = append(errors, fmt.Sprintf("remove %s: %v", path, err))
		}
	}
	if len(errors) > 0 {
		return stderrors.New(strings.Join(errors, "; "))
	}
	return nil
}

func deregisterMachineForUninstall(backendURL string, cfg machineConfig) error {
	payload := map[string]any{
		"reason": "agent_uninstall",
	}
	body, err := json.Marshal(payload)
	if err != nil {
		return err
	}
	req, err := http.NewRequest(http.MethodPost, strings.TrimRight(backendURL, "/")+"/api/agent/uninstall", bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+cfg.MachineToken)
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		data, _ := io.ReadAll(io.LimitReader(resp.Body, 2048))
		return fmt.Errorf("backend returned %d: %s", resp.StatusCode, strings.TrimSpace(string(data)))
	}
	return nil
}

func removeAgentService() error {
	var errors []string
	if err := systemctlIfAvailable("reset-failed", "nethera-agent"); err != nil {
		errors = append(errors, err.Error())
	}
	if err := os.Remove(defaultAgentServicePath); err != nil && !os.IsNotExist(err) {
		errors = append(errors, fmt.Sprintf("remove %s: %v", defaultAgentServicePath, err))
	}
	if err := systemctlIfAvailable("daemon-reload"); err != nil {
		errors = append(errors, err.Error())
	}
	if len(errors) > 0 {
		return stderrors.New(strings.Join(errors, "; "))
	}
	return nil
}

func removeAgentFiles(keepState bool) error {
	var errors []string
	for _, path := range []string{"/etc/nethera", filepath.Join(os.TempDir(), "nethera")} {
		if err := os.RemoveAll(path); err != nil {
			errors = append(errors, fmt.Sprintf("remove %s: %v", path, err))
		}
	}
	if !keepState {
		if err := os.RemoveAll(netheraStateDir()); err != nil {
			errors = append(errors, fmt.Sprintf("remove %s: %v", netheraStateDir(), err))
		}
	}
	if len(errors) > 0 {
		return stderrors.New(strings.Join(errors, "; "))
	}
	return nil
}
