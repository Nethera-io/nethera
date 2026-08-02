package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"sort"
	"strings"

	"gopkg.in/yaml.v3"
)

func fetchDeploymentSecrets(backendURL, token, deploymentID string) (deploymentSecretBundle, error) {
	return fetchDeploymentSecretsWithContext(context.Background(), backendURL, token, deploymentID)
}

func fetchDeploymentSecretsWithContext(ctx context.Context, backendURL, token, deploymentID string) (deploymentSecretBundle, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, strings.TrimRight(backendURL, "/")+"/api/agent/deployments/"+url.PathEscape(deploymentID)+"/secrets", nil)
	if err != nil {
		return deploymentSecretBundle{}, err
	}
	req.Header.Set("Authorization", "Bearer "+token)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return deploymentSecretBundle{}, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		return deploymentSecretBundle{}, &httpStatusError{Endpoint: "api/agent/deployments/:deploymentId/secrets", Status: resp.StatusCode, Details: summarizeBody(body)}
	}
	var result deploymentSecretsResponse
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return deploymentSecretBundle{}, err
	}
	if result.RuntimeSecrets == nil {
		result.RuntimeSecrets = result.Secrets
	}
	if result.RuntimeSecrets == nil {
		result.RuntimeSecrets = map[string]string{}
	}
	if result.GeneratedEnv == nil {
		result.GeneratedEnv = map[string]string{}
	}
	for name, value := range result.RuntimeSecrets {
		if strings.ContainsAny(value, "\r\n") {
			return deploymentSecretBundle{}, fmt.Errorf("secret %s contains a multiline value; multiline secret values are not supported yet", name)
		}
	}
	for name, value := range result.GeneratedEnv {
		if !validSecretName(name) {
			return deploymentSecretBundle{}, fmt.Errorf("generated env var %s is not a valid environment variable name", name)
		}
		if strings.ContainsAny(value, "\r\n") {
			return deploymentSecretBundle{}, fmt.Errorf("generated env var %s contains a multiline value", name)
		}
		if _, exists := result.RuntimeSecrets[name]; exists {
			return deploymentSecretBundle{}, fmt.Errorf("generated env var %s conflicts with a runtime secret", name)
		}
	}
	for _, credential := range result.ImagePullCredentials {
		if strings.TrimSpace(credential.Registry) == "" || credential.Username == "" || credential.Password == "" {
			return deploymentSecretBundle{}, fmt.Errorf("image pull credential response is incomplete")
		}
	}
	return deploymentSecretBundle{RuntimeSecrets: result.RuntimeSecrets, GeneratedEnv: result.GeneratedEnv, ImagePullCredentials: uniqueImagePullCredentials(result.ImagePullCredentials)}, nil
}

func uniqueImagePullCredentials(credentials []imagePullCredentialSecret) []imagePullCredentialSecret {
	seen := map[string]bool{}
	unique := []imagePullCredentialSecret{}
	for _, credential := range credentials {
		key := credential.Registry + "\n" + credential.Username + "\n" + credential.Password
		if seen[key] {
			continue
		}
		seen[key] = true
		unique = append(unique, credential)
	}
	return unique
}

func ensureDockerConfigDir(path string) error {
	if err := os.MkdirAll(path, 0o700); err != nil {
		return err
	}
	return os.Chmod(path, 0o700)
}

func directoryExists(path string) bool {
	info, err := os.Stat(path)
	return err == nil && info.IsDir()
}

func dockerLoginForImagePullCredentials(dockerBin, dockerConfigDir string, credentials []imagePullCredentialSecret) ([]string, error) {
	return dockerLoginForImagePullCredentialsWithContext(context.Background(), dockerBin, dockerConfigDir, credentials)
}

func dockerLoginForImagePullCredentialsWithContext(ctx context.Context, dockerBin, dockerConfigDir string, credentials []imagePullCredentialSecret) ([]string, error) {
	if len(credentials) == 0 {
		return nil, nil
	}
	logs := []string{}
	for _, credential := range uniqueImagePullCredentials(credentials) {
		registry := strings.TrimSpace(credential.Registry)
		logs = append(logs, fmt.Sprintf("Authenticating to registry %s", registry))
		cmd := exec.CommandContext(ctx, dockerBin, "login", registry, "-u", credential.Username, "--password-stdin")
		cmd.Env = append(os.Environ(), "DOCKER_CONFIG="+dockerConfigDir)
		cmd.Stdin = strings.NewReader(credential.Password)
		output, err := runCommandStreaming(cmd, nil)
		if err != nil {
			logs = append(logs, fmt.Sprintf("Registry authentication failed for %s", registry))
			if exitErr, ok := err.(*exec.ExitError); ok {
				logs = append(logs, fmt.Sprintf("docker login exit code: %d", exitErr.ExitCode()))
			}
			_ = output
			return logs, fmt.Errorf("Failed to authenticate to registry %s. Check image pull credentials for this app.", registry)
		}
		logs = append(logs, fmt.Sprintf("Registry authentication succeeded for %s", registry))
	}
	return logs, nil
}

func extractRequiredSecretNames(composeContent string) ([]string, error) {
	var root yaml.Node
	if err := yaml.Unmarshal([]byte(composeContent), &root); err != nil {
		return nil, fmt.Errorf("compose yaml is invalid: %w", err)
	}
	if len(root.Content) == 0 || root.Content[0].Kind != yaml.MappingNode {
		return nil, fmt.Errorf("compose yaml must be a mapping at the top level")
	}
	if err := validateNoLocalFileReferences(root.Content[0]); err != nil {
		return nil, err
	}
	servicesNode := yamlMappingValue(root.Content[0], "services")
	if servicesNode == nil {
		return nil, nil
	}
	if servicesNode.Kind != yaml.MappingNode {
		return nil, fmt.Errorf("services must be a mapping")
	}
	seen := map[string]bool{}
	for index := 0; index+1 < len(servicesNode.Content); index += 2 {
		serviceName := strings.TrimSpace(servicesNode.Content[index].Value)
		serviceNode := servicesNode.Content[index+1]
		if serviceName == "" || serviceNode.Kind != yaml.MappingNode {
			continue
		}
		netheraNode := yamlMappingValue(serviceNode, "nethera")
		if netheraNode == nil {
			continue
		}
		if netheraNode.Kind != yaml.MappingNode {
			return nil, fmt.Errorf("service %s nethera must be a mapping", serviceName)
		}
		secretsNode := yamlMappingValue(netheraNode, "secrets")
		if secretsNode == nil {
			continue
		}
		if secretsNode.Kind != yaml.SequenceNode {
			return nil, fmt.Errorf("service %s nethera.secrets must be a list", serviceName)
		}
		for _, item := range secretsNode.Content {
			name := strings.TrimSpace(item.Value)
			if !validSecretName(name) {
				return nil, fmt.Errorf("service %s has invalid secret name %q", serviceName, name)
			}
			seen[name] = true
		}
	}
	names := make([]string, 0, len(seen))
	for name := range seen {
		names = append(names, name)
	}
	sort.Strings(names)
	return names, nil
}

func validateNoLocalFileReferences(root *yaml.Node) error {
	if err := validateTopLevelFileReferences(root, "configs"); err != nil {
		return err
	}
	if err := validateTopLevelFileReferences(root, "secrets"); err != nil {
		return err
	}
	servicesNode := yamlMappingValue(root, "services")
	if servicesNode == nil {
		return nil
	}
	if servicesNode.Kind != yaml.MappingNode {
		return fmt.Errorf("services must be a mapping")
	}
	for index := 0; index+1 < len(servicesNode.Content); index += 2 {
		serviceName := strings.TrimSpace(servicesNode.Content[index].Value)
		serviceNode := servicesNode.Content[index+1]
		if serviceName == "" || serviceNode.Kind != yaml.MappingNode {
			continue
		}
		if err := validateServiceNoLocalFileReferences(serviceName, serviceNode); err != nil {
			return err
		}
	}
	return nil
}

func validateTopLevelFileReferences(root *yaml.Node, key string) error {
	node := yamlMappingValue(root, key)
	if node == nil || node.Kind != yaml.MappingNode {
		return nil
	}
	for index := 0; index+1 < len(node.Content); index += 2 {
		value := node.Content[index+1]
		if value.Kind != yaml.MappingNode {
			continue
		}
		fileNode := yamlMappingValue(value, "file")
		if fileNode != nil {
			return fmt.Errorf("Local file reference %q is not supported yet.\nUse an absolute path on the target machine, or build and push an image first.", strings.TrimSpace(fileNode.Value))
		}
	}
	return nil
}

func validateServiceNoLocalFileReferences(serviceName string, serviceNode *yaml.Node) error {
	if yamlMappingValue(serviceNode, "build") != nil {
		return fmt.Errorf("Local build contexts are not supported yet.\nBuild and push your image to a registry, then reference it with image:.")
	}
	if yamlMappingValue(serviceNode, "env_file") != nil {
		return fmt.Errorf("Local env_file is not supported.\nUse app-scoped secrets with `neth secrets set`.")
	}
	volumesNode := yamlMappingValue(serviceNode, "volumes")
	if volumesNode == nil {
		return nil
	}
	if volumesNode.Kind != yaml.SequenceNode {
		return fmt.Errorf("service %s volumes must be a list", serviceName)
	}
	for _, item := range volumesNode.Content {
		source := composeVolumeSource(item)
		if source != "" && isLocalFileReference(source) {
			return fmt.Errorf("Local file reference %q is not supported yet.\nUse an absolute path on the target machine, or build and push an image first.", source)
		}
	}
	return nil
}

func composeVolumeSource(node *yaml.Node) string {
	if node.Kind == yaml.MappingNode {
		typeNode := yamlMappingValue(node, "type")
		if typeNode != nil && strings.TrimSpace(typeNode.Value) == "volume" {
			return ""
		}
		sourceNode := yamlMappingValue(node, "source")
		if sourceNode == nil {
			sourceNode = yamlMappingValue(node, "src")
		}
		if sourceNode == nil {
			return ""
		}
		return strings.TrimSpace(sourceNode.Value)
	}
	raw := strings.TrimSpace(node.Value)
	parts := strings.Split(raw, ":")
	if len(parts) < 2 {
		return ""
	}
	return strings.TrimSpace(parts[0])
}

func isLocalFileReference(path string) bool {
	normalized := strings.TrimSpace(path)
	if normalized == "" || strings.HasPrefix(normalized, "/") {
		return false
	}
	if normalized == "." || normalized == ".." || strings.HasPrefix(normalized, "./") || strings.HasPrefix(normalized, "../") {
		return true
	}
	return strings.Contains(normalized, "/")
}

func validSecretName(name string) bool {
	if name == "" || len(name) > 128 {
		return false
	}
	for index, ch := range name {
		if index == 0 {
			if (ch < 'A' || ch > 'Z') && ch != '_' {
				return false
			}
			continue
		}
		if (ch < 'A' || ch > 'Z') && (ch < '0' || ch > '9') && ch != '_' {
			return false
		}
	}
	return true
}

func writeDeploymentEnvFile(path string, secrets map[string]string) error {
	if len(secrets) == 0 {
		return nil
	}
	var builder strings.Builder
	names := make([]string, 0, len(secrets))
	for name := range secrets {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		builder.WriteString(name)
		builder.WriteByte('=')
		builder.WriteString(formatEnvValue(secrets[name]))
		builder.WriteByte('\n')
	}
	tmpPath := path + ".tmp"
	if err := os.WriteFile(tmpPath, []byte(builder.String()), 0o600); err != nil {
		return err
	}
	if err := os.Chmod(tmpPath, 0o600); err != nil {
		_ = os.Remove(tmpPath)
		return err
	}
	if err := os.Rename(tmpPath, path); err != nil {
		_ = os.Remove(tmpPath)
		return err
	}
	return os.Chmod(path, 0o600)
}

func mergeDeploymentEnv(runtimeSecrets, generatedEnv map[string]string) (map[string]string, error) {
	merged := map[string]string{}
	for name, value := range generatedEnv {
		if !validSecretName(name) {
			return nil, fmt.Errorf("generated env var %s is not a valid environment variable name", name)
		}
		merged[name] = value
	}
	for name, value := range runtimeSecrets {
		if _, exists := merged[name]; exists {
			return nil, fmt.Errorf("runtime secret %s conflicts with a generated environment variable", name)
		}
		merged[name] = value
	}
	return merged, nil
}

func formatEnvValue(value string) string {
	if value == "" {
		return `""`
	}
	if !strings.ContainsAny(value, " \t\"'=#") {
		return value
	}
	escaped := strings.ReplaceAll(value, `\`, `\\`)
	escaped = strings.ReplaceAll(escaped, `"`, `\"`)
	return `"` + escaped + `"`
}
