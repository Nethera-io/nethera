package main

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"gopkg.in/yaml.v3"
)

func placeholderHelloWorldCompose() string {
	return strings.TrimRight(`services:
  web:
		image: nginxdemos/hello
		nethera:
			public: 80
    ports:
			- "80"
`, "\n") + "\n"
}

func findNetheraManifestUpward(start string) (string, error) {
	dir, err := filepath.Abs(start)
	if err != nil {
		return "", err
	}
	for {
		candidate := filepath.Join(dir, "nethera.yml")
		if _, statErr := os.Stat(candidate); statErr == nil {
			return candidate, nil
		} else if !os.IsNotExist(statErr) {
			return "", statErr
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return "", os.ErrNotExist
		}
		dir = parent
	}
}

func resolveNetheraManifestPath(inputPath string) (string, error) {
	absPath, err := filepath.Abs(strings.TrimSpace(inputPath))
	if err != nil {
		return "", err
	}

	info, err := os.Stat(absPath)
	if err != nil {
		return "", err
	}
	if info.IsDir() {
		manifestPath := filepath.Join(absPath, "nethera.yml")
		if _, err := os.Stat(manifestPath); err != nil {
			if os.IsNotExist(err) {
				return "", fmt.Errorf("nethera.yml not found. Run neth init")
			}
			return "", err
		}
		return manifestPath, nil
	}

	if filepath.Base(absPath) != "nethera.yml" {
		return "", fmt.Errorf("expected nethera.yml, got %s", absPath)
	}
	return absPath, nil
}

func defaultAppNameFromDir(dir string) string {
	base := strings.TrimSpace(filepath.Base(dir))
	if base == "" || base == "." || base == string(filepath.Separator) {
		return "default"
	}
	lower := strings.ToLower(base)
	var builder strings.Builder
	for _, ch := range lower {
		if (ch >= 'a' && ch <= 'z') || (ch >= '0' && ch <= '9') || ch == '-' || ch == '_' {
			builder.WriteRune(ch)
			continue
		}
		builder.WriteRune('-')
	}
	result := strings.Trim(builder.String(), "-")
	if result == "" {
		return "default"
	}
	return result
}

func annotateComposeMetadata(composeContent, appName, action string, destroyVolumes bool) string {
	trimmedAppName := strings.TrimSpace(appName)
	if trimmedAppName == "" {
		trimmedAppName = "default"
	}
	trimmedAction := strings.TrimSpace(strings.ToLower(action))
	if trimmedAction == "" {
		trimmedAction = "deploy"
	}
	trimmedCompose := strings.TrimLeft(composeContent, "\n")
	lines := strings.Split(trimmedCompose, "\n")
	metadata := []string{
		fmt.Sprintf("# nethera-app-name: %s", trimmedAppName),
		fmt.Sprintf("# nethera-action: %s", trimmedAction),
	}
	if trimmedAction == "destroy" && destroyVolumes {
		metadata = append(metadata, "# nethera-destroy-volumes: true")
	}
	for len(lines) > 0 {
		trimmedLine := strings.ToLower(strings.TrimSpace(lines[0]))
		if strings.HasPrefix(trimmedLine, "# nethera-app-name:") ||
			strings.HasPrefix(trimmedLine, "# nethera-action:") ||
			strings.HasPrefix(trimmedLine, "# nethera-destroy-volumes:") {
			lines = lines[1:]
			continue
		}
		break
	}
	return strings.Join(append(metadata, lines...), "\n")
}

func normalizeComposeYAML(composeContent string) (string, error) {
	var root yaml.Node
	if err := yaml.Unmarshal([]byte(composeContent), &root); err != nil {
		return "", fmt.Errorf("compose yaml is invalid: %w", err)
	}
	if len(root.Content) == 0 {
		return "", fmt.Errorf("compose yaml is empty")
	}
	if err := updateComposeNode(root.Content[0]); err != nil {
		return "", err
	}
	var builder strings.Builder
	encoder := yaml.NewEncoder(&builder)
	encoder.SetIndent(2)
	if err := encoder.Encode(&root); err != nil {
		_ = encoder.Close()
		return "", err
	}
	if err := encoder.Close(); err != nil {
		return "", err
	}
	return strings.TrimRight(builder.String(), "\n") + "\n", nil
}

func updateComposeNode(node *yaml.Node) error {
	if node.Kind != yaml.MappingNode {
		return fmt.Errorf("compose yaml must be a mapping at the top level")
	}

	servicesNode := mappingValue(node, "services")
	if servicesNode == nil {
		return nil
	}
	if servicesNode.Kind != yaml.MappingNode {
		return fmt.Errorf("services must be a mapping")
	}

	for index := 0; index < len(servicesNode.Content); index += 2 {
		serviceValue := servicesNode.Content[index+1]
		if serviceValue.Kind != yaml.MappingNode {
			return fmt.Errorf("service %q must be a mapping", servicesNode.Content[index].Value)
		}
		if err := normalizeServiceNode(serviceValue); err != nil {
			return err
		}
	}
	return nil
}

func normalizeServiceNode(serviceNode *yaml.Node) error {
	portsNode := mappingValue(serviceNode, "ports")
	if portsNode == nil {
		return nil
	}
	if portsNode.Kind != yaml.SequenceNode {
		return fmt.Errorf("ports must be a list")
	}

	netheraNode := mappingValue(serviceNode, "nethera")
	publicNode := (*yaml.Node)(nil)
	if netheraNode != nil && netheraNode.Kind == yaml.MappingNode {
		publicNode = mappingValue(netheraNode, "public")
	}
	if publicNode != nil && strings.EqualFold(strings.TrimSpace(publicNode.Value), "true") {
		return fmt.Errorf("nethera.public: true is not supported. Use nethera.public: <container port>, for example public: 80")
	}
	for _, portNode := range portsNode.Content {
		if portNode.Kind != yaml.ScalarNode {
			return fmt.Errorf("ports entries must be strings")
		}
	}
	return nil
}

func normalizePortMapping(portValue string) (string, bool) {
	trimmed := strings.TrimSpace(portValue)
	if trimmed == "" {
		return trimmed, false
	}
	protocol := ""
	if index := strings.LastIndex(trimmed, "/"); index != -1 {
		protocol = trimmed[index:]
		trimmed = trimmed[:index]
	}
	parts := strings.Split(trimmed, ":")
	if len(parts) < 2 {
		return portValue, false
	}
	return parts[len(parts)-1] + protocol, true
}

func mappingValue(node *yaml.Node, key string) *yaml.Node {
	if node.Kind != yaml.MappingNode {
		return nil
	}
	for index := 0; index < len(node.Content); index += 2 {
		if node.Content[index].Value == key {
			return node.Content[index+1]
		}
	}
	return nil
}

func removeMappingKey(node *yaml.Node, key string) {
	if node.Kind != yaml.MappingNode {
		return
	}
	filtered := make([]*yaml.Node, 0, len(node.Content))
	for index := 0; index < len(node.Content); index += 2 {
		if node.Content[index].Value == key {
			continue
		}
		filtered = append(filtered, node.Content[index], node.Content[index+1])
	}
	node.Content = filtered
}

func setMappingValue(node *yaml.Node, key, value string) {
	if node.Kind != yaml.MappingNode {
		return
	}
	for index := 0; index < len(node.Content); index += 2 {
		if node.Content[index].Value == key {
			node.Content[index+1] = &yaml.Node{Kind: yaml.ScalarNode, Tag: "!!bool", Value: value}
			return
		}
	}
	node.Content = append(node.Content,
		&yaml.Node{Kind: yaml.ScalarNode, Tag: "!!str", Value: key},
		&yaml.Node{Kind: yaml.ScalarNode, Tag: "!!bool", Value: value},
	)
}

func setNestedMappingValue(node *yaml.Node, parentKey, key, value string) {
	parent := mappingValue(node, parentKey)
	if parent == nil || parent.Kind != yaml.MappingNode {
		parent = &yaml.Node{Kind: yaml.MappingNode, Tag: "!!map"}
		node.Content = append(node.Content,
			&yaml.Node{Kind: yaml.ScalarNode, Tag: "!!str", Value: parentKey},
			parent,
		)
	}
	setMappingValue(parent, key, value)
}

func loadManifest(path string) (netheraManifest, error) {
	var manifest netheraManifest
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return netheraManifest{}, nil
		}
		return netheraManifest{}, err
	}
	lines := strings.Split(string(data), "\n")
	inTargets := false
	inCompose := false
	for _, line := range lines {
		trimmed := strings.TrimSpace(line)
		if trimmed == "" {
			if inCompose {
				manifest.Compose += "\n"
			}
			continue
		}
		if strings.HasPrefix(trimmed, "appId:") {
			manifest.AppID = strings.TrimSpace(strings.TrimPrefix(trimmed, "appId:"))
			continue
		}
		if strings.HasPrefix(trimmed, "appName:") {
			manifest.AppName = strings.TrimSpace(strings.TrimPrefix(trimmed, "appName:"))
			continue
		}
		if trimmed == "targets:" {
			inTargets = true
			inCompose = false
			continue
		}
		if inTargets && strings.HasPrefix(trimmed, "-") {
			manifest.Targets = append(manifest.Targets, strings.TrimSpace(strings.TrimPrefix(trimmed, "-")))
			continue
		}
		if inTargets {
			inTargets = false
			inCompose = true
		}
		if !inCompose {
			inCompose = true
		}
		if inCompose {
			manifest.Compose += line + "\n"
		}
	}
	manifest.Compose = strings.TrimRight(manifest.Compose, "\n") + "\n"
	return manifest, nil
}

func saveManifest(path string, manifest netheraManifest) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	var builder strings.Builder
	if strings.TrimSpace(manifest.AppName) != "" {
		builder.WriteString(fmt.Sprintf("appName: %s\n", manifest.AppName))
	}
	if strings.TrimSpace(manifest.AppID) != "" {
		builder.WriteString(fmt.Sprintf("appId: %s\n", manifest.AppID))
	}
	builder.WriteString("targets:\n")
	for _, target := range manifest.Targets {
		builder.WriteString(fmt.Sprintf(" - %s\n", target))
	}
	if strings.TrimSpace(manifest.Compose) != "" {
		builder.WriteString("\n")
		builder.WriteString(strings.TrimRight(manifest.Compose, "\n"))
		builder.WriteString("\n")
	}
	return os.WriteFile(path, []byte(builder.String()), 0o644)
}
