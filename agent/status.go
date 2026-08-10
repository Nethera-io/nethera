package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"syscall"
	"time"

	"gopkg.in/yaml.v3"
)

var (
	cachedMachineSnapshot   map[string]any
	cachedMachineSnapshotAt time.Time
)

func statusSnapshotInterval() time.Duration {
	seconds := agentLogStreamEnvInt("STATUS_SNAPSHOT_INTERVAL_SECONDS", 30)
	if seconds < 5 {
		seconds = 5
	}
	return time.Duration(seconds) * time.Second
}

func cloneSnapshot(input map[string]any) map[string]any {
	out := make(map[string]any, len(input))
	for key, value := range input {
		out[key] = value
	}
	return out
}

func statusCommandEnv() []string {
	env := os.Environ()
	dir := filepath.Join(netheraStateDir(), "docker-status-config")
	if err := os.MkdirAll(dir, 0o700); err == nil {
		configPath := filepath.Join(dir, "config.json")
		if _, statErr := os.Stat(configPath); os.IsNotExist(statErr) {
			_ = os.WriteFile(configPath, []byte("{}\n"), 0o600)
		}
		env = append(env, "DOCKER_CONFIG="+dir)
	}
	env = append(env, "NO_AT_BRIDGE=1")
	return env
}

func reconcileLocalDeployments(machineWireGuardIP string) ([]string, error) {
	root := deploymentsStateDir()
	entries, err := os.ReadDir(root)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
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
		metadata, err := loadDeploymentMetadata(metadataPathForDeployment(filepath.Join(root, entry.Name())))
		if err != nil {
			logs = append(logs, fmt.Sprintf("reconcile skipped %s: %v", entry.Name(), err))
			continue
		}
		if metadata.ProjectName == "" || metadata.GeneratedComposePath == "" {
			logs = append(logs, fmt.Sprintf("reconcile skipped %s: incomplete metadata", entry.Name()))
			continue
		}
		if metadata.SelfHealDisabled {
			logs = append(logs, fmt.Sprintf("reconcile skipped %s: self-heal disabled after failed deployment", metadata.DeploymentID))
			continue
		}
		if _, err := os.Stat(metadata.GeneratedComposePath); err != nil {
			logs = append(logs, fmt.Sprintf("reconcile skipped %s: compose file unavailable: %v", metadata.DeploymentID, err))
			continue
		}
		projectName, useProjectFlag := composeProjectForMetadata(metadata)
		if deploymentRuntimeStatus(dockerBin, projectName, metadata.GeneratedComposePath) == "running" {
			continue
		}
		commandArgs := composeCommandArgs(projectName, useProjectFlag, metadata.GeneratedComposePath, "up", "-d", "--remove-orphans")
		reconcileCtx, cancelReconcile := context.WithTimeout(context.Background(), localDeploymentReconcileCommandTimeout())
		cmd := exec.CommandContext(reconcileCtx, dockerBin, commandArgs...)
		cmd.Env = statusCommandEnv()
		output, err := runCommandStreaming(cmd, nil)
		cancelReconcile()
		logs = append(logs, fmt.Sprintf("reconcile running: %s %s", dockerBin, strings.Join(commandArgs, " ")))
		logs = append(logs, formatCommandOutput(output)...)
		if reconcileCtx.Err() == context.DeadlineExceeded {
			logs = append(logs, fmt.Sprintf("reconcile failed for %s: docker compose timed out", metadata.DeploymentID))
			continue
		}
		if err != nil {
			logs = append(logs, fmt.Sprintf("reconcile failed for %s: %v", metadata.DeploymentID, err))
			continue
		}
		metadata.LastAppliedAt = time.Now().UTC().Format(time.RFC3339)
		_ = saveDeploymentMetadata(metadataPathForDeployment(filepath.Join(root, entry.Name())), metadata)
		logs = append(logs, fmt.Sprintf("reconciled deployment %s status=%s", metadata.DeploymentID, deploymentRuntimeStatus(dockerBin, projectName, metadata.GeneratedComposePath)))
	}
	_ = machineWireGuardIP
	return logs, nil
}

func localDeploymentReconcileCommandTimeout() time.Duration {
	seconds := agentLogStreamEnvInt("DEPLOY_RECONCILE_TIMEOUT_SECONDS", 900)
	if seconds < 30 {
		seconds = 30
	}
	return time.Duration(seconds) * time.Second
}

func inspectDeploymentStatus(dockerBin, projectName string, expectedServices []string) string {
	return inspectDeploymentStatusWithContext(context.Background(), dockerBin, projectName, expectedServices)
}

func inspectDeploymentStatusWithContext(ctx context.Context, dockerBin, projectName string, expectedServices []string) string {
	containers, err := inspectDeploymentContainersWithContext(ctx, dockerBin, projectName)
	if err != nil {
		return "unknown"
	}
	return summarizeContainerStatus(containers, len(expectedServices))
}

func inspectDeploymentContainers(dockerBin, projectName string) ([]containerStatusReport, error) {
	return inspectDeploymentContainersWithContext(context.Background(), dockerBin, projectName)
}

func inspectDeploymentContainersWithContext(ctx context.Context, dockerBin, projectName string) ([]containerStatusReport, error) {
	containers, err := inspectDeploymentContainersViaDockerAPI(ctx, projectName)
	if err == nil {
		return containers, nil
	}
	return inspectDeploymentContainersViaDockerCLI(ctx, dockerBin, projectName)
}

func summarizeContainerStatus(containers []containerStatusReport, expected int) string {
	if len(containers) == 0 {
		return "unknown"
	}
	if expected == 0 {
		expected = len(containers)
	}
	running := 0
	for _, container := range containers {
		if strings.EqualFold(container.State, "running") {
			running += 1
		}
	}
	if running == expected && len(containers) >= expected {
		return "running"
	}
	if running > 0 {
		return "degraded"
	}
	return "stopped"
}

func summarizeContainerStatusForCompose(containers []containerStatusReport, expectedServices []string, oneShotServices map[string]bool) string {
	if len(oneShotServices) == 0 {
		return summarizeContainerStatus(containers, len(expectedServices))
	}
	filtered := make([]containerStatusReport, 0, len(containers))
	for _, container := range containers {
		if oneShotServices[containerServiceName(container.Name)] {
			continue
		}
		filtered = append(filtered, container)
	}
	return summarizeContainerStatus(filtered, len(expectedServices))
}

func containerServiceName(containerName string) string {
	name := strings.TrimPrefix(strings.TrimSpace(containerName), "/")
	if name == "" {
		return ""
	}
	parts := strings.Split(name, "-")
	if len(parts) >= 3 {
		return strings.Join(parts[1:len(parts)-1], "-")
	}
	parts = strings.Split(name, "_")
	if len(parts) >= 3 {
		return strings.Join(parts[1:len(parts)-1], "_")
	}
	return ""
}

func composeRuntimeExpectations(composePath string) ([]string, map[string]bool) {
	content, err := os.ReadFile(composePath)
	if err != nil {
		return nil, map[string]bool{}
	}
	var root yaml.Node
	if err := yaml.Unmarshal(content, &root); err != nil || len(root.Content) == 0 || root.Content[0].Kind != yaml.MappingNode {
		return nil, map[string]bool{}
	}
	servicesNode := yamlMappingValue(root.Content[0], "services")
	oneShot := detectOneShotServices(servicesNode)
	expected := []string{}
	if servicesNode != nil && servicesNode.Kind == yaml.MappingNode {
		for index := 0; index+1 < len(servicesNode.Content); index += 2 {
			serviceName := strings.TrimSpace(servicesNode.Content[index].Value)
			if serviceName != "" && !oneShot[serviceName] {
				expected = append(expected, serviceName)
			}
		}
	}
	return expected, oneShot
}

func deploymentRuntimeStatus(dockerBin, projectName, composePath string) string {
	inspectCtx, cancelInspect := context.WithTimeout(context.Background(), 4*time.Second)
	containers, inspectErr := inspectDeploymentContainersWithContext(inspectCtx, dockerBin, projectName)
	cancelInspect()
	if inspectErr != nil {
		return "unknown"
	}
	expectedServices, oneShotServices := composeRuntimeExpectations(composePath)
	return summarizeContainerStatusForCompose(containers, expectedServices, oneShotServices)
}

func collectDeploymentReports() ([]deploymentStatusReport, error) {
	root := deploymentsStateDir()
	entries, err := os.ReadDir(root)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	dockerBin, err := resolveDockerBinary()
	if err != nil {
		return nil, err
	}
	reports := []deploymentStatusReport{}
	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		metadata, err := loadDeploymentMetadata(metadataPathForDeployment(filepath.Join(root, entry.Name())))
		if err != nil || metadata.DeploymentID == "" {
			continue
		}
		inspectCtx, cancelInspect := context.WithTimeout(context.Background(), 4*time.Second)
		containers, inspectErr := inspectDeploymentContainersWithContext(inspectCtx, dockerBin, metadata.ProjectName)
		cancelInspect()
		status := "unknown"
		if inspectErr == nil {
			expectedServices, oneShotServices := composeRuntimeExpectations(metadata.GeneratedComposePath)
			status = summarizeContainerStatusForCompose(containers, expectedServices, oneShotServices)
		}
		reports = append(reports, deploymentStatusReport{
			DeploymentID:     metadata.DeploymentID,
			Status:           status,
			ComposeHash:      metadata.ComposeHash,
			SelfHealDisabled: metadata.SelfHealDisabled,
			Containers:       containers,
		})
	}
	return reports, nil
}

func localDeploymentsNeedReconcile() bool {
	reports, err := collectDeploymentReports()
	if err != nil || len(reports) == 0 {
		return false
	}
	for _, report := range reports {
		if report.SelfHealDisabled {
			continue
		}
		if !strings.EqualFold(strings.TrimSpace(report.Status), "running") {
			return true
		}
	}
	return false
}

func collectMachineSnapshot(activeJob *deployJob) (map[string]any, error) {
	now := time.Now()
	if cachedMachineSnapshot != nil && now.Sub(cachedMachineSnapshotAt) < statusSnapshotInterval() {
		snapshot := cloneSnapshot(cachedMachineSnapshot)
		snapshot["activeJob"] = activeJobSnapshot(activeJob)
		return snapshot, nil
	}
	snapshot, err := collectMachineSnapshotFresh(activeJob)
	if err != nil {
		return nil, err
	}
	cached := cloneSnapshot(snapshot)
	cached["activeJob"] = nil
	cachedMachineSnapshot = cached
	cachedMachineSnapshotAt = now
	return snapshot, nil
}

func activeJobSnapshot(activeJob *deployJob) any {
	if activeJob == nil {
		return nil
	}
	return map[string]any{
		"id":           activeJob.ID,
		"deploymentId": activeJob.DeploymentID,
	}
}

func collectMachineSnapshotFresh(activeJob *deployJob) (map[string]any, error) {
	deploymentCounts := map[string]int{
		"running":  0,
		"degraded": 0,
		"failed":   0,
	}
	deploymentReports := []deploymentStatusReport{}
	if reports, err := collectDeploymentReports(); err == nil {
		deploymentReports = reports
		for _, report := range reports {
			switch report.Status {
			case "running":
				deploymentCounts["running"] += 1
			case "degraded", "stopped", "unknown":
				deploymentCounts["degraded"] += 1
			case "failed":
				deploymentCounts["failed"] += 1
			}
		}
	}

	updateState, _ := loadAgentUpdateState()
	dockerUp := dockerAvailable()
	snapshot := map[string]any{
		"agentVersion":        agentVersion(),
		"updateChannel":       agentUpdateChannel(),
		"os":                  runtime.GOOS,
		"arch":                normalizeRuntimeArch(runtime.GOARCH),
		"lastUpdateAttemptAt": updateState.LastAttemptAt,
		"lastUpdateError":     updateState.LastError,
		"wireguard": map[string]any{
			"up": wireGuardInterfaceExists("wg0"),
		},
		"docker": map[string]any{
			"up": dockerUp,
		},
		"lanAddresses":      collectLANAddresses(),
		"gpu":               collectGPUDiagnostics(dockerUp),
		"gpuMetrics":        collectGPUMetrics(),
		"deployments":       deploymentCounts,
		"deploymentReports": deploymentReports,
		"activeJob":         nil,
	}
	snapshot["activeJob"] = activeJobSnapshot(activeJob)
	if cpu := readCPUSnapshot(); cpu != nil {
		snapshot["cpu"] = cpu
	}
	if memory := readMemorySnapshot(); memory != nil {
		snapshot["memory"] = memory
	}
	if disk := readDiskSnapshot(netheraStateDir()); disk != nil {
		snapshot["disk"] = disk
	}
	return snapshot, nil
}

func collectLANAddresses() []string {
	interfaces, err := net.Interfaces()
	if err != nil {
		return nil
	}
	addresses := []string{}
	seen := map[string]bool{}
	for _, iface := range interfaces {
		if iface.Flags&net.FlagUp == 0 || iface.Flags&net.FlagLoopback != 0 {
			continue
		}
		if strings.HasPrefix(iface.Name, "docker") || strings.HasPrefix(iface.Name, "br-") || strings.HasPrefix(iface.Name, "veth") || strings.HasPrefix(iface.Name, "wg") {
			continue
		}
		addrs, err := iface.Addrs()
		if err != nil {
			continue
		}
		for _, addr := range addrs {
			var ip net.IP
			switch value := addr.(type) {
			case *net.IPNet:
				ip = value.IP
			case *net.IPAddr:
				ip = value.IP
			}
			if ip == nil || ip.IsLoopback() || ip.IsUnspecified() || ip.IsMulticast() {
				continue
			}
			if v4 := ip.To4(); v4 != nil {
				if !(v4[0] == 10 || (v4[0] == 172 && v4[1] >= 16 && v4[1] <= 31) || (v4[0] == 192 && v4[1] == 168)) {
					continue
				}
				text := v4.String()
				if !seen[text] {
					seen[text] = true
					addresses = append(addresses, text)
				}
			} else if ip.IsPrivate() {
				text := ip.String()
				if !seen[text] {
					seen[text] = true
					addresses = append(addresses, text)
				}
			}
		}
	}
	return addresses
}

func collectGPUDiagnostics(dockerUp bool) []any {
	checks := []any{}
	hostCheck := gpuCheck("host_nvidia_smi", "warning", "nvidia-smi was not found on the host")
	if path, err := findNvidiaSMI(); err == nil {
		output, runErr := runStatusCommand(4*time.Second, path, "--query-gpu=name,driver_version", "--format=csv,noheader")
		if runErr == nil && strings.TrimSpace(output) != "" {
			firstGPU := strings.Split(strings.TrimSpace(output), "\n")[0]
			hostCheck = gpuCheck("host_nvidia_smi", "ok", "host NVIDIA driver is visible")
			hostCheck["detail"] = fmt.Sprintf("%s (%s)", strings.TrimSpace(firstGPU), path)
		} else {
			hostCheck = gpuCheck("host_nvidia_smi", "error", "nvidia-smi exists but failed to query GPUs")
			hostCheck["path"] = path
			if runErr != nil {
				hostCheck["detail"] = shortStatusDetail(runErr.Error())
			}
		}
	}
	checks = append(checks, hostCheck)

	deviceCheck := gpuDeviceCheck()
	checks = append(checks, deviceCheck)

	if !dockerUp {
		checks = append(checks, gpuCheck("docker", "error", "Docker is not available"))
		return checks
	}
	checks = append(checks, gpuCheck("docker", "ok", "Docker is available"))

	runtimeCheck := gpuCheck("docker_nvidia_runtime", "warning", "Docker does not report an NVIDIA runtime")
	if runtimes, err := dockerRuntimeNames(4 * time.Second); err == nil {
		joined := strings.Join(runtimes, ", ")
		if strings.Contains(strings.ToLower(joined), "nvidia") {
			runtimeCheck = gpuCheck("docker_nvidia_runtime", "ok", "Docker reports an NVIDIA runtime")
		}
		runtimeCheck["detail"] = shortStatusDetail(joined)
	} else {
		runtimeCheck = gpuCheck("docker_nvidia_runtime", "warning", "could not inspect Docker runtimes")
		runtimeCheck["detail"] = shortStatusDetail(err.Error())
	}
	checks = append(checks, runtimeCheck)

	return checks
}

func collectGPUMetrics() []any {
	path, err := findNvidiaSMI()
	if err != nil {
		return nil
	}
	output, err := runStatusCommand(
		4*time.Second,
		path,
		"--query-gpu=index,name,uuid,driver_version,utilization.gpu,memory.used,memory.total,temperature.gpu",
		"--format=csv,noheader,nounits",
	)
	if err != nil || strings.TrimSpace(output) == "" {
		return nil
	}

	metrics := []any{}
	for _, line := range strings.Split(strings.TrimSpace(output), "\n") {
		parts := strings.Split(line, ",")
		if len(parts) < 8 {
			continue
		}
		for i := range parts {
			parts[i] = strings.TrimSpace(parts[i])
		}
		metric := map[string]any{
			"index":              parseOptionalInt(parts[0]),
			"name":               parts[1],
			"uuid":               parts[2],
			"driverVersion":      parts[3],
			"utilisationPercent": parseOptionalFloat(parts[4]),
			"memoryUsedMiB":      parseOptionalFloat(parts[5]),
			"memoryTotalMiB":     parseOptionalFloat(parts[6]),
			"temperatureCelsius": parseOptionalFloat(parts[7]),
		}
		metrics = append(metrics, metric)
	}
	return metrics
}

func gpuDeviceCheck() map[string]any {
	if isWSL() {
		check := gpuCheck("host_gpu_device", "warning", "WSL GPU device /dev/dxg was not found")
		if info, err := os.Stat("/dev/dxg"); err == nil && !info.IsDir() {
			check = gpuCheck("host_gpu_device", "ok", "WSL GPU device is present")
			check["detail"] = "/dev/dxg"
		}
		return check
	}

	check := gpuCheck("host_nvidia_devices", "warning", "no /dev/nvidia* devices were found")
	if devices, err := filepath.Glob("/dev/nvidia*"); err == nil && len(devices) > 0 {
		check = gpuCheck("host_nvidia_devices", "ok", "NVIDIA device files are present")
		check["detail"] = fmt.Sprintf("%d device file(s)", len(devices))
	}
	return check
}

func isWSL() bool {
	if _, err := os.Stat("/usr/lib/wsl/lib/nvidia-smi"); err == nil {
		return true
	}
	data, err := os.ReadFile("/proc/version")
	if err != nil {
		return false
	}
	version := strings.ToLower(string(data))
	return strings.Contains(version, "microsoft") || strings.Contains(version, "wsl")
}

func findNvidiaSMI() (string, error) {
	if path, err := exec.LookPath("nvidia-smi"); err == nil {
		return path, nil
	}
	candidates := []string{
		"/usr/bin/nvidia-smi",
		"/usr/local/bin/nvidia-smi",
		"/usr/local/cuda/bin/nvidia-smi",
		"/usr/lib/wsl/lib/nvidia-smi",
		"/bin/nvidia-smi",
	}
	for _, candidate := range candidates {
		if info, err := os.Stat(candidate); err == nil && !info.IsDir() {
			return candidate, nil
		}
	}
	return "", exec.ErrNotFound
}

func gpuCheck(name, status, message string) map[string]any {
	return map[string]any{
		"name":    name,
		"status":  status,
		"message": message,
	}
}

func parseOptionalFloat(value string) any {
	value = strings.TrimSpace(value)
	if value == "" || strings.EqualFold(value, "N/A") || strings.EqualFold(value, "[N/A]") {
		return nil
	}
	parsed, err := strconv.ParseFloat(value, 64)
	if err != nil {
		return nil
	}
	return parsed
}

func parseOptionalInt(value string) any {
	value = strings.TrimSpace(value)
	if value == "" || strings.EqualFold(value, "N/A") || strings.EqualFold(value, "[N/A]") {
		return nil
	}
	parsed, err := strconv.Atoi(value)
	if err != nil {
		return nil
	}
	return parsed
}

func runStatusCommand(timeout time.Duration, name string, args ...string) (string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, name, args...)
	cmd.Env = os.Environ()
	if filepath.Base(name) == "docker" || name == "docker" {
		cmd.Env = statusCommandEnv()
	}
	output, err := cmd.CombinedOutput()
	if ctx.Err() == context.DeadlineExceeded {
		return string(output), ctx.Err()
	}
	return string(output), err
}

func shortStatusDetail(value string) string {
	value = strings.TrimSpace(strings.ReplaceAll(value, "\n", " "))
	if len(value) > 240 {
		return value[:240]
	}
	return value
}

func agentUpdateChannel() string {
	if value := strings.TrimSpace(os.Getenv("NETHERA_AGENT_UPDATE_CHANNEL")); value != "" {
		return strings.ToLower(value)
	}
	return "stable"
}

var agentBuildVersion = "0.1.0"

func agentVersion() string {
	if value := strings.TrimSpace(os.Getenv("NETHERA_AGENT_VERSION")); value != "" {
		return value
	}
	return agentBuildVersion
}

func normalizeRuntimeArch(arch string) string {
	switch strings.ToLower(strings.TrimSpace(arch)) {
	case "x86_64":
		return "amd64"
	case "aarch64":
		return "arm64"
	default:
		return arch
	}
}

func dockerAvailable() bool {
	if dockerPing(4*time.Second) == nil {
		return true
	}
	return dockerPingViaCLI(4*time.Second) == nil
}

type dockerAPIEndpoint struct {
	baseURL    string
	socketPath string
}

func dockerAPIEndpointFromEnv() (dockerAPIEndpoint, error) {
	host := strings.TrimSpace(os.Getenv("DOCKER_HOST"))
	if host == "" {
		const defaultSocket = "/var/run/docker.sock"
		if info, err := os.Stat(defaultSocket); err == nil && !info.IsDir() {
			return dockerAPIEndpoint{baseURL: "http://docker", socketPath: defaultSocket}, nil
		}
		return dockerAPIEndpoint{}, fmt.Errorf("docker socket is not configured and %s is unavailable", defaultSocket)
	}
	parsed, err := url.Parse(host)
	if err != nil {
		return dockerAPIEndpoint{}, fmt.Errorf("invalid DOCKER_HOST: %w", err)
	}
	switch strings.ToLower(parsed.Scheme) {
	case "unix":
		if parsed.Path == "" {
			return dockerAPIEndpoint{}, fmt.Errorf("DOCKER_HOST unix socket path is empty")
		}
		return dockerAPIEndpoint{baseURL: "http://docker", socketPath: parsed.Path}, nil
	case "tcp":
		if parsed.Host == "" {
			return dockerAPIEndpoint{}, fmt.Errorf("DOCKER_HOST tcp host is empty")
		}
		return dockerAPIEndpoint{baseURL: "http://" + parsed.Host}, nil
	case "http", "https":
		if parsed.Host == "" {
			return dockerAPIEndpoint{}, fmt.Errorf("DOCKER_HOST %s host is empty", parsed.Scheme)
		}
		return dockerAPIEndpoint{baseURL: strings.TrimRight(host, "/")}, nil
	default:
		return dockerAPIEndpoint{}, fmt.Errorf("DOCKER_HOST scheme %q is not supported by direct health probes", parsed.Scheme)
	}
}

func dockerHTTPClient(timeout time.Duration, endpoint dockerAPIEndpoint) *http.Client {
	transport := &http.Transport{
		DisableKeepAlives: true,
	}
	if endpoint.socketPath != "" {
		socketPath := endpoint.socketPath
		transport.DialContext = func(ctx context.Context, _, _ string) (net.Conn, error) {
			var dialer net.Dialer
			return dialer.DialContext(ctx, "unix", socketPath)
		}
	}
	return &http.Client{Transport: transport, Timeout: timeout}
}

func dockerAPIGet(timeout time.Duration, path string) ([]byte, error) {
	endpoint, err := dockerAPIEndpointFromEnv()
	if err != nil {
		return nil, err
	}
	client := dockerHTTPClient(timeout, endpoint)
	if transport, ok := client.Transport.(*http.Transport); ok {
		defer transport.CloseIdleConnections()
	}
	response, err := client.Get(endpoint.baseURL + path)
	if err != nil {
		return nil, err
	}
	defer response.Body.Close()
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		body, _ := io.ReadAll(io.LimitReader(response.Body, 512))
		return nil, fmt.Errorf("docker API %s returned %d: %s", path, response.StatusCode, strings.TrimSpace(string(body)))
	}
	return io.ReadAll(io.LimitReader(response.Body, 2*1024*1024))
}

func dockerPing(timeout time.Duration) error {
	_, err := dockerAPIGet(timeout, "/_ping")
	return err
}

func dockerRuntimeNames(timeout time.Duration) ([]string, error) {
	body, err := dockerAPIGet(timeout, "/info")
	if err != nil {
		return dockerRuntimeNamesViaCLI(timeout)
	}
	return parseDockerRuntimeNames(body)
}

func parseDockerRuntimeNames(body []byte) ([]string, error) {
	var info struct {
		Runtimes map[string]any `json:"Runtimes"`
	}
	if err := json.Unmarshal(body, &info); err != nil {
		return nil, err
	}
	names := make([]string, 0, len(info.Runtimes))
	for name := range info.Runtimes {
		names = append(names, name)
	}
	return names, nil
}

func dockerPingViaCLI(timeout time.Duration) error {
	dockerBin, err := resolveDockerBinary()
	if err != nil {
		return err
	}
	_, err = runStatusCommand(timeout, dockerBin, "version", "--format", "{{json .Server}}")
	return err
}

func dockerRuntimeNamesViaCLI(timeout time.Duration) ([]string, error) {
	dockerBin, err := resolveDockerBinary()
	if err != nil {
		return nil, err
	}
	output, err := runStatusCommand(timeout, dockerBin, "info", "--format", "{{json .Runtimes}}")
	if err != nil {
		return nil, err
	}
	var runtimes map[string]any
	if err := json.Unmarshal([]byte(strings.TrimSpace(output)), &runtimes); err != nil {
		return nil, err
	}
	names := make([]string, 0, len(runtimes))
	for name := range runtimes {
		names = append(names, name)
	}
	return names, nil
}

func inspectDeploymentContainersViaDockerAPI(ctx context.Context, projectName string) ([]containerStatusReport, error) {
	filters, err := json.Marshal(map[string][]string{
		"label": {"com.docker.compose.project=" + projectName},
	})
	if err != nil {
		return nil, err
	}
	timeout := 4 * time.Second
	if deadline, ok := ctx.Deadline(); ok {
		timeout = time.Until(deadline)
		if timeout <= 0 {
			timeout = time.Second
		}
	}
	body, err := dockerAPIGet(timeout, "/containers/json?all=1&filters="+url.QueryEscape(string(filters)))
	if err != nil {
		return nil, err
	}
	var apiContainers []struct {
		Names []string `json:"Names"`
		State string   `json:"State"`
	}
	if err := json.Unmarshal(body, &apiContainers); err != nil {
		return nil, err
	}
	containers := make([]containerStatusReport, 0, len(apiContainers))
	for _, container := range apiContainers {
		name := ""
		if len(container.Names) > 0 {
			name = strings.TrimPrefix(container.Names[0], "/")
		}
		containers = append(containers, containerStatusReport{
			Name:         name,
			State:        container.State,
			RestartCount: 0,
		})
	}
	return containers, nil
}

func inspectDeploymentContainersViaDockerCLI(ctx context.Context, dockerBin, projectName string) ([]containerStatusReport, error) {
	if strings.TrimSpace(dockerBin) == "" {
		resolved, err := resolveDockerBinary()
		if err != nil {
			return nil, err
		}
		dockerBin = resolved
	}
	args := []string{"ps", "-a", "--filter", "label=com.docker.compose.project=" + projectName, "--format", "{{json .}}"}
	cmd := exec.CommandContext(ctx, dockerBin, args...)
	cmd.Env = statusCommandEnv()
	output, err := cmd.CombinedOutput()
	if ctx.Err() == context.DeadlineExceeded {
		return nil, ctx.Err()
	}
	if err != nil {
		return nil, fmt.Errorf("docker ps failed: %w: %s", err, shortStatusDetail(string(output)))
	}
	containers := []containerStatusReport{}
	for _, line := range strings.Split(strings.TrimSpace(string(output)), "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		var item struct {
			Names  string `json:"Names"`
			State  string `json:"State"`
			Status string `json:"Status"`
		}
		if err := json.Unmarshal([]byte(line), &item); err != nil {
			return nil, err
		}
		state := strings.ToLower(strings.TrimSpace(item.State))
		if state == "" {
			state = stateFromDockerCLIStatus(item.Status)
		}
		containers = append(containers, containerStatusReport{
			Name:         strings.TrimSpace(item.Names),
			State:        state,
			RestartCount: 0,
		})
	}
	return containers, nil
}

func stateFromDockerCLIStatus(status string) string {
	status = strings.ToLower(strings.TrimSpace(status))
	switch {
	case strings.HasPrefix(status, "up"):
		return "running"
	case strings.Contains(status, "restarting"):
		return "restarting"
	case strings.Contains(status, "paused"):
		return "paused"
	case strings.Contains(status, "exited"):
		return "exited"
	case strings.Contains(status, "created"):
		return "created"
	default:
		return status
	}
}

func readCPUSnapshot() map[string]any {
	sample, err := readCPUSample()
	if err != nil {
		return nil
	}
	previous := previousCPUSample
	previousCPUSample = sample
	if previous == nil || sample.total <= previous.total || sample.idle < previous.idle {
		return nil
	}
	totalDelta := sample.total - previous.total
	idleDelta := sample.idle - previous.idle
	if totalDelta == 0 || idleDelta > totalDelta {
		return nil
	}
	utilisation := 100 * float64(totalDelta-idleDelta) / float64(totalDelta)
	return map[string]any{
		"utilisationPercent": roundOneDecimal(utilisation),
	}
}

func readCPUSample() (*cpuSample, error) {
	data, err := os.ReadFile("/proc/stat")
	if err != nil {
		return nil, err
	}
	for _, line := range strings.Split(string(data), "\n") {
		fields := strings.Fields(line)
		if len(fields) < 5 || fields[0] != "cpu" {
			continue
		}
		var total uint64
		for index := 1; index < len(fields); index += 1 {
			value, err := strconv.ParseUint(fields[index], 10, 64)
			if err != nil {
				return nil, err
			}
			total += value
		}
		idle, err := strconv.ParseUint(fields[4], 10, 64)
		if err != nil {
			return nil, err
		}
		if len(fields) > 5 {
			iowait, err := strconv.ParseUint(fields[5], 10, 64)
			if err == nil {
				idle += iowait
			}
		}
		return &cpuSample{total: total, idle: idle}, nil
	}
	return nil, fmt.Errorf("aggregate cpu line not found")
}

func roundOneDecimal(value float64) float64 {
	return float64(int(value*10+0.5)) / 10
}

func readMemorySnapshot() map[string]any {
	data, err := os.ReadFile("/proc/meminfo")
	if err != nil {
		return nil
	}
	values := map[string]uint64{}
	for _, line := range strings.Split(string(data), "\n") {
		fields := strings.Fields(line)
		if len(fields) < 2 {
			continue
		}
		key := strings.TrimSuffix(fields[0], ":")
		value, err := strconv.ParseUint(fields[1], 10, 64)
		if err != nil {
			continue
		}
		values[key] = value * 1024
	}
	total := values["MemTotal"]
	available := values["MemAvailable"]
	if total == 0 || available > total {
		return nil
	}
	return map[string]any{
		"usedBytes":      total - available,
		"availableBytes": available,
	}
}

func readDiskSnapshot(path string) map[string]any {
	var stats syscall.Statfs_t
	if err := syscall.Statfs(path, &stats); err != nil {
		if err := syscall.Statfs("/", &stats); err != nil {
			return nil
		}
	}
	total := stats.Blocks * uint64(stats.Bsize)
	available := stats.Bavail * uint64(stats.Bsize)
	if total == 0 || available > total {
		return nil
	}
	return map[string]any{
		"usedBytes":      total - available,
		"availableBytes": available,
	}
}

func extractAppNameFromCompose(composeContent string) string {
	return extractMetadataFromCompose(composeContent, "# nethera-app-name:", "default")
}

func extractActionFromCompose(composeContent string) string {
	return extractMetadataFromCompose(composeContent, "# nethera-action:", "deploy")
}

func extractDestroyVolumesFromCompose(composeContent string) bool {
	value := strings.ToLower(extractMetadataFromCompose(composeContent, "# nethera-destroy-volumes:", "false"))
	return value == "true" || value == "yes" || value == "1"
}

func extractMetadataFromCompose(composeContent, prefix, fallback string) string {
	lines := strings.Split(composeContent, "\n")
	for _, line := range lines {
		trimmed := strings.TrimSpace(line)
		if !strings.HasPrefix(trimmed, "#") {
			break
		}
		if strings.HasPrefix(strings.ToLower(trimmed), prefix) {
			value := strings.TrimSpace(trimmed[len(prefix):])
			if value != "" {
				return value
			}
			break
		}
	}
	return fallback
}
