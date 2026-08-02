package main

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"unicode/utf8"

	"gopkg.in/yaml.v3"
)

const (
	managedFilesMaxPerService = 10
	managedFilesMaxFileBytes  = 64 * 1024
	managedFilesMaxTotalBytes = 256 * 1024
)

var managedFileModePattern = regexp.MustCompile(`^0[0-7]{3}$`)

func collectManagedFiles(composeContent, manifestPath string) ([]managedFileSnapshot, error) {
	var root yaml.Node
	if err := yaml.Unmarshal([]byte(composeContent), &root); err != nil {
		return nil, fmt.Errorf("compose yaml is invalid: %w", err)
	}
	if len(root.Content) == 0 || root.Content[0].Kind != yaml.MappingNode {
		return nil, fmt.Errorf("compose yaml must be a mapping at the top level")
	}
	projectDir, err := filepath.Abs(filepath.Dir(manifestPath))
	if err != nil {
		return nil, err
	}
	projectDirReal, err := filepath.EvalSymlinks(projectDir)
	if err != nil {
		return nil, err
	}
	servicesNode := mappingValue(root.Content[0], "services")
	if servicesNode == nil {
		return nil, nil
	}
	if servicesNode.Kind != yaml.MappingNode {
		return nil, fmt.Errorf("services must be a mapping")
	}

	snapshots := []managedFileSnapshot{}
	totalBytes := 0
	for index := 0; index+1 < len(servicesNode.Content); index += 2 {
		serviceName := strings.TrimSpace(servicesNode.Content[index].Value)
		serviceNode := servicesNode.Content[index+1]
		if serviceName == "" || serviceNode.Kind != yaml.MappingNode {
			continue
		}
		netheraNode := mappingValue(serviceNode, "nethera")
		if netheraNode == nil {
			continue
		}
		if netheraNode.Kind != yaml.MappingNode {
			return nil, fmt.Errorf("service %s nethera must be a mapping", serviceName)
		}
		filesNode := mappingValue(netheraNode, "files")
		if filesNode == nil {
			continue
		}
		serviceFiles, serviceBytes, err := collectServiceManagedFiles(serviceName, filesNode, projectDir, projectDirReal)
		if err != nil {
			return nil, err
		}
		totalBytes += serviceBytes
		if totalBytes > managedFilesMaxTotalBytes {
			return nil, fmt.Errorf("managed files are too large. Maximum total size is 256 KB")
		}
		snapshots = append(snapshots, serviceFiles...)
	}
	return snapshots, nil
}

func collectServiceManagedFiles(serviceName string, filesNode *yaml.Node, projectDir, projectDirReal string) ([]managedFileSnapshot, int, error) {
	if filesNode.Kind != yaml.MappingNode {
		return nil, 0, fmt.Errorf("service %s nethera.files must be a mapping", serviceName)
	}
	if len(filesNode.Content)/2 > managedFilesMaxPerService {
		return nil, 0, fmt.Errorf("service %s has too many managed files. Maximum is %d per service", serviceName, managedFilesMaxPerService)
	}
	targets := map[string]bool{}
	snapshots := []managedFileSnapshot{}
	totalBytes := 0
	for index := 0; index+1 < len(filesNode.Content); index += 2 {
		name := strings.TrimSpace(filesNode.Content[index].Value)
		if err := validateManagedFileName(name); err != nil {
			return nil, 0, err
		}
		configNode := filesNode.Content[index+1]
		if configNode.Kind != yaml.MappingNode {
			return nil, 0, fmt.Errorf("managed file %s for service %s must be a mapping", name, serviceName)
		}
		source := strings.TrimSpace(yamlScalarValue(mappingValue(configNode, "source")))
		target := strings.TrimSpace(yamlScalarValue(mappingValue(configNode, "target")))
		mode := strings.TrimSpace(yamlScalarValue(mappingValue(configNode, "mode")))
		if mode == "" {
			mode = "0644"
		}
		if err := validateManagedFileSource(source); err != nil {
			return nil, 0, err
		}
		if err := validateManagedFileTarget(target); err != nil {
			return nil, 0, err
		}
		if err := validateManagedFileMode(mode); err != nil {
			return nil, 0, err
		}
		if targets[target] {
			return nil, 0, fmt.Errorf("managed file target %s is declared more than once for service %s", target, serviceName)
		}
		targets[target] = true

		content, size, err := readManagedFile(source, projectDir, projectDirReal)
		if err != nil {
			return nil, 0, err
		}
		totalBytes += size
		snapshots = append(snapshots, managedFileSnapshot{
			ServiceName: serviceName,
			Name:        name,
			Source:      filepath.ToSlash(source),
			Target:      target,
			Mode:        mode,
			Content:     content,
		})
	}
	return snapshots, totalBytes, nil
}

func validateManagedFileName(name string) error {
	if name == "" {
		return fmt.Errorf("managed file name is required")
	}
	if len(name) > 128 {
		return fmt.Errorf("managed file name %q is too long. Maximum length is 128 characters", name)
	}
	if strings.ContainsAny(name, `/\`) || strings.ContainsRune(name, 0) {
		return fmt.Errorf("managed file name %q must not contain path separators", name)
	}
	return nil
}

func validateManagedFileSource(source string) error {
	if source == "" {
		return fmt.Errorf("managed file source is required")
	}
	if filepath.IsAbs(source) {
		return fmt.Errorf("managed file source %s must be relative to nethera.yml", source)
	}
	if strings.ContainsRune(source, 0) {
		return fmt.Errorf("managed file source %s is invalid", source)
	}
	for _, segment := range strings.FieldsFunc(filepath.ToSlash(source), func(r rune) bool { return r == '/' }) {
		if segment == ".." {
			return fmt.Errorf("managed file source %s must not contain .. path segments", source)
		}
	}
	return nil
}

func validateManagedFileTarget(target string) error {
	if target == "" || !strings.HasPrefix(target, "/") || target == "/" {
		return fmt.Errorf("managed file target must be an absolute container path")
	}
	if strings.ContainsRune(target, 0) {
		return fmt.Errorf("managed file target %s is invalid", target)
	}
	for _, segment := range strings.Split(target, "/") {
		if segment == ".." || segment == "." {
			return fmt.Errorf("managed file target %s contains an invalid path segment", target)
		}
	}
	return nil
}

func validateManagedFileMode(mode string) error {
	if !managedFileModePattern.MatchString(mode) {
		return fmt.Errorf("managed file mode must be a four-digit octal string such as \"0644\"")
	}
	return nil
}

func readManagedFile(source, projectDir, projectDirReal string) (string, int, error) {
	candidate := filepath.Clean(filepath.Join(projectDir, source))
	realPath, err := filepath.EvalSymlinks(candidate)
	if err != nil {
		if os.IsNotExist(err) {
			return "", 0, fmt.Errorf("managed file %s does not exist", source)
		}
		return "", 0, err
	}
	if !pathInside(realPath, projectDirReal) {
		return "", 0, fmt.Errorf("managed file %s resolves outside the app directory", source)
	}
	info, err := os.Stat(realPath)
	if err != nil {
		return "", 0, err
	}
	if info.IsDir() {
		return "", 0, fmt.Errorf("managed file %s is a directory; managed files must be regular UTF-8 text files", source)
	}
	if !info.Mode().IsRegular() {
		return "", 0, fmt.Errorf("managed file %s is not a regular file", source)
	}
	if info.Size() > managedFilesMaxFileBytes {
		return "", 0, fmt.Errorf("managed file %s is too large. Maximum supported size is 64 KB", source)
	}
	data, err := os.ReadFile(realPath)
	if err != nil {
		return "", 0, err
	}
	if !utf8.Valid(data) {
		return "", 0, fmt.Errorf("managed file %s is not a UTF-8 text file. Managed files only support small text files", source)
	}
	return string(data), len(data), nil
}

func pathInside(path, root string) bool {
	rel, err := filepath.Rel(root, path)
	if err != nil {
		return false
	}
	return rel == "." || (!strings.HasPrefix(rel, ".."+string(filepath.Separator)) && rel != "..")
}

func yamlScalarValue(node *yaml.Node) string {
	if node == nil || node.Kind != yaml.ScalarNode {
		return ""
	}
	return node.Value
}
