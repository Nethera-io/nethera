package main

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
)

func composeProjectName(deploymentID string) string {
	deploymentPart := sanitizeProjectSegment(deploymentID)
	if deploymentPart == "" {
		deploymentPart = "unknown"
	}
	return "nethera_" + deploymentPart
}

func sanitizeProjectSegment(value string) string {
	base := strings.ToLower(strings.TrimSpace(value))
	if base == "" {
		return ""
	}
	var builder strings.Builder
	for _, ch := range base {
		if (ch >= 'a' && ch <= 'z') || (ch >= '0' && ch <= '9') || ch == '-' || ch == '_' {
			builder.WriteRune(ch)
			continue
		}
		builder.WriteRune('-')
	}
	return strings.Trim(builder.String(), "-")
}

func netheraStateDir() string {
	if value := strings.TrimSpace(os.Getenv("NETHERA_AGENT_STATE_DIR")); value != "" {
		return value
	}
	if value := strings.TrimSpace(os.Getenv("NETHERA_STATE_DIR")); value != "" {
		return value
	}
	return filepath.Join(string(os.PathSeparator), "var", "lib", "nethera")
}

func deploymentsStateDir() string {
	return filepath.Join(netheraStateDir(), "deployments")
}

func ensureDeploymentDir(deploymentID string) (string, error) {
	root := deploymentsStateDir()
	if err := ensureWritableDir(root); err != nil {
		return "", err
	}
	dir := filepath.Join(root, sanitizeProjectSegment(deploymentID))
	if err := ensureWritableDir(dir); err != nil {
		return "", err
	}
	if err := os.Chmod(dir, 0o700); err != nil {
		return "", err
	}
	return dir, nil
}

func ensureWritableDir(path string) error {
	if err := os.MkdirAll(path, 0o755); err == nil {
		return nil
	} else if !os.IsPermission(err) || os.Geteuid() == 0 {
		return err
	}
	sudoArgs := []string{"install", "-d", "-m", "0755", "-o", strconv.Itoa(os.Getuid()), "-g", strconv.Itoa(os.Getgid()), path}
	cmd := exec.Command("sudo", sudoArgs...)
	cmd.Env = os.Environ()
	output, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("create state directory %s: %w: %s", path, err, summarizeBody(output))
	}
	return nil
}

func metadataPathForDeployment(deploymentDir string) string {
	return filepath.Join(deploymentDir, "deployment.json")
}

func loadDeploymentMetadata(path string) (deploymentMetadata, error) {
	var metadata deploymentMetadata
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return metadata, nil
		}
		return metadata, err
	}
	if err := json.Unmarshal(data, &metadata); err != nil {
		return metadata, err
	}
	if metadata.AllocatedPorts == nil {
		metadata.AllocatedPorts = map[string]int{}
	}
	return metadata, nil
}

func saveDeploymentMetadata(path string, metadata deploymentMetadata) error {
	if metadata.AllocatedPorts == nil {
		metadata.AllocatedPorts = map[string]int{}
	}
	data, err := json.MarshalIndent(metadata, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(path, append(data, '\n'), 0o644)
}

func sha256Hex(content string) string {
	sum := sha256.Sum256([]byte(content))
	return "sha256:" + hex.EncodeToString(sum[:])
}

func wireGuardIPFromAddress(address string) string {
	trimmed := strings.TrimSpace(address)
	if before, _, ok := strings.Cut(trimmed, "/"); ok {
		return before
	}
	return trimmed
}
