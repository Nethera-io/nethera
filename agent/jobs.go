package main

import (
	"bytes"
	"context"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"syscall"
	"time"

	"gopkg.in/yaml.v3"
)

const (
	postDeployMaxCommands         = 10
	postDeployMaxCommandLength    = 1000
	postDeployExecReadyTimeout    = 60 * time.Second
	postDeployExecReadyPoll       = 2 * time.Second
	postDeployCommandTimeout      = 30 * time.Minute
	postDeployMaxOutputLines      = 80
	postDeployMaxOutputLineLength = 500
)

func mergeLANEndpointEnv(generatedEnv map[string]string, endpoints []publicEndpointReport) map[string]string {
	merged := map[string]string{}
	for name, value := range generatedEnv {
		merged[name] = value
	}

	lanEndpointCount := 0
	var singleLANService string
	for _, endpoint := range endpoints {
		if !endpoint.PreferLAN || strings.TrimSpace(endpoint.LANHost) == "" || endpoint.LANPort <= 0 || endpoint.LANPort > 65535 {
			continue
		}
		serviceName := strings.TrimSpace(endpoint.ServiceName)
		if serviceName == "" {
			continue
		}
		label := normalizedServiceEnvLabel(serviceName)
		lanHost := net.JoinHostPort(strings.TrimSpace(endpoint.LANHost), fmt.Sprintf("%d", endpoint.LANPort))
		lanURL := "http://" + lanHost
		merged["NETHERA_"+label+"_LAN_HOST"] = lanHost
		merged["NETHERA_"+label+"_LAN_URL"] = lanURL
		lanEndpointCount++
		singleLANService = label
	}

	if lanEndpointCount == 1 {
		merged["NETHERA_LAN_HOST"] = merged["NETHERA_"+singleLANService+"_LAN_HOST"]
		merged["NETHERA_LAN_URL"] = merged["NETHERA_"+singleLANService+"_LAN_URL"]
	}

	return merged
}

func normalizedServiceEnvLabel(serviceName string) string {
	var builder strings.Builder
	previousUnderscore := false
	for _, char := range strings.ToUpper(strings.TrimSpace(serviceName)) {
		if (char >= 'A' && char <= 'Z') || (char >= '0' && char <= '9') {
			builder.WriteRune(char)
			previousUnderscore = false
			continue
		}
		if !previousUnderscore && builder.Len() > 0 {
			builder.WriteByte('_')
			previousUnderscore = true
		}
	}
	normalized := strings.Trim(builder.String(), "_")
	if normalized == "" {
		normalized = "SERVICE"
	}
	if normalized[0] >= '0' && normalized[0] <= '9' {
		normalized = "_" + normalized
	}
	return normalized
}

type postDeployCommand struct {
	ServiceName string
	Command     string
}

type deployLogSink func(stream, line string)

type liveCommandCapture struct {
	mu      sync.Mutex
	output  bytes.Buffer
	pending map[string]string
	sink    deployLogSink
}

type liveCommandWriter struct {
	capture *liveCommandCapture
	stream  string
}

func (w liveCommandWriter) Write(data []byte) (int, error) {
	w.capture.mu.Lock()
	defer w.capture.mu.Unlock()
	_, _ = w.capture.output.Write(data)
	pending := w.capture.pending[w.stream] + string(data)
	for {
		index := strings.IndexAny(pending, "\r\n")
		if index < 0 {
			break
		}
		line := strings.TrimSpace(pending[:index])
		pending = strings.TrimLeft(pending[index+1:], "\r\n")
		if line != "" {
			w.capture.sink(w.stream, line)
		}
	}
	w.capture.pending[w.stream] = pending
	return len(data), nil
}

func (capture *liveCommandCapture) flush() {
	capture.mu.Lock()
	defer capture.mu.Unlock()
	for stream, pending := range capture.pending {
		if line := strings.TrimSpace(pending); line != "" {
			capture.sink(stream, line)
		}
		capture.pending[stream] = ""
	}
}

func runCommandStreaming(cmd *exec.Cmd, sink deployLogSink) ([]byte, error) {
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error {
		if cmd.Process == nil {
			return os.ErrProcessDone
		}
		return syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
	}
	cmd.WaitDelay = time.Second
	if sink == nil {
		return cmd.CombinedOutput()
	}
	capture := &liveCommandCapture{pending: map[string]string{}, sink: sink}
	cmd.Stdout = liveCommandWriter{capture: capture, stream: "stdout"}
	cmd.Stderr = liveCommandWriter{capture: capture, stream: "stderr"}
	err := cmd.Run()
	capture.flush()
	return append([]byte(nil), capture.output.Bytes()...), err
}

func emitDeployLog(sink deployLogSink, line string) {
	if sink != nil && strings.TrimSpace(line) != "" {
		sink("deploy", line)
	}
}

func appendDeploymentEnv(base []string, deploymentEnv map[string]string) []string {
	if len(deploymentEnv) == 0 {
		return base
	}
	next := append([]string{}, base...)
	names := make([]string, 0, len(deploymentEnv))
	for name := range deploymentEnv {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		next = append(next, name+"="+deploymentEnv[name])
	}
	return next
}

func firstDeployJob(payloads []deployJobPayload) *deployJob {
	if len(payloads) == 0 {
		return nil
	}
	payload := payloads[0]
	job := deployJob{
		ID:                 payload.ID,
		Type:               payload.Type,
		MachineID:          payload.MachineID,
		ApplicationID:      payload.AppID,
		DeploymentID:       payload.DeploymentID,
		ComposeYAML:        payload.ComposeYAML,
		ManagedFiles:       payload.ManagedFiles,
		ReservedHostPorts:  payload.ReservedHostPorts,
		Status:             payload.Status,
		CleanupDeployments: payload.CleanupDeployments,
		CleanupWireGuard:   payload.CleanupWireGuard,
	}
	if job.Type == "" {
		job.Type = "deploy"
	}
	if job.DeploymentID == "" {
		job.DeploymentID = job.ID
	}
	if strings.TrimSpace(job.ComposeYAML) == "" && job.Type != "deregister_machine" {
		return nil
	}
	return &job
}

func runJob(job *deployJob, machineWireGuardIP string, backendURL string, machineToken string) ([]string, []publicEndpointReport, error) {
	return runJobWithLogSink(job, machineWireGuardIP, backendURL, machineToken, nil)
}

func runJobWithLogSink(job *deployJob, machineWireGuardIP string, backendURL string, machineToken string, sink deployLogSink) ([]string, []publicEndpointReport, error) {
	return runJobWithContext(context.Background(), job, machineWireGuardIP, backendURL, machineToken, sink)
}

func runJobWithContext(ctx context.Context, job *deployJob, machineWireGuardIP string, backendURL string, machineToken string, sink deployLogSink) ([]string, []publicEndpointReport, error) {
	if job.Type == "deregister_machine" {
		logs, err := runDeregisterMachineJob(job)
		return logs, nil, err
	}
	return runComposeDeploymentWithContext(ctx, job, machineWireGuardIP, backendURL, machineToken, sink)
}

func durationFromPollAfter(seconds int, fallback time.Duration) time.Duration {
	if seconds <= 0 {
		return fallback
	}
	if seconds > 60 {
		seconds = 60
	}
	return time.Duration(seconds) * time.Second
}

func boundedBackoff(current time.Duration) time.Duration {
	next := current * 2
	if next < 5*time.Second {
		return 5 * time.Second
	}
	if next > 30*time.Second {
		return 30 * time.Second
	}
	return next
}

func runComposeDeployment(job *deployJob, machineWireGuardIP string, backendURL string, machineToken string) ([]string, []publicEndpointReport, error) {
	return runComposeDeploymentWithLogSink(job, machineWireGuardIP, backendURL, machineToken, nil)
}

func runComposeDeploymentWithLogSink(job *deployJob, machineWireGuardIP string, backendURL string, machineToken string, sink deployLogSink) ([]string, []publicEndpointReport, error) {
	return runComposeDeploymentWithContext(context.Background(), job, machineWireGuardIP, backendURL, machineToken, sink)
}

func runComposeDeploymentWithContext(ctx context.Context, job *deployJob, machineWireGuardIP string, backendURL string, machineToken string, sink deployLogSink) ([]string, []publicEndpointReport, error) {
	appName := extractAppNameFromCompose(job.ComposeYAML)
	action := extractActionFromCompose(job.ComposeYAML)
	destroyVolumes := extractDestroyVolumesFromCompose(job.ComposeYAML)
	deploymentID := strings.TrimSpace(job.DeploymentID)
	if deploymentID == "" {
		deploymentID = job.ID
	}
	projectName := composeProjectName(deploymentID)
	useProjectFlag := true
	deploymentDir, err := ensureDeploymentDir(deploymentID)
	if err != nil {
		return nil, nil, err
	}
	composePath := filepath.Join(deploymentDir, "docker-compose.generated.yml")
	runComposePath := composePath
	pendingComposePath := ""
	envPath := filepath.Join(deploymentDir, ".env")
	dockerConfigDir := filepath.Join(deploymentDir, "docker-config")
	metadataPath := filepath.Join(deploymentDir, "deployment.json")
	existingMetadata, _ := loadDeploymentMetadata(metadataPath)
	generatedCompose := job.ComposeYAML
	allocatedPorts := map[string]int{}
	expectedServices := []string{}
	publicEndpoints := []publicEndpointReport{}
	deployPrepLogs := []string{}
	deploymentEnv := map[string]string{}
	managedFileMounts := []managedFileMount{}
	secretBundle := deploymentSecretBundle{}
	var envErr error
	composeHash := sha256Hex(job.ComposeYAML)
	dockerBin, err := resolveDockerBinary()
	if err != nil {
		return []string{err.Error()}, nil, err
	}
	if action != "destroy" {
		if _, postDeployErr := extractPostDeployCommands(job.ComposeYAML); postDeployErr != nil {
			return []string{postDeployErr.Error()}, nil, postDeployErr
		}
		if _, secretParseErr := extractRequiredSecretNames(job.ComposeYAML); secretParseErr != nil {
			return []string{secretParseErr.Error()}, nil, secretParseErr
		}
		var secretErr error
		secretBundle, secretErr = fetchDeploymentSecretsWithContext(ctx, backendURL, machineToken, deploymentID)
		if secretErr != nil {
			return []string{"failed to fetch deployment secrets"}, nil, secretErr
		}
		if len(secretBundle.ImagePullCredentials) > 0 {
			if err := ensureDockerConfigDir(dockerConfigDir); err != nil {
				return []string{"failed to prepare deployment Docker config"}, nil, err
			}
		}
		var managedFileErr error
		managedFileMounts, managedFileErr = materializeManagedFiles(deploymentDir, job.ManagedFiles)
		if managedFileErr != nil {
			return []string{managedFileErr.Error()}, nil, managedFileErr
		}
		occupiedPorts := occupiedLocalTCPPorts(machineWireGuardIP, publicHostPortStart, publicHostPortEnd)
		reservedHostPorts := mergeReservedPorts(job.ReservedHostPorts, occupiedPorts)
		if len(occupiedPorts) > 0 {
			message := fmt.Sprintf("detected %d occupied WireGuard host port(s); choosing an available public port", len(occupiedPorts))
			emitDeployLog(sink, message)
			deployPrepLogs = append(deployPrepLogs, message)
		}
		generatedCompose, allocatedPorts, expectedServices, publicEndpoints, err = generateComposeFileWithReservedPorts(job.ComposeYAML, appName, deploymentID, job.ApplicationID, machineWireGuardIP, existingMetadata.AllocatedPorts, reservedHostPorts, envPath, managedFileMounts, secretBundle.GeneratedEnv)
		if err != nil {
			return []string{err.Error()}, nil, err
		}
		generatedEnv := mergeLANEndpointEnv(secretBundle.GeneratedEnv, publicEndpoints)
		deploymentEnv, envErr = mergeDeploymentEnv(secretBundle.RuntimeSecrets, generatedEnv)
		if envErr != nil {
			return []string{envErr.Error()}, nil, envErr
		}
		if len(deploymentEnv) > 0 {
			if err := writeDeploymentEnvFile(envPath, deploymentEnv); err != nil {
				return []string{"failed to write deployment env file"}, nil, err
			}
		}
		composeHash = sha256Hex(generatedCompose)
		pendingComposePath = filepath.Join(deploymentDir, "docker-compose.generated.pending.yml")
		runComposePath = pendingComposePath
		defer func() {
			if pendingComposePath != "" {
				_ = os.Remove(pendingComposePath)
			}
		}()
		if err := os.WriteFile(runComposePath, []byte(generatedCompose), 0o644); err != nil {
			return nil, nil, err
		}
		projectName, useProjectFlag = composeProjectForContent(generatedCompose, appName)
		loginLogs, loginErr := dockerLoginForImagePullCredentialsWithContext(ctx, dockerBin, dockerConfigDir, secretBundle.ImagePullCredentials)
		if loginErr != nil {
			return loginLogs, nil, loginErr
		}
		deployPrepLogs = append(deployPrepLogs, loginLogs...)
		if existingMetadata.DeploymentID != "" && existingMetadata.GeneratedComposePath != "" && !existingMetadata.SelfHealDisabled {
			existingMetadata.SelfHealDisabled = true
			if err := saveDeploymentMetadata(metadataPath, existingMetadata); err != nil {
				return []string{"failed to mark previous deployment state as pending replacement"}, nil, err
			}
		}
	} else if existingMetadata.GeneratedComposePath != "" {
		composePath = existingMetadata.GeneratedComposePath
		if generatedData, readErr := os.ReadFile(composePath); readErr == nil {
			projectName, useProjectFlag = composeProjectForContent(string(generatedData), appName)
		} else if existingMetadata.ProjectName != "" {
			projectName = existingMetadata.ProjectName
		}
	}

	dockerEnv := appendDeploymentEnv(os.Environ(), deploymentEnv)
	if action != "destroy" && directoryExists(dockerConfigDir) {
		dockerEnv = append(dockerEnv, "DOCKER_CONFIG="+dockerConfigDir)
	}
	preLogs := append([]string{}, deployPrepLogs...)
	if action != "destroy" && directoryExists(dockerConfigDir) {
		pullArgs := composeCommandArgs(projectName, useProjectFlag, runComposePath, "pull")
		pullCmd := exec.CommandContext(ctx, dockerBin, pullArgs...)
		pullCmd.Env = dockerEnv
		pullCommand := fmt.Sprintf("running: %s %s", dockerBin, strings.Join(pullArgs, " "))
		emitDeployLog(sink, pullCommand)
		output, err := runCommandStreaming(pullCmd, sink)
		preLogs = append(preLogs, pullCommand)
		preLogs = append(preLogs, formatCommandOutput(output)...)
		if err != nil {
			if exitErr, ok := err.(*exec.ExitError); ok {
				preLogs = append(preLogs, fmt.Sprintf("docker compose pull exit code: %d", exitErr.ExitCode()))
			}
			return preLogs, nil, fmt.Errorf("docker compose pull failed: %w", err)
		}
	}

	commandArgs := composeCommandArgs(projectName, useProjectFlag, runComposePath)
	switch action {
	case "destroy":
		commandArgs = append(commandArgs, "down", "--remove-orphans")
		if destroyVolumes {
			commandArgs = append(commandArgs, "--volumes")
		}
	default:
		action = "deploy"
		commandArgs = append(commandArgs, "up", "-d", "--remove-orphans")
	}
	commandLine := dockerBin + " " + strings.Join(commandArgs, " ")
	emitDeployLog(sink, fmt.Sprintf("running: %s", commandLine))
	cmd := exec.CommandContext(ctx, dockerBin, commandArgs...)
	cmd.Env = dockerEnv
	output, err := runCommandStreaming(cmd, sink)
	logs := []string{fmt.Sprintf("project: %s", projectName), fmt.Sprintf("app: %s", appName), fmt.Sprintf("action: %s", action), fmt.Sprintf("running: %s", commandLine)}
	logs = append(logs, preLogs...)
	logs = append(logs, formatCommandOutput(output)...)
	if err != nil && action == "deploy" && isRetryableDockerPortPublishError(string(output)) {
		retryLogs, retryAllocatedPorts, retryEndpoints, retryComposeHash, retryErr := retryComposeDeployWithNextPublicPort(ctx, job, appName, deploymentID, machineWireGuardIP, existingMetadata, managedFileMounts, secretBundle, envPath, runComposePath, projectName, useProjectFlag, dockerBin, dockerEnv, allocatedPorts, sink)
		logs = append(logs, retryLogs...)
		if retryErr == nil {
			allocatedPorts = retryAllocatedPorts
			publicEndpoints = retryEndpoints
			composeHash = retryComposeHash
			err = nil
		} else {
			err = retryErr
		}
	}
	if err != nil {
		if exitErr, ok := err.(*exec.ExitError); ok {
			logs = append(logs, fmt.Sprintf("docker compose exit code: %d", exitErr.ExitCode()))
		}
		return logs, nil, fmt.Errorf("docker compose failed: %w", err)
	}
	if action == "deploy" {
		postDeployCommands, postDeployErr := extractPostDeployCommands(job.ComposeYAML)
		if postDeployErr != nil {
			return append(logs, postDeployErr.Error()), publicEndpoints, postDeployErr
		}
		postDeployLogs, postDeployErr := runPostDeployCommandsWithContext(ctx, dockerBin, dockerEnv, projectName, useProjectFlag, runComposePath, postDeployCommands, sink)
		logs = append(logs, postDeployLogs...)
		if postDeployErr != nil {
			return logs, publicEndpoints, postDeployErr
		}
		if pendingComposePath != "" {
			if err := os.Rename(pendingComposePath, composePath); err != nil {
				return logs, publicEndpoints, fmt.Errorf("failed to commit generated compose: %w", err)
			}
		}
		metadata := deploymentMetadata{
			DeploymentID:         deploymentID,
			ApplicationID:        job.ApplicationID,
			ComposeHash:          composeHash,
			ProjectName:          projectName,
			GeneratedComposePath: composePath,
			AllocatedPorts:       allocatedPorts,
			LastAppliedAt:        time.Now().UTC().Format(time.RFC3339),
		}
		if err := saveDeploymentMetadata(metadataPath, metadata); err != nil {
			return logs, publicEndpoints, err
		}
		status := inspectDeploymentStatusWithContext(ctx, dockerBin, projectName, expectedServices)
		logs = append(logs, fmt.Sprintf("deployment status: %s", status))
	} else if action == "destroy" {
		if err := os.RemoveAll(deploymentDir); err != nil {
			return logs, publicEndpoints, fmt.Errorf("failed to remove deployment metadata: %w", err)
		}
		logs = append(logs, fmt.Sprintf("removed Nethera deployment metadata for %s", deploymentID))
	}
	logs = append(logs, "docker compose completed successfully")
	return logs, publicEndpoints, nil
}

func isRetryableDockerPortPublishError(output string) bool {
	lower := strings.ToLower(output)
	if !strings.Contains(lower, "port") {
		return false
	}
	return strings.Contains(lower, "address already in use") ||
		strings.Contains(lower, "port is already allocated") ||
		strings.Contains(lower, "ports are not available") ||
		strings.Contains(lower, "/forwards/expose returned unexpected status")
}

func retryComposeDeployWithNextPublicPort(
	ctx context.Context,
	job *deployJob,
	appName string,
	deploymentID string,
	machineWireGuardIP string,
	existingMetadata deploymentMetadata,
	managedFileMounts []managedFileMount,
	secretBundle deploymentSecretBundle,
	envPath string,
	runComposePath string,
	projectName string,
	useProjectFlag bool,
	dockerBin string,
	dockerEnv []string,
	failedAllocatedPorts map[string]int,
	sink deployLogSink,
) ([]string, map[string]int, []publicEndpointReport, string, error) {
	if len(failedAllocatedPorts) == 0 {
		return nil, nil, nil, "", fmt.Errorf("docker compose failed after port publish error, but no allocated public ports were available to retry")
	}
	failedPorts := make([]int, 0, len(failedAllocatedPorts))
	for _, port := range failedAllocatedPorts {
		if port > 0 {
			failedPorts = append(failedPorts, port)
		}
	}
	sort.Ints(failedPorts)
	message := fmt.Sprintf("Docker rejected public port %s; retrying with a new Nethera host port", joinPortList(failedPorts))
	emitDeployLog(sink, message)

	occupiedPorts := occupiedLocalTCPPorts(machineWireGuardIP, publicHostPortStart, publicHostPortEnd)
	reservedHostPorts := mergeReservedPorts(mergeReservedPorts(job.ReservedHostPorts, occupiedPorts), failedPorts)
	generatedCompose, allocatedPorts, _, publicEndpoints, err := generateComposeFileWithReservedPorts(job.ComposeYAML, appName, deploymentID, job.ApplicationID, machineWireGuardIP, existingMetadata.AllocatedPorts, reservedHostPorts, envPath, managedFileMounts, secretBundle.GeneratedEnv)
	if err != nil {
		return []string{message, err.Error()}, nil, nil, "", err
	}
	generatedEnv := mergeLANEndpointEnv(secretBundle.GeneratedEnv, publicEndpoints)
	deploymentEnv, err := mergeDeploymentEnv(secretBundle.RuntimeSecrets, generatedEnv)
	if err != nil {
		return []string{message, err.Error()}, nil, nil, "", err
	}
	if len(deploymentEnv) > 0 {
		if err := writeDeploymentEnvFile(envPath, deploymentEnv); err != nil {
			return []string{message, "failed to write deployment env file"}, nil, nil, "", err
		}
	}
	if err := os.WriteFile(runComposePath, []byte(generatedCompose), 0o644); err != nil {
		return []string{message}, nil, nil, "", err
	}
	commandArgs := composeCommandArgs(projectName, useProjectFlag, runComposePath, "up", "-d", "--remove-orphans")
	commandLine := dockerBin + " " + strings.Join(commandArgs, " ")
	emitDeployLog(sink, fmt.Sprintf("running: %s", commandLine))
	cmd := exec.CommandContext(ctx, dockerBin, commandArgs...)
	cmd.Env = dockerEnv
	output, err := runCommandStreaming(cmd, sink)
	logs := []string{message, fmt.Sprintf("running: %s", commandLine)}
	logs = append(logs, formatCommandOutput(output)...)
	if err != nil {
		if exitErr, ok := err.(*exec.ExitError); ok {
			logs = append(logs, fmt.Sprintf("docker compose retry exit code: %d", exitErr.ExitCode()))
		}
		return logs, nil, nil, "", fmt.Errorf("docker compose retry failed: %w", err)
	}
	return logs, allocatedPorts, publicEndpoints, sha256Hex(generatedCompose), nil
}

func joinPortList(ports []int) string {
	if len(ports) == 0 {
		return "unknown"
	}
	parts := make([]string, 0, len(ports))
	for _, port := range ports {
		parts = append(parts, fmt.Sprintf("%d", port))
	}
	return strings.Join(parts, ", ")
}

func extractPostDeployCommands(composeYAML string) ([]postDeployCommand, error) {
	var root yaml.Node
	if err := yaml.Unmarshal([]byte(composeYAML), &root); err != nil {
		return nil, fmt.Errorf("compose yaml is invalid: %w", err)
	}
	if len(root.Content) == 0 || root.Content[0].Kind != yaml.MappingNode {
		return nil, nil
	}
	servicesNode := yamlMappingValue(root.Content[0], "services")
	if servicesNode == nil {
		return nil, nil
	}
	if servicesNode.Kind != yaml.MappingNode {
		return nil, fmt.Errorf("services must be a mapping")
	}
	commands := []postDeployCommand{}
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
		if yamlMappingValue(netheraNode, "setup") != nil {
			return nil, fmt.Errorf("service %s nethera.setup has been renamed to nethera.postDeploy", serviceName)
		}
		postDeployNode := yamlMappingValue(netheraNode, "postDeploy")
		if postDeployNode == nil {
			continue
		}
		if postDeployNode.Kind != yaml.SequenceNode {
			return nil, fmt.Errorf("service %s nethera.postDeploy must be a list of shell commands", serviceName)
		}
		if len(postDeployNode.Content) == 0 {
			return nil, fmt.Errorf("service %s nethera.postDeploy must include at least one command", serviceName)
		}
		if len(postDeployNode.Content) > postDeployMaxCommands {
			return nil, fmt.Errorf("service %s nethera.postDeploy supports at most %d commands", serviceName, postDeployMaxCommands)
		}
		for _, commandNode := range postDeployNode.Content {
			if commandNode.Kind != yaml.ScalarNode {
				return nil, fmt.Errorf("service %s nethera.postDeploy entries must be shell command strings", serviceName)
			}
			command := strings.TrimSpace(commandNode.Value)
			if command == "" {
				return nil, fmt.Errorf("service %s nethera.postDeploy contains an empty command", serviceName)
			}
			if len(command) > postDeployMaxCommandLength {
				return nil, fmt.Errorf("service %s nethera.postDeploy command is too long; maximum is %d characters", serviceName, postDeployMaxCommandLength)
			}
			commands = append(commands, postDeployCommand{ServiceName: serviceName, Command: command})
		}
	}
	return commands, nil
}

func runPostDeployCommands(dockerBin string, dockerEnv []string, projectName string, useProjectFlag bool, composePath string, commands []postDeployCommand) ([]string, error) {
	return runPostDeployCommandsWithLogSink(dockerBin, dockerEnv, projectName, useProjectFlag, composePath, commands, nil)
}

func runPostDeployCommandsWithLogSink(dockerBin string, dockerEnv []string, projectName string, useProjectFlag bool, composePath string, commands []postDeployCommand, sink deployLogSink) ([]string, error) {
	return runPostDeployCommandsWithContext(context.Background(), dockerBin, dockerEnv, projectName, useProjectFlag, composePath, commands, sink)
}

func runPostDeployCommandsWithContext(ctx context.Context, dockerBin string, dockerEnv []string, projectName string, useProjectFlag bool, composePath string, commands []postDeployCommand, sink deployLogSink) ([]string, error) {
	if len(commands) == 0 {
		return nil, nil
	}
	logs := []string{"postDeploy: starting"}
	emitDeployLog(sink, logs[0])
	readyServices := map[string]bool{}
	for _, item := range commands {
		if !readyServices[item.ServiceName] {
			readyLogs, err := waitForPostDeployExecWithContext(ctx, dockerBin, dockerEnv, projectName, useProjectFlag, composePath, item.ServiceName, sink)
			logs = append(logs, readyLogs...)
			if err != nil {
				return logs, err
			}
			readyServices[item.ServiceName] = true
		}
		commandLogs, err := runPostDeployCommandWithContext(ctx, dockerBin, dockerEnv, projectName, useProjectFlag, composePath, item, sink)
		logs = append(logs, commandLogs...)
		if err != nil {
			return logs, err
		}
	}
	logs = append(logs, "postDeploy: completed successfully")
	emitDeployLog(sink, "postDeploy: completed successfully")
	return logs, nil
}

func waitForPostDeployExec(dockerBin string, dockerEnv []string, projectName string, useProjectFlag bool, composePath string, serviceName string) ([]string, error) {
	return waitForPostDeployExecWithLogSink(dockerBin, dockerEnv, projectName, useProjectFlag, composePath, serviceName, nil)
}

func waitForPostDeployExecWithLogSink(dockerBin string, dockerEnv []string, projectName string, useProjectFlag bool, composePath string, serviceName string, sink deployLogSink) ([]string, error) {
	return waitForPostDeployExecWithContext(context.Background(), dockerBin, dockerEnv, projectName, useProjectFlag, composePath, serviceName, sink)
}

func waitForPostDeployExecWithContext(ctx context.Context, dockerBin string, dockerEnv []string, projectName string, useProjectFlag bool, composePath string, serviceName string, sink deployLogSink) ([]string, error) {
	deadline := time.Now().Add(postDeployExecReadyTimeout)
	logs := []string{fmt.Sprintf("postDeploy %s: waiting for container exec", serviceName)}
	emitDeployLog(sink, logs[0])
	var lastErr error
	for {
		args := composeCommandArgs(projectName, useProjectFlag, composePath, "exec", "-T", serviceName, "sh", "-lc", "true")
		attemptCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
		cmd := exec.CommandContext(attemptCtx, dockerBin, args...)
		cmd.Env = dockerEnv
		output, err := runCommandStreaming(cmd, nil)
		cancel()
		if err == nil {
			logs = append(logs, fmt.Sprintf("postDeploy %s: container exec ready", serviceName))
			emitDeployLog(sink, logs[len(logs)-1])
			return logs, nil
		}
		lastErr = fmt.Errorf("docker compose exec readiness failed: %w: %s", err, summarizeBody(output))
		if time.Now().After(deadline) {
			logs = append(logs, fmt.Sprintf("postDeploy %s: container exec not ready", serviceName))
			emitDeployLog(sink, logs[len(logs)-1])
			return logs, lastErr
		}
		select {
		case <-ctx.Done():
			return logs, ctx.Err()
		case <-time.After(postDeployExecReadyPoll):
		}
	}
}

func runPostDeployCommand(dockerBin string, dockerEnv []string, projectName string, useProjectFlag bool, composePath string, item postDeployCommand) ([]string, error) {
	return runPostDeployCommandWithLogSink(dockerBin, dockerEnv, projectName, useProjectFlag, composePath, item, nil)
}

func runPostDeployCommandWithLogSink(dockerBin string, dockerEnv []string, projectName string, useProjectFlag bool, composePath string, item postDeployCommand, sink deployLogSink) ([]string, error) {
	return runPostDeployCommandWithContext(context.Background(), dockerBin, dockerEnv, projectName, useProjectFlag, composePath, item, sink)
}

func runPostDeployCommandWithContext(ctx context.Context, dockerBin string, dockerEnv []string, projectName string, useProjectFlag bool, composePath string, item postDeployCommand, sink deployLogSink) ([]string, error) {
	args := composeCommandArgs(projectName, useProjectFlag, composePath, "exec", "-T", item.ServiceName, "sh", "-lc", item.Command)
	commandCtx, cancel := context.WithTimeout(ctx, postDeployCommandTimeout)
	defer cancel()
	cmd := exec.CommandContext(commandCtx, dockerBin, args...)
	cmd.Env = dockerEnv
	logs := []string{fmt.Sprintf("postDeploy %s: %s", item.ServiceName, item.Command)}
	emitDeployLog(sink, logs[0])
	output, err := runCommandStreaming(cmd, sink)
	logs = append(logs, formatPostDeployOutput(output)...)
	if commandCtx.Err() == context.DeadlineExceeded {
		return logs, fmt.Errorf("postDeploy command timed out for service %s", item.ServiceName)
	}
	if err != nil {
		if exitErr, ok := err.(*exec.ExitError); ok {
			logs = append(logs, fmt.Sprintf("postDeploy %s exit code: %d", item.ServiceName, exitErr.ExitCode()))
		}
		return logs, fmt.Errorf("postDeploy command failed for service %s: %w", item.ServiceName, err)
	}
	return logs, nil
}

func formatPostDeployOutput(output []byte) []string {
	text := strings.TrimSpace(string(output))
	if text == "" {
		return []string{"postDeploy output: <empty>"}
	}
	lines := strings.Split(text, "\n")
	formatted := make([]string, 0, len(lines))
	for index, line := range lines {
		if index >= postDeployMaxOutputLines {
			formatted = append(formatted, "postDeploy output: ...")
			break
		}
		trimmed := strings.TrimSpace(line)
		if trimmed == "" {
			continue
		}
		if len(trimmed) > postDeployMaxOutputLineLength {
			trimmed = trimmed[:postDeployMaxOutputLineLength] + "..."
		}
		formatted = append(formatted, fmt.Sprintf("postDeploy output: %s", trimmed))
	}
	if len(formatted) == 0 {
		return []string{"postDeploy output: <empty>"}
	}
	return formatted
}

func composeProjectForContent(composeContent, appName string) (string, bool) {
	if composeName := extractComposeProjectName(composeContent); composeName != "" {
		return composeName, false
	}
	return composeProjectName(appName), true
}

func composeProjectForMetadata(metadata deploymentMetadata) (string, bool) {
	if generatedData, err := os.ReadFile(metadata.GeneratedComposePath); err == nil {
		if composeName := extractComposeProjectName(string(generatedData)); composeName != "" {
			return composeName, false
		}
	}
	return metadata.ProjectName, true
}

func composeCommandArgs(projectName string, useProjectFlag bool, composePath string, extra ...string) []string {
	args := []string{"compose"}
	if useProjectFlag {
		args = append(args, "-p", projectName)
	}
	args = append(args, "-f", composePath)
	args = append(args, extra...)
	return args
}

func runDeregisterMachineJob(job *deployJob) ([]string, error) {
	logs := []string{
		fmt.Sprintf("job: %s", job.ID),
		"action: deregister_machine",
		fmt.Sprintf("cleanupDeployments: %t", job.CleanupDeployments),
		fmt.Sprintf("cleanupWireGuard: %t", job.CleanupWireGuard),
	}
	if job.CleanupWireGuard {
		logs = append(logs, "wireguard cleanup is not implemented; no WireGuard configuration was changed")
		return logs, fmt.Errorf("wireguard cleanup is not implemented")
	}
	if job.CleanupDeployments {
		cleanupLogs, err := cleanupManagedDeployments()
		logs = append(logs, cleanupLogs...)
		if err != nil {
			return logs, err
		}
	} else {
		logs = append(logs, "deployment cleanup was not requested; local containers were left running")
	}
	logs = append(logs, "deregister_machine completed successfully")
	return logs, nil
}

func cleanupManagedDeployments() ([]string, error) {
	root := deploymentsStateDir()
	entries, err := os.ReadDir(root)
	if err != nil {
		if os.IsNotExist(err) {
			return []string{"no Nethera deployment metadata found"}, nil
		}
		return nil, err
	}
	dockerBin, err := resolveDockerBinary()
	if err != nil {
		return []string{err.Error()}, err
	}
	logs := []string{}
	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		deploymentDir := filepath.Join(root, entry.Name())
		metadata, err := loadDeploymentMetadata(metadataPathForDeployment(deploymentDir))
		if err != nil {
			logs = append(logs, fmt.Sprintf("cleanup skipped %s: %v", entry.Name(), err))
			continue
		}
		if metadata.DeploymentID == "" || metadata.ProjectName == "" || metadata.GeneratedComposePath == "" {
			logs = append(logs, fmt.Sprintf("cleanup skipped %s: incomplete metadata", entry.Name()))
			continue
		}
		if _, err := os.Stat(metadata.GeneratedComposePath); err != nil {
			return logs, fmt.Errorf("refusing cleanup for deployment %s: compose file unavailable: %w", metadata.DeploymentID, err)
		}
		projectName, useProjectFlag := composeProjectForMetadata(metadata)
		managed, hasContainers, labelLogs, err := projectHasOnlyNetheraManagedContainers(dockerBin, projectName)
		logs = append(logs, labelLogs...)
		if err != nil {
			return logs, err
		}
		if !managed {
			return logs, fmt.Errorf("refusing cleanup for project %s: containers are not exclusively labelled nethera.managed=true", projectName)
		}
		if hasContainers {
			commandArgs := composeCommandArgs(projectName, useProjectFlag, metadata.GeneratedComposePath, "down", "--remove-orphans")
			cmd := exec.Command(dockerBin, commandArgs...)
			cmd.Env = os.Environ()
			output, err := cmd.CombinedOutput()
			logs = append(logs, fmt.Sprintf("cleanup running: %s %s", dockerBin, strings.Join(commandArgs, " ")))
			logs = append(logs, formatCommandOutput(output)...)
			if err != nil {
				if exitErr, ok := err.(*exec.ExitError); ok {
					logs = append(logs, fmt.Sprintf("docker compose exit code: %d", exitErr.ExitCode()))
				}
				return logs, fmt.Errorf("docker compose cleanup failed: %w", err)
			}
		}
		if err := os.RemoveAll(deploymentDir); err != nil {
			return logs, fmt.Errorf("failed to remove deployment metadata for %s: %w", metadata.DeploymentID, err)
		}
		logs = append(logs, fmt.Sprintf("removed Nethera deployment metadata for %s", metadata.DeploymentID))
	}
	if len(logs) == 0 {
		logs = append(logs, "no Nethera-managed deployments found")
	}
	return logs, nil
}

func projectHasOnlyNetheraManagedContainers(dockerBin, projectName string) (bool, bool, []string, error) {
	total, err := countContainersByFilters(dockerBin, []string{"label=com.docker.compose.project=" + projectName})
	if err != nil {
		return false, false, nil, err
	}
	managed, err := countContainersByFilters(dockerBin, []string{"label=com.docker.compose.project=" + projectName, "label=nethera.managed=true"})
	if err != nil {
		return false, false, nil, err
	}
	logs := []string{fmt.Sprintf("project %s containers: total=%d netheraManaged=%d", projectName, total, managed)}
	if total == 0 {
		logs = append(logs, "no containers found for project; metadata will be removed without docker compose down")
		return true, false, logs, nil
	}
	return total == managed, true, logs, nil
}

func countContainersByFilters(dockerBin string, filters []string) (int, error) {
	args := []string{"ps", "-a"}
	for _, filter := range filters {
		args = append(args, "--filter", filter)
	}
	args = append(args, "--format", "{{.ID}}")
	cmd := exec.Command(dockerBin, args...)
	output, err := cmd.CombinedOutput()
	if err != nil {
		return 0, fmt.Errorf("docker ps failed: %w: %s", err, summarizeBody(output))
	}
	trimmed := strings.TrimSpace(string(output))
	if trimmed == "" {
		return 0, nil
	}
	return len(strings.Split(trimmed, "\n")), nil
}
