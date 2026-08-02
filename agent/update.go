package main

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

const agentUpdateStateFile = "agent-update.json"

func shouldAttemptAgentUpdate(update agentUpdatePayload) (bool, string) {
	if !update.Available {
		return false, ""
	}
	if compareAgentVersions(update.Version, agentVersion()) <= 0 {
		return false, fmt.Sprintf("ignoring agent update to %s because current version is %s", update.Version, agentVersion())
	}
	state, _ := loadAgentUpdateState()
	if state.NextAttemptAfter != "" {
		nextAttempt, err := time.Parse(time.RFC3339, state.NextAttemptAfter)
		if err == nil && time.Now().UTC().Before(nextAttempt) {
			return false, fmt.Sprintf("agent update retry delayed until %s", state.NextAttemptAfter)
		}
	}
	return true, ""
}

func performAgentUpdate(update agentUpdatePayload) error {
	fmt.Printf("Agent update required: %s\n", strings.TrimSpace(update.Version))
	if err := validateAgentUpdatePayload(update); err != nil {
		return recordAgentUpdateFailure(err)
	}
	if err := recordAgentUpdateAttempt(); err != nil {
		fmt.Printf("agent update state warning: %v\n", err)
	}
	fmt.Printf("Downloading agent update %s\n", strings.TrimSpace(update.Version))
	tempPath, err := downloadAgentUpdate(update.URL)
	if err != nil {
		return recordAgentUpdateFailure(fmt.Errorf("download failed"))
	}
	defer os.Remove(tempPath)
	if err := verifyAgentUpdateChecksum(tempPath, update.SHA256); err != nil {
		return recordAgentUpdateFailure(fmt.Errorf("checksum verification failed"))
	}
	fmt.Println("Agent update checksum verified")
	if err := installAgentUpdate(tempPath); err != nil {
		return recordAgentUpdateFailure(fmt.Errorf("install failed"))
	}
	if err := clearAgentUpdateState(); err != nil {
		fmt.Printf("agent update state warning: %v\n", err)
	}
	fmt.Println("Agent update installed; restarting service")
	if err := restartAgentService(); err != nil {
		return recordAgentUpdateFailure(fmt.Errorf("restart failed"))
	}
	return nil
}

func validateAgentUpdatePayload(update agentUpdatePayload) error {
	if strings.TrimSpace(update.Version) == "" {
		return fmt.Errorf("update version missing")
	}
	if compareAgentVersions(update.Version, agentVersion()) <= 0 {
		return fmt.Errorf("refusing downgrade from %s to %s", agentVersion(), update.Version)
	}
	parsedURL, err := url.Parse(strings.TrimSpace(update.URL))
	if err != nil || parsedURL.Scheme != "https" || parsedURL.Host == "" {
		return fmt.Errorf("update URL must be HTTPS")
	}
	if !isSHA256Hex(update.SHA256) {
		return fmt.Errorf("update checksum missing or invalid")
	}
	return nil
}

func downloadAgentUpdate(downloadURL string) (string, error) {
	binaryPath, err := agentBinaryPath()
	if err != nil {
		return "", err
	}
	tempFile, err := os.CreateTemp(filepath.Dir(binaryPath), ".nethera-agent-update-*")
	if err != nil {
		return "", err
	}
	tempPath := tempFile.Name()
	resp, err := http.Get(downloadURL)
	if err != nil {
		_ = tempFile.Close()
		_ = os.Remove(tempPath)
		return "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		_ = tempFile.Close()
		_ = os.Remove(tempPath)
		return "", fmt.Errorf("download failed with status %d", resp.StatusCode)
	}
	if _, err := io.Copy(tempFile, resp.Body); err != nil {
		_ = tempFile.Close()
		_ = os.Remove(tempPath)
		return "", err
	}
	if err := tempFile.Close(); err != nil {
		_ = os.Remove(tempPath)
		return "", err
	}
	if err := os.Chmod(tempPath, 0o755); err != nil {
		_ = os.Remove(tempPath)
		return "", err
	}
	return tempPath, nil
}

func verifyAgentUpdateChecksum(path string, expected string) error {
	file, err := os.Open(path)
	if err != nil {
		return err
	}
	defer file.Close()
	hash := sha256.New()
	if _, err := io.Copy(hash, file); err != nil {
		return err
	}
	actual := hex.EncodeToString(hash.Sum(nil))
	if !strings.EqualFold(actual, strings.TrimSpace(expected)) {
		return fmt.Errorf("checksum mismatch")
	}
	return nil
}

func installAgentUpdate(tempPath string) error {
	binaryPath, err := agentBinaryPath()
	if err != nil {
		return err
	}
	previousPath := binaryPath + ".previous"
	if _, err := os.Stat(binaryPath); err == nil {
		_ = os.Remove(previousPath)
		if err := os.Rename(binaryPath, previousPath); err != nil {
			return err
		}
	}
	if err := os.Rename(tempPath, binaryPath); err != nil {
		if _, restoreErr := os.Stat(previousPath); restoreErr == nil {
			_ = os.Rename(previousPath, binaryPath)
		}
		return err
	}
	return os.Chmod(binaryPath, 0o755)
}

func restartAgentService() error {
	if override := strings.TrimSpace(os.Getenv("NETHERA_AGENT_RESTART_COMMAND")); override != "" {
		cmd := exec.Command("sh", "-c", override)
		cmd.Env = os.Environ()
		return cmd.Start()
	}
	command := "sleep 1; systemctl restart nethera-agent"
	cmd := exec.Command("sh", "-c", command)
	cmd.Env = os.Environ()
	if err := cmd.Start(); err != nil {
		return err
	}
	go func() {
		time.Sleep(250 * time.Millisecond)
		os.Exit(0)
	}()
	return nil
}

func agentBinaryPath() (string, error) {
	if value := strings.TrimSpace(os.Getenv("NETHERA_AGENT_BINARY_PATH")); value != "" {
		return value, nil
	}
	path, err := os.Executable()
	if err != nil {
		return "", err
	}
	resolved, err := filepath.EvalSymlinks(path)
	if err == nil {
		return resolved, nil
	}
	return path, nil
}

func loadAgentUpdateState() (agentUpdateState, error) {
	var state agentUpdateState
	data, err := os.ReadFile(agentUpdateStatePath())
	if err != nil {
		if os.IsNotExist(err) {
			return state, nil
		}
		return state, err
	}
	if err := json.Unmarshal(data, &state); err != nil {
		return agentUpdateState{}, err
	}
	return state, nil
}

func saveAgentUpdateState(state agentUpdateState) error {
	path := agentUpdateStatePath()
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	data, err := json.MarshalIndent(state, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(path, append(data, '\n'), 0o600)
}

func recordAgentUpdateAttempt() error {
	state, _ := loadAgentUpdateState()
	state.LastAttemptAt = time.Now().UTC().Format(time.RFC3339)
	state.LastError = ""
	return saveAgentUpdateState(state)
}

func recordAgentUpdateFailure(err error) error {
	state, _ := loadAgentUpdateState()
	state.LastAttemptAt = time.Now().UTC().Format(time.RFC3339)
	state.LastError = shortUpdateError(err)
	state.ConsecutiveFailures += 1
	state.NextAttemptAfter = time.Now().UTC().Add(updateRetryDelay(state.ConsecutiveFailures)).Format(time.RFC3339)
	_ = saveAgentUpdateState(state)
	return err
}

func clearAgentUpdateState() error {
	return saveAgentUpdateState(agentUpdateState{})
}

func agentUpdateStatePath() string {
	return filepath.Join(netheraStateDir(), agentUpdateStateFile)
}

func updateRetryDelay(failures int) time.Duration {
	switch {
	case failures <= 1:
		return time.Minute
	case failures == 2:
		return 5 * time.Minute
	default:
		return 15 * time.Minute
	}
}

func shortUpdateError(err error) string {
	if err == nil {
		return ""
	}
	message := strings.TrimSpace(err.Error())
	if len(message) > 120 {
		return message[:120]
	}
	return message
}

func isSHA256Hex(value string) bool {
	value = strings.TrimSpace(value)
	if len(value) != 64 {
		return false
	}
	for _, ch := range value {
		if (ch >= '0' && ch <= '9') || (ch >= 'a' && ch <= 'f') || (ch >= 'A' && ch <= 'F') {
			continue
		}
		return false
	}
	return true
}

func compareAgentVersions(left string, right string) int {
	leftParts := parseAgentVersion(left)
	rightParts := parseAgentVersion(right)
	for index := 0; index < 3; index += 1 {
		if leftParts[index] < rightParts[index] {
			return -1
		}
		if leftParts[index] > rightParts[index] {
			return 1
		}
	}
	return 0
}

func parseAgentVersion(value string) [3]int {
	core := strings.TrimPrefix(strings.TrimSpace(value), "v")
	if before, _, ok := strings.Cut(core, "-"); ok {
		core = before
	}
	if before, _, ok := strings.Cut(core, "+"); ok {
		core = before
	}
	parts := strings.Split(core, ".")
	parsed := [3]int{}
	for index := 0; index < len(parts) && index < 3; index += 1 {
		number, err := strconv.Atoi(parts[index])
		if err != nil || number < 0 {
			continue
		}
		parsed[index] = number
	}
	return parsed
}
