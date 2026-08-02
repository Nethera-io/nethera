package main

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"unicode/utf8"
)

const (
	agentManagedFilesMaxPerService = 10
	agentManagedFilesMaxFileBytes  = 64 * 1024
	agentManagedFilesMaxTotalBytes = 256 * 1024
)

var agentManagedFileModePattern = regexp.MustCompile(`^0[0-7]{3}$`)

func materializeManagedFiles(deploymentDir string, files []managedFileSnapshot) ([]managedFileMount, error) {
	if len(files) == 0 {
		return nil, nil
	}
	filesRoot := filepath.Join(deploymentDir, "files")
	if err := os.MkdirAll(filesRoot, 0o700); err != nil {
		return nil, err
	}
	if err := os.Chmod(filesRoot, 0o700); err != nil {
		return nil, err
	}

	perService := map[string]int{}
	totalBytes := 0
	mounts := make([]managedFileMount, 0, len(files))
	for _, file := range files {
		if err := validateAgentManagedFile(file); err != nil {
			return nil, err
		}
		if strings.TrimSpace(file.Mode) == "" {
			file.Mode = "0644"
		}
		perService[file.ServiceName]++
		if perService[file.ServiceName] > agentManagedFilesMaxPerService {
			return nil, fmt.Errorf("service %s has too many managed files. Maximum is %d per service", file.ServiceName, agentManagedFilesMaxPerService)
		}
		contentBytes := []byte(file.Content)
		totalBytes += len(contentBytes)
		if len(contentBytes) > agentManagedFilesMaxFileBytes {
			return nil, fmt.Errorf("managed file %s is too large. Maximum supported size is 64 KB", file.Source)
		}
		if totalBytes > agentManagedFilesMaxTotalBytes {
			return nil, fmt.Errorf("managed files are too large. Maximum total size is 256 KB")
		}

		serviceDir := filepath.Join(filesRoot, sanitizeProjectSegment(file.ServiceName))
		if err := os.MkdirAll(serviceDir, 0o700); err != nil {
			return nil, err
		}
		if err := os.Chmod(serviceDir, 0o700); err != nil {
			return nil, err
		}
		finalPath := filepath.Join(serviceDir, file.Name)
		modeValue, _ := strconv.ParseUint(file.Mode, 8, 32)
		if err := writeManagedFileAtomic(finalPath, contentBytes, os.FileMode(modeValue)); err != nil {
			return nil, err
		}
		mounts = append(mounts, managedFileMount{
			ServiceName: file.ServiceName,
			Name:        file.Name,
			Target:      file.Target,
			HostPath:    finalPath,
		})
	}
	return mounts, nil
}

func validateAgentManagedFile(file managedFileSnapshot) error {
	if strings.TrimSpace(file.ServiceName) == "" {
		return fmt.Errorf("managed file serviceName is required")
	}
	if err := validateAgentManagedFileName(file.Name); err != nil {
		return err
	}
	if err := validateAgentManagedFileTarget(file.Target); err != nil {
		return err
	}
	if strings.TrimSpace(file.Mode) == "" {
		file.Mode = "0644"
	}
	if !agentManagedFileModePattern.MatchString(file.Mode) {
		return fmt.Errorf("managed file mode must be a four-digit octal string such as \"0644\"")
	}
	if !utf8.ValidString(file.Content) {
		return fmt.Errorf("managed file %s is not a UTF-8 text file. Managed files only support small text files", file.Source)
	}
	return nil
}

func validateAgentManagedFileName(name string) error {
	if strings.TrimSpace(name) == "" {
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

func validateAgentManagedFileTarget(target string) error {
	if target == "" || !strings.HasPrefix(target, "/") || target == "/" {
		return fmt.Errorf("managed file target must be an absolute container path")
	}
	if strings.ContainsRune(target, 0) {
		return fmt.Errorf("managed file target %s is invalid", target)
	}
	for _, segment := range strings.Split(target, "/") {
		if segment == "." || segment == ".." {
			return fmt.Errorf("managed file target %s contains an invalid path segment", target)
		}
	}
	return nil
}

func writeManagedFileAtomic(path string, content []byte, mode os.FileMode) error {
	tmp, err := os.CreateTemp(filepath.Dir(path), "."+filepath.Base(path)+".tmp-*")
	if err != nil {
		return err
	}
	tmpPath := tmp.Name()
	cleanup := true
	defer func() {
		if cleanup {
			_ = os.Remove(tmpPath)
		}
	}()
	if _, err := tmp.Write(content); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Chmod(mode); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := os.Rename(tmpPath, path); err != nil {
		return err
	}
	cleanup = false
	return nil
}
