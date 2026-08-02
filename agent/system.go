package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

const (
	jobCompleteMaxLogLines     = 300
	jobCompleteHeadLogLines    = 80
	jobCompleteMaxLogBytes     = 64 * 1024
	jobCompleteMaxLogLineBytes = 2000
)

func formatCommandOutput(output []byte) []string {
	text := strings.TrimSpace(string(output))
	if text == "" {
		return []string{"docker output: <empty>"}
	}
	lines := strings.FieldsFunc(text, func(value rune) bool {
		return value == '\n' || value == '\r'
	})
	formatted := make([]string, 0, len(lines))
	for _, line := range lines {
		trimmed := strings.TrimSpace(line)
		if trimmed == "" {
			continue
		}
		formatted = append(formatted, fmt.Sprintf("docker output: %s", trimmed))
	}
	if len(formatted) == 0 {
		return []string{"docker output: <empty>"}
	}
	return formatted
}

func compactJobCompleteLogs(logs []string) []string {
	if len(logs) == 0 {
		return nil
	}
	compacted := make([]string, 0, len(logs))
	progressOmitted := 0
	progressSeen := 0
	for _, line := range logs {
		line = truncateLogLine(strings.TrimSpace(line), jobCompleteMaxLogLineBytes)
		if line == "" {
			continue
		}
		if isDockerProgressLogLine(line) {
			progressSeen += 1
			if progressSeen == 1 || progressSeen%100 == 0 {
				compacted = append(compacted, line)
			} else {
				progressOmitted += 1
			}
			continue
		}
		compacted = append(compacted, line)
	}
	if progressOmitted > 0 {
		compacted = append(compacted, fmt.Sprintf("omitted %d repetitive Docker progress lines", progressOmitted))
	}
	compacted = boundLogLines(compacted, jobCompleteMaxLogLines, jobCompleteHeadLogLines)
	for logPayloadBytes(compacted) > jobCompleteMaxLogBytes && len(compacted) > 1 {
		removed := len(compacted) / 2
		if removed < 1 {
			removed = 1
		}
		compacted = append([]string{fmt.Sprintf("omitted %d log lines to fit deploy completion payload", removed)}, compacted[removed:]...)
	}
	return compacted
}

func isDockerProgressLogLine(line string) bool {
	body := strings.TrimSpace(strings.TrimPrefix(line, "docker output:"))
	return strings.Contains(body, "Downloading [") ||
		strings.Contains(body, "Extracting [") ||
		strings.Contains(body, "Pulling fs layer") ||
		strings.Contains(body, "Waiting") ||
		strings.Contains(body, "Verifying Checksum")
}

func truncateLogLine(line string, maxBytes int) string {
	if maxBytes <= 0 || len(line) <= maxBytes {
		return line
	}
	return line[:maxBytes] + "... [truncated]"
}

func boundLogLines(lines []string, maxLines int, headLines int) []string {
	if maxLines <= 0 || len(lines) <= maxLines {
		return lines
	}
	if headLines < 0 {
		headLines = 0
	}
	if headLines >= maxLines {
		headLines = maxLines / 2
	}
	tailLines := maxLines - headLines - 1
	if tailLines < 0 {
		tailLines = 0
	}
	omitted := len(lines) - headLines - tailLines
	result := make([]string, 0, maxLines)
	result = append(result, lines[:headLines]...)
	result = append(result, fmt.Sprintf("omitted %d log lines", omitted))
	if tailLines > 0 {
		result = append(result, lines[len(lines)-tailLines:]...)
	}
	return result
}

func logPayloadBytes(lines []string) int {
	total := 0
	for _, line := range lines {
		total += len(line) + 4
	}
	return total
}

func resolveDockerBinary() (string, error) {
	for _, candidate := range []string{"docker", "podman"} {
		if _, err := exec.LookPath(candidate); err == nil {
			return candidate, nil
		}
	}
	return "", fmt.Errorf("docker CLI not found; install Docker Desktop or Docker Engine")
}

func resolveWGQuickBinary() (string, error) {
	if _, err := exec.LookPath("wg-quick"); err == nil {
		return "wg-quick", nil
	}
	return "", fmt.Errorf("wg-quick not found; install wireguard tools")
}

func ensureWireGuardProvisioning(backendURL, token string) (*wireGuardNetworkResponse, error) {
	networkConfig, err := fetchWireGuardNetworkConfig(backendURL, token)
	if err != nil {
		return nil, err
	}
	configText := renderWireGuardConfig(networkConfig)
	configPath, err := writeWireGuardConfig(configText)
	if err != nil {
		return nil, err
	}
	if err := applyWireGuardConfig(configPath); err != nil {
		return nil, err
	}
	return networkConfig, nil
}

func reconcileWireGuardProvisioning(backendURL, token string, current *wireGuardNetworkResponse) (*wireGuardNetworkResponse, bool, error) {
	networkConfig, err := fetchWireGuardNetworkConfig(backendURL, token)
	if err != nil {
		return current, false, err
	}
	if current != nil && wireGuardNetworkConfigEqual(current, networkConfig) && liveWireGuardConfigMatches("wg0", networkConfig) {
		return current, false, nil
	}
	configText := renderWireGuardConfig(networkConfig)
	configPath, err := writeWireGuardConfig(configText)
	if err != nil {
		return current, false, err
	}
	if err := applyWireGuardConfig(configPath); err != nil {
		return current, false, err
	}
	return networkConfig, true, nil
}

func wireGuardNetworkConfigEqual(left, right *wireGuardNetworkResponse) bool {
	if left == nil || right == nil {
		return left == right
	}
	return strings.TrimSpace(left.MachineID) == strings.TrimSpace(right.MachineID) &&
		strings.TrimSpace(left.Interface.PrivateKey) == strings.TrimSpace(right.Interface.PrivateKey) &&
		strings.TrimSpace(left.Interface.Address) == strings.TrimSpace(right.Interface.Address) &&
		strings.TrimSpace(left.Peer.PublicKey) == strings.TrimSpace(right.Peer.PublicKey) &&
		strings.TrimSpace(left.Peer.Endpoint) == strings.TrimSpace(right.Peer.Endpoint) &&
		strings.TrimSpace(left.Peer.AllowedIPs) == strings.TrimSpace(right.Peer.AllowedIPs) &&
		left.Peer.PersistentKeepalive == right.Peer.PersistentKeepalive
}

func liveWireGuardConfigMatches(interfaceName string, config *wireGuardNetworkResponse) bool {
	if config == nil || !wireGuardInterfaceExists(interfaceName) {
		return false
	}
	if !wireGuardPeerFieldMatches(interfaceName, "allowed-ips", config.Peer.PublicKey, config.Peer.AllowedIPs) {
		return false
	}
	if !wireGuardPeerKeepaliveMatches(interfaceName, config.Peer.PublicKey, config.Peer.PersistentKeepalive) {
		return false
	}
	return true
}

func wireGuardPeerFieldMatches(interfaceName, field, publicKey, expected string) bool {
	output, err := wireGuardShow(interfaceName, field)
	if err != nil {
		return false
	}
	expected = strings.TrimSpace(expected)
	for _, line := range strings.Split(output, "\n") {
		parts := strings.Fields(line)
		if len(parts) < 2 || strings.TrimSpace(parts[0]) != strings.TrimSpace(publicKey) {
			continue
		}
		return strings.TrimSpace(strings.Join(parts[1:], " ")) == expected
	}
	return false
}

func wireGuardPeerKeepaliveMatches(interfaceName, publicKey string, expected int) bool {
	output, err := wireGuardShow(interfaceName, "persistent-keepalive")
	if err != nil {
		return false
	}
	for _, line := range strings.Split(output, "\n") {
		parts := strings.Fields(line)
		if len(parts) < 2 || strings.TrimSpace(parts[0]) != strings.TrimSpace(publicKey) {
			continue
		}
		value, err := strconv.Atoi(strings.TrimSpace(parts[1]))
		return err == nil && value == expected
	}
	return false
}

func wireGuardShow(interfaceName, field string) (string, error) {
	cmd := exec.Command("wg", "show", interfaceName, field)
	output, err := cmd.CombinedOutput()
	if err != nil {
		return "", fmt.Errorf("wg show %s %s: %s", interfaceName, field, strings.TrimSpace(string(output)))
	}
	return string(output), nil
}

func fetchWireGuardNetworkConfig(backendURL, token string) (*wireGuardNetworkResponse, error) {
	url := strings.TrimRight(backendURL, "/") + "/api/machines/network"
	req, err := http.NewRequest(http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+token)
	client := &http.Client{Timeout: 30 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		return nil, &httpStatusError{Endpoint: "api/machines/network", Status: resp.StatusCode, Details: summarizeBody(body)}
	}
	var payload wireGuardNetworkResponse
	if err := json.NewDecoder(resp.Body).Decode(&payload); err != nil {
		return nil, err
	}
	if strings.TrimSpace(payload.Interface.PrivateKey) == "" || strings.TrimSpace(payload.Interface.Address) == "" {
		return nil, fmt.Errorf("api/machines/network returned incomplete interface settings")
	}
	if strings.TrimSpace(payload.Peer.PublicKey) == "" || strings.TrimSpace(payload.Peer.Endpoint) == "" || strings.TrimSpace(payload.Peer.AllowedIPs) == "" {
		return nil, fmt.Errorf("api/machines/network returned incomplete peer settings")
	}
	if payload.Peer.PersistentKeepalive <= 0 {
		payload.Peer.PersistentKeepalive = 25
	}
	return &payload, nil
}

func renderWireGuardConfig(config *wireGuardNetworkResponse) string {
	return strings.Join([]string{
		"[Interface]",
		"PrivateKey = " + config.Interface.PrivateKey,
		"Address = " + config.Interface.Address,
		"",
		"[Peer]",
		"PublicKey = " + config.Peer.PublicKey,
		"Endpoint = " + config.Peer.Endpoint,
		"AllowedIPs = " + config.Peer.AllowedIPs,
		fmt.Sprintf("PersistentKeepalive = %d", config.Peer.PersistentKeepalive),
		"",
	}, "\n")
}

func writeWireGuardConfig(configText string) (string, error) {
	for _, targetPath := range []string{defaultSystemWGConfigPath(), defaultUserWGConfigPath()} {
		if err := os.MkdirAll(filepath.Dir(targetPath), 0o755); err != nil {
			continue
		}
		if err := os.WriteFile(targetPath, []byte(configText), 0o600); err != nil {
			continue
		}
		fmt.Printf("wireguard config written: %s\n", targetPath)
		return targetPath, nil
	}
	return "", fmt.Errorf("failed to write wireguard config to system or user path")
}

func applyWireGuardConfig(configPath string) error {
	wgQuickBin, err := resolveWGQuickBinary()
	if err != nil {
		return err
	}

	if wireGuardInterfaceExists("wg0") {
		fmt.Println("wireguard interface wg0 already exists; restarting")
		downCmd := privilegedCommand(wgQuickBin, "down", configPath)
		downCmd.Env = os.Environ()
		downOutput, downErr := downCmd.CombinedOutput()
		if downErr != nil {
			fmt.Printf("wireguard down warning: %s\n", strings.TrimSpace(string(downOutput)))
		}
	}

	upCmd := privilegedCommand(wgQuickBin, "up", configPath)
	upCmd.Env = os.Environ()
	upOutput, upErr := upCmd.CombinedOutput()
	if upErr != nil {
		return fmt.Errorf("wg-quick up failed: %w: %s", upErr, summarizeBody(upOutput))
	}
	if summary := summarizeBody(upOutput); summary != "" {
		fmt.Printf("wireguard up: %s\n", summary)
	}
	return nil
}

func wireGuardInterfaceExists(name string) bool {
	cmd := exec.Command("ip", "link", "show", name)
	err := cmd.Run()
	return err == nil
}

func ensureWireGuardPrivileges() error {
	if os.Geteuid() == 0 {
		return nil
	}
	fmt.Println("wireguard requires elevated privileges; requesting sudo once at startup")
	cmd := exec.Command("sudo", "-v")
	cmd.Env = os.Environ()
	cmd.Stdin = os.Stdin
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("failed to acquire sudo credentials for wireguard: %w", err)
	}
	return nil
}

func privilegedCommand(name string, args ...string) *exec.Cmd {
	if os.Geteuid() == 0 {
		return exec.Command(name, args...)
	}
	sudoArgs := append([]string{"-n", name}, args...)
	return exec.Command("sudo", sudoArgs...)
}

func writeComposeFile(content, jobID string) (string, error) {
	dir := filepath.Join(os.TempDir(), "nethera", jobID)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", err
	}
	path := filepath.Join(dir, "docker-compose.yml")
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		return "", err
	}
	return path, nil
}

func markJobComplete(backendURL, jobID, token, status string, logs []string, endpoints []publicEndpointReport) error {
	logs = compactJobCompleteLogs(logs)
	payload, err := json.Marshal(map[string]any{"status": status, "logs": logs, "endpoints": endpoints})
	if err != nil {
		return err
	}
	req, err := http.NewRequest(http.MethodPost, backendURL+"/deploy/complete?job_id="+jobID, bytes.NewReader(payload))
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Content-Type", "application/json")
	client := &http.Client{Timeout: 30 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		return &httpStatusError{Endpoint: "deploy/complete", Status: resp.StatusCode, Details: summarizeBody(body)}
	}
	var response struct {
		Status string `json:"status"`
		Logs   string `json:"logs"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&response); err == nil && status == "succeeded" && strings.EqualFold(strings.TrimSpace(response.Status), "failed") {
		details := firstJobLogLine(response.Logs)
		if details == "" {
			details = "backend marked deployment failed after completion"
		}
		return fmt.Errorf(details)
	}
	return nil
}

func firstJobLogLine(raw string) string {
	trimmed := strings.TrimSpace(raw)
	if trimmed == "" {
		return ""
	}
	var lines []string
	if err := json.Unmarshal([]byte(trimmed), &lines); err == nil {
		for _, line := range lines {
			if value := strings.TrimSpace(line); value != "" {
				return value
			}
		}
		return ""
	}
	return trimmed
}
