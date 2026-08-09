package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"testing"
	"time"

	"gopkg.in/yaml.v3"
	"nhooyr.io/websocket"
)

func TestWriteComposeFile(t *testing.T) {
	tmpPath, err := writeComposeFile("services:\n  web:\n    image: nginx\n", "job-1")
	if err != nil {
		t.Fatalf("write compose file: %v", err)
	}
	defer os.RemoveAll(filepath.Dir(tmpPath))

	data, err := os.ReadFile(tmpPath)
	if err != nil {
		t.Fatalf("read compose file: %v", err)
	}
	if string(data) != "services:\n  web:\n    image: nginx\n" {
		t.Fatalf("unexpected compose contents: %s", string(data))
	}
}

func TestInspectDeploymentContainersUsesDockerAPIBeforeCLIFallback(t *testing.T) {
	socketPath := filepath.Join(t.TempDir(), "docker.sock")
	listener, err := net.Listen("unix", socketPath)
	if err != nil {
		t.Fatalf("listen unix socket: %v", err)
	}
	defer listener.Close()

	server := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/containers/json" {
			t.Fatalf("unexpected docker API path: %s", r.URL.Path)
		}
		if !strings.Contains(r.URL.RawQuery, "com.docker.compose.project%3Dnethera_api") {
			t.Fatalf("expected compose project filter, query=%s", r.URL.RawQuery)
		}
		_, _ = w.Write([]byte(`[{"Names":["/nethera_api-web-1"],"State":"running"}]`))
	})}
	done := make(chan struct{})
	go func() {
		defer close(done)
		_ = server.Serve(listener)
	}()
	defer func() {
		_ = server.Close()
		<-done
	}()

	failingDocker := filepath.Join(t.TempDir(), "docker")
	if err := os.WriteFile(failingDocker, []byte("#!/bin/sh\necho docker cli should not run >&2\nexit 99\n"), 0o755); err != nil {
		t.Fatalf("write fake docker: %v", err)
	}
	t.Setenv("DOCKER_HOST", "unix://"+socketPath)

	containers, err := inspectDeploymentContainersWithContext(context.Background(), failingDocker, "nethera_api")
	if err != nil {
		t.Fatalf("inspect containers: %v", err)
	}
	if len(containers) != 1 || containers[0].Name != "nethera_api-web-1" || containers[0].State != "running" {
		t.Fatalf("unexpected containers: %#v", containers)
	}
}

func TestInspectDeploymentContainersFallsBackToDockerCLI(t *testing.T) {
	fakeDocker := filepath.Join(t.TempDir(), "docker")
	script := "#!/bin/sh\n" +
		"if [ \"$1\" = ps ]; then\n" +
		"  printf '%s\\n' '{\"Names\":\"nethera_api-web-1\",\"State\":\"running\",\"Status\":\"Up 1 minute\"}'\n" +
		"  exit 0\n" +
		"fi\n" +
		"exit 99\n"
	if err := os.WriteFile(fakeDocker, []byte(script), 0o755); err != nil {
		t.Fatalf("write fake docker: %v", err)
	}
	t.Setenv("DOCKER_HOST", "unix://"+filepath.Join(t.TempDir(), "missing.sock"))
	t.Setenv("NETHERA_AGENT_STATE_DIR", t.TempDir())

	containers, err := inspectDeploymentContainersWithContext(context.Background(), fakeDocker, "nethera_api")
	if err != nil {
		t.Fatalf("inspect containers: %v", err)
	}
	if len(containers) != 1 || containers[0].Name != "nethera_api-web-1" || containers[0].State != "running" {
		t.Fatalf("unexpected containers: %#v", containers)
	}
}

func TestInstallerEnvironmentPaths(t *testing.T) {
	configDir := t.TempDir()
	stateDir := t.TempDir()
	t.Setenv("NETHERA_AGENT_CONFIG_DIR", configDir)
	t.Setenv("NETHERA_AGENT_STATE_DIR", stateDir)

	if got := defaultMachineConfigPath(); got != filepath.Join(configDir, "machine.json") {
		t.Fatalf("defaultMachineConfigPath() = %q", got)
	}
	if got := netheraStateDir(); got != stateDir {
		t.Fatalf("netheraStateDir() = %q", got)
	}
}

func TestAgentEnvironmentForBackendInfersProd(t *testing.T) {
	t.Setenv("NETHERA_ENV", "")

	if got := agentEnvironmentForBackend("https://api.nethera.io"); got != "prod" {
		t.Fatalf("agentEnvironmentForBackend(prod) = %q, want prod", got)
	}
}

func TestAgentEnvironmentForBackendHonorsExplicitEnv(t *testing.T) {
	t.Setenv("NETHERA_ENV", "test")

	if got := agentEnvironmentForBackend("https://api.nethera.io"); got != "test" {
		t.Fatalf("agentEnvironmentForBackend() = %q, want explicit test", got)
	}
}

func TestComposeProjectForContentHonorsTopLevelName(t *testing.T) {
	projectName, useProjectFlag := composeProjectForContent("name: home-assistant\nservices:\n  web:\n    image: nginx\n", "ignored-app")
	if projectName != "home-assistant" {
		t.Fatalf("projectName = %q, want home-assistant", projectName)
	}
	if useProjectFlag {
		t.Fatalf("expected top-level name to suppress docker compose -p")
	}
	args := composeCommandArgs(projectName, useProjectFlag, "/tmp/docker-compose.generated.yml", "up", "-d")
	if got, want := strings.Join(args, " "), "compose -f /tmp/docker-compose.generated.yml up -d"; got != want {
		t.Fatalf("compose args = %q, want %q", got, want)
	}
}

func TestComposeProjectForContentFallsBackToAppName(t *testing.T) {
	projectName, useProjectFlag := composeProjectForContent("services:\n  web:\n    image: nginx\n", "my app")
	if projectName != "nethera_my-app" {
		t.Fatalf("projectName = %q, want nethera_my-app", projectName)
	}
	if !useProjectFlag {
		t.Fatalf("expected fallback project name to use docker compose -p")
	}
}

func TestAgentUpdateVersionComparison(t *testing.T) {
	if compareAgentVersions("0.1.8", "0.1.4") <= 0 {
		t.Fatalf("expected 0.1.8 > 0.1.4")
	}
	if compareAgentVersions("0.10.0", "0.9.9") <= 0 {
		t.Fatalf("expected semantic comparison, not string comparison")
	}
	if compareAgentVersions("0.1.0", "0.1.0") != 0 {
		t.Fatalf("expected equal versions")
	}
}

func TestFirstDeployJobPreservesReservedHostPorts(t *testing.T) {
	job := firstDeployJob([]deployJobPayload{{
		ID:                "job-1",
		DeploymentID:      "dep-1",
		AppID:             "app-1",
		ComposeYAML:       "services: {}",
		ReservedHostPorts: []int{20000, 20001},
	}})

	if job == nil {
		t.Fatal("expected deploy job")
	}
	if len(job.ReservedHostPorts) != 2 || job.ReservedHostPorts[0] != 20000 || job.ReservedHostPorts[1] != 20001 {
		t.Fatalf("reserved host ports were not preserved: %#v", job.ReservedHostPorts)
	}
}

func TestShouldAttemptAgentUpdateRejectsDowngrade(t *testing.T) {
	t.Setenv("NETHERA_AGENT_VERSION", "0.2.0")
	t.Setenv("NETHERA_AGENT_STATE_DIR", t.TempDir())
	attempt, reason := shouldAttemptAgentUpdate(agentUpdatePayload{Available: true, Version: "0.1.9"})
	if attempt {
		t.Fatalf("expected downgrade to be rejected")
	}
	if !strings.Contains(reason, "ignoring agent update") {
		t.Fatalf("unexpected reason: %s", reason)
	}
}

func TestInstallAgentUpdateKeepsPreviousBinary(t *testing.T) {
	binDir := t.TempDir()
	binaryPath := filepath.Join(binDir, "nethera-agent")
	if err := os.WriteFile(binaryPath, []byte("old"), 0o755); err != nil {
		t.Fatalf("write old binary: %v", err)
	}
	tempPath := filepath.Join(binDir, ".nethera-agent-update-test")
	if err := os.WriteFile(tempPath, []byte("new"), 0o755); err != nil {
		t.Fatalf("write new binary: %v", err)
	}
	t.Setenv("NETHERA_AGENT_BINARY_PATH", binaryPath)

	if err := installAgentUpdate(tempPath); err != nil {
		t.Fatalf("install update: %v", err)
	}
	newData, err := os.ReadFile(binaryPath)
	if err != nil {
		t.Fatalf("read new binary: %v", err)
	}
	if string(newData) != "new" {
		t.Fatalf("binary was not updated: %q", newData)
	}
	previousData, err := os.ReadFile(binaryPath + ".previous")
	if err != nil {
		t.Fatalf("read previous binary: %v", err)
	}
	if string(previousData) != "old" {
		t.Fatalf("previous binary was not preserved: %q", previousData)
	}
}

func TestVerifyAgentUpdateChecksum(t *testing.T) {
	path := filepath.Join(t.TempDir(), "nethera-agent")
	content := []byte("new-agent")
	if err := os.WriteFile(path, content, 0o755); err != nil {
		t.Fatalf("write binary: %v", err)
	}
	sum := sha256.Sum256(content)
	if err := verifyAgentUpdateChecksum(path, hex.EncodeToString(sum[:])); err != nil {
		t.Fatalf("verify checksum: %v", err)
	}
	if err := verifyAgentUpdateChecksum(path, strings.Repeat("0", 64)); err == nil {
		t.Fatalf("expected checksum mismatch")
	}
}

func TestAgentUpdateFailureBackoff(t *testing.T) {
	t.Setenv("NETHERA_AGENT_STATE_DIR", t.TempDir())
	if err := recordAgentUpdateFailure(os.ErrPermission); err == nil {
		t.Fatalf("expected recordAgentUpdateFailure to return original error")
	}
	state, err := loadAgentUpdateState()
	if err != nil {
		t.Fatalf("load update state: %v", err)
	}
	if state.LastError == "" || state.NextAttemptAfter == "" || state.ConsecutiveFailures != 1 {
		t.Fatalf("unexpected update state: %#v", state)
	}
}

func TestGenerateComposeFileAddsResilienceTransforms(t *testing.T) {
	generated, allocatedPorts, _, _, err := generateComposeFile(`services:
  web:
    image: nginx
    ports:
      - "8080:3000"
      - "127.255.0.1:32781:3000"
`, "myapp", "dep_123", "app_123", "10.100.0.2", map[string]int{"web:3000": 32781}, "/var/lib/nethera/deployments/dep_123/.env", nil, nil)
	if err != nil {
		t.Fatalf("generate compose: %v", err)
	}
	if allocatedPorts["web:3000"] != 32781 {
		t.Fatalf("allocated port not preserved: %#v", allocatedPorts)
	}
	for _, want := range []string{
		"restart: unless-stopped",
		`- "8080:3000"`,
		`- "10.100.0.2:32781:3000"`,
		"nethera.managed: \"true\"",
		"nethera.deployment_id: dep_123",
		"nethera.application_id: app_123",
	} {
		if !strings.Contains(generated, want) {
			t.Fatalf("generated compose missing %q:\n%s", want, generated)
		}
	}
}

func TestGenerateComposeFilePreservesOneShotSetupServices(t *testing.T) {
	generated, _, expectedServices, _, err := generateComposeFile(`services:
  init:
    image: busybox:1.36
    command: sh -c "chown -R 1024:1024 /data"
    volumes:
      - data:/data
  web:
    image: nginx
    depends_on:
      init:
        condition: service_completed_successfully
    volumes:
      - data:/usr/share/nginx/html
volumes:
  data:
`, "myapp", "dep_123", "app_123", "10.100.0.2", nil, "/var/lib/nethera/deployments/dep_123/.env", nil, nil)
	if err != nil {
		t.Fatalf("generate compose: %v", err)
	}
	if strings.Contains(generated, "init:\n    image: busybox:1.36\n    command:") && strings.Contains(generated, "init:\n    image: busybox:1.36\n    restart: unless-stopped") {
		t.Fatalf("one-shot init service should not receive a restart policy:\n%s", generated)
	}
	if !strings.Contains(generated, "web:\n    image: nginx\n    depends_on:") || !strings.Contains(generated, "restart: unless-stopped") {
		t.Fatalf("long-running web service should still receive a restart policy:\n%s", generated)
	}
	if len(expectedServices) != 1 || expectedServices[0] != "web" {
		t.Fatalf("expected only web as a long-running service, got %#v", expectedServices)
	}
}

func TestOneShotContainersDoNotDegradeDeploymentStatus(t *testing.T) {
	status := summarizeContainerStatusForCompose([]containerStatusReport{
		{Name: "nethera_comfyui-init-1", State: "exited"},
		{Name: "nethera_comfyui-comfyui-1", State: "running"},
	}, []string{"comfyui"}, map[string]bool{"init": true})
	if status != "running" {
		t.Fatalf("expected completed one-shot service to be ignored, got %q", status)
	}
}

func TestGenerateComposeFileAddsSecretEnvFile(t *testing.T) {
	generated, _, _, _, err := generateComposeFile(`services:
  web:
    image: nginx
    nethera:
      secrets:
        - OPENAI_API_KEY
`, "myapp", "dep_123", "app_123", "10.100.0.2", nil, "/var/lib/nethera/deployments/dep_123/.env", nil, nil)
	if err != nil {
		t.Fatalf("generate compose: %v", err)
	}
	for _, want := range []string{
		"env_file:",
		"- /var/lib/nethera/deployments/dep_123/.env",
	} {
		if !strings.Contains(generated, want) {
			t.Fatalf("generated compose missing %q:\n%s", want, generated)
		}
	}
	if strings.Contains(generated, "nethera:") || strings.Contains(generated, "OPENAI_API_KEY") {
		t.Fatalf("generated compose leaked secret declaration:\n%s", generated)
	}
}

func TestRunComposeDeploymentFetchesAndWritesSecrets(t *testing.T) {
	stateDir := t.TempDir()
	t.Setenv("NETHERA_STATE_DIR", stateDir)

	binDir := t.TempDir()
	dockerLog := filepath.Join(t.TempDir(), "docker.log")
	dockerPath := filepath.Join(binDir, "docker")
	fakeDocker := "#!/bin/sh\n" +
		"printf '%s\\n' \"$*\" >> \"" + dockerLog + "\"\n" +
		"if [ \"$1\" = \"ps\" ]; then printf 'web\\trunning\\n'; fi\n" +
		"exit 0\n"
	if err := os.WriteFile(dockerPath, []byte(fakeDocker), 0o755); err != nil {
		t.Fatalf("write fake docker: %v", err)
	}
	t.Setenv("PATH", binDir+string(os.PathListSeparator)+os.Getenv("PATH"))

	secretRequests := 0
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/agent/deployments/dep_secret/secrets" {
			t.Fatalf("unexpected backend path: %s", r.URL.Path)
		}
		if got := r.Header.Get("Authorization"); got != "Bearer machine-token" {
			t.Fatalf("unexpected auth header: %q", got)
		}
		secretRequests += 1
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"deploymentId":"dep_secret","appId":"app_secret","secrets":{"OPENAI_API_KEY":"sk-test-123"}}`))
	}))
	defer backend.Close()

	job := &deployJob{
		ID:            "job_secret",
		DeploymentID:  "dep_secret",
		ApplicationID: "app_secret",
		ComposeYAML: `# nethera-app-name: secret-app
services:
  web:
    image: alpine
    command: ["sh", "-c", "printf '%s' \"$OPENAI_API_KEY\""]
    nethera:
      secrets:
        - OPENAI_API_KEY
`,
	}
	logs, _, err := runComposeDeployment(job, "10.100.0.2", backend.URL, "machine-token")
	if err != nil {
		t.Fatalf("run compose deployment: %v\nlogs: %s", err, strings.Join(logs, "\n"))
	}
	if secretRequests != 1 {
		t.Fatalf("secret endpoint called %d times, want 1", secretRequests)
	}

	deploymentDir := filepath.Join(stateDir, "deployments", "dep_secret")
	envData, err := os.ReadFile(filepath.Join(deploymentDir, ".env"))
	if err != nil {
		t.Fatalf("read generated env file: %v", err)
	}
	if got, want := string(envData), "OPENAI_API_KEY=sk-test-123\n"; got != want {
		t.Fatalf("unexpected env file contents:\n%s", got)
	}
	envInfo, err := os.Stat(filepath.Join(deploymentDir, ".env"))
	if err != nil {
		t.Fatalf("stat generated env file: %v", err)
	}
	if envInfo.Mode().Perm() != 0o600 {
		t.Fatalf("env file permissions = %v, want 0600", envInfo.Mode().Perm())
	}

	composeData, err := os.ReadFile(filepath.Join(deploymentDir, "docker-compose.generated.yml"))
	if err != nil {
		t.Fatalf("read generated compose: %v", err)
	}
	generatedCompose := string(composeData)
	if !strings.Contains(generatedCompose, "env_file:") || !strings.Contains(generatedCompose, filepath.Join(deploymentDir, ".env")) {
		t.Fatalf("generated compose missing env_file reference:\n%s", generatedCompose)
	}
	if strings.Contains(generatedCompose, "nethera:") || strings.Contains(generatedCompose, "secrets:") || strings.Contains(generatedCompose, "sk-test-123") {
		t.Fatalf("generated compose leaked secret declaration or value:\n%s", generatedCompose)
	}

	dockerInvocations, err := os.ReadFile(dockerLog)
	if err != nil {
		t.Fatalf("read fake docker log: %v", err)
	}
	if !strings.Contains(string(dockerInvocations), "compose -p nethera_secret-app") {
		t.Fatalf("docker compose was not invoked for expected project:\n%s", dockerInvocations)
	}
	if strings.Contains(string(dockerInvocations), "sk-test-123") {
		t.Fatalf("docker command leaked secret value:\n%s", dockerInvocations)
	}
}

func TestRunComposeDeploymentDoesNotCommitMetadataOnDockerFailure(t *testing.T) {
	stateDir := t.TempDir()
	t.Setenv("NETHERA_STATE_DIR", stateDir)

	binDir := t.TempDir()
	dockerPath := filepath.Join(binDir, "docker")
	fakeDocker := "#!/bin/sh\n" +
		"if [ \"$1\" = \"ps\" ]; then exit 0; fi\n" +
		"echo 'local error: tls: bad record MAC' >&2\n" +
		"exit 1\n"
	if err := os.WriteFile(dockerPath, []byte(fakeDocker), 0o755); err != nil {
		t.Fatalf("write fake docker: %v", err)
	}
	t.Setenv("PATH", binDir+string(os.PathListSeparator)+os.Getenv("PATH"))

	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/agent/deployments/dep_failed/secrets" {
			t.Fatalf("unexpected backend path: %s", r.URL.Path)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"deploymentId":"dep_failed","appId":"app_failed","runtimeSecrets":{},"generatedEnv":{},"imagePullCredentials":[]}`))
	}))
	defer backend.Close()

	job := &deployJob{
		ID:            "job_failed",
		DeploymentID:  "dep_failed",
		ApplicationID: "app_failed",
		ComposeYAML: `# nethera-app-name: failed-app
services:
  web:
    image: nginx
    nethera:
      public: 80
`,
	}
	logs, _, err := runComposeDeployment(job, "10.100.0.2", backend.URL, "machine-token")
	if err == nil {
		t.Fatalf("expected docker failure, got nil error")
	}
	joined := strings.Join(logs, "\n")
	if !strings.Contains(joined, "local error: tls: bad record MAC") {
		t.Fatalf("expected docker error in logs:\n%s", joined)
	}
	deploymentDir := filepath.Join(stateDir, "deployments", "dep_failed")
	if _, statErr := os.Stat(filepath.Join(deploymentDir, "deployment.json")); !os.IsNotExist(statErr) {
		t.Fatalf("failed deploy should not commit deployment metadata, stat err=%v", statErr)
	}
	if localDeploymentsNeedReconcile() {
		t.Fatalf("failed deploy should not be treated as desired self-heal state")
	}
}

func TestFailedUpdateDisablesSelfHealForPreviousSuccessfulDeployment(t *testing.T) {
	stateDir := t.TempDir()
	t.Setenv("NETHERA_STATE_DIR", stateDir)

	binDir := t.TempDir()
	dockerLog := filepath.Join(t.TempDir(), "docker.log")
	failMarker := filepath.Join(t.TempDir(), "fail")
	dockerPath := filepath.Join(binDir, "docker")
	fakeDocker := "#!/bin/sh\n" +
		"echo \"$@\" >> \"" + dockerLog + "\"\n" +
		"if [ -f \"" + failMarker + "\" ]; then\n" +
		"  echo 'compose update failed' >&2\n" +
		"  exit 1\n" +
		"fi\n" +
		"exit 0\n"
	if err := os.WriteFile(dockerPath, []byte(fakeDocker), 0o755); err != nil {
		t.Fatalf("write fake docker: %v", err)
	}
	t.Setenv("PATH", binDir+string(os.PathListSeparator)+os.Getenv("PATH"))

	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/agent/deployments/dep_update/secrets" {
			t.Fatalf("unexpected backend path: %s", r.URL.Path)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"deploymentId":"dep_update","appId":"app_update","runtimeSecrets":{},"generatedEnv":{},"imagePullCredentials":[]}`))
	}))
	defer backend.Close()

	job := &deployJob{
		ID:            "job_update",
		DeploymentID:  "dep_update",
		ApplicationID: "app_update",
		ComposeYAML: `# nethera-app-name: update-app
services:
  web:
    image: nginx:1
    nethera:
      public: 80
`,
	}
	if logs, _, err := runComposeDeployment(job, "10.100.0.2", backend.URL, "machine-token"); err != nil {
		t.Fatalf("initial deploy failed: %v\n%s", err, strings.Join(logs, "\n"))
	}
	metadataPath := filepath.Join(stateDir, "deployments", "dep_update", "deployment.json")
	metadata, err := loadDeploymentMetadata(metadataPath)
	if err != nil {
		t.Fatalf("load initial metadata: %v", err)
	}
	if metadata.SelfHealDisabled {
		t.Fatalf("successful deploy should be eligible for self-heal")
	}

	if err := os.WriteFile(failMarker, []byte("fail"), 0o644); err != nil {
		t.Fatalf("write fail marker: %v", err)
	}
	job.ComposeYAML = `# nethera-app-name: update-app
services:
  web:
    image: nginx:2
    nethera:
      public: 80
`
	if _, _, err := runComposeDeployment(job, "10.100.0.2", backend.URL, "machine-token"); err == nil {
		t.Fatalf("expected update failure")
	}
	metadata, err = loadDeploymentMetadata(metadataPath)
	if err != nil {
		t.Fatalf("load metadata after failed update: %v", err)
	}
	if !metadata.SelfHealDisabled {
		t.Fatalf("failed update should disable self-heal for the previous deployment state")
	}

	beforeReconcile, _ := os.ReadFile(dockerLog)
	reconcileLogs, err := reconcileLocalDeployments("10.100.0.2")
	if err != nil {
		t.Fatalf("reconcile errored: %v", err)
	}
	if !strings.Contains(strings.Join(reconcileLogs, "\n"), "self-heal disabled after failed deployment") {
		t.Fatalf("expected reconcile skip log, got:\n%s", strings.Join(reconcileLogs, "\n"))
	}
	afterReconcile, _ := os.ReadFile(dockerLog)
	if string(afterReconcile) != string(beforeReconcile) {
		t.Fatalf("reconcile should not invoke docker for a deployment disabled by a failed update")
	}

	if err := os.Remove(failMarker); err != nil {
		t.Fatalf("remove fail marker: %v", err)
	}
	if logs, _, err := runComposeDeployment(job, "10.100.0.2", backend.URL, "machine-token"); err != nil {
		t.Fatalf("successful redeploy failed: %v\n%s", err, strings.Join(logs, "\n"))
	}
	metadata, err = loadDeploymentMetadata(metadataPath)
	if err != nil {
		t.Fatalf("load metadata after recovery deploy: %v", err)
	}
	if metadata.SelfHealDisabled {
		t.Fatalf("successful redeploy should re-enable self-heal")
	}
}

func TestRunComposeDeploymentWritesGeneratedEndpointEnv(t *testing.T) {
	stateDir := t.TempDir()
	t.Setenv("NETHERA_STATE_DIR", stateDir)

	binDir := t.TempDir()
	dockerPath := filepath.Join(binDir, "docker")
	fakeDocker := "#!/bin/sh\n" +
		"if [ \"$1\" = \"ps\" ]; then printf 'web\\trunning\\nworker\\trunning\\n'; fi\n" +
		"exit 0\n"
	if err := os.WriteFile(dockerPath, []byte(fakeDocker), 0o755); err != nil {
		t.Fatalf("write fake docker: %v", err)
	}
	t.Setenv("PATH", binDir+string(os.PathListSeparator)+os.Getenv("PATH"))

	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/agent/deployments/dep_url/secrets" {
			t.Fatalf("unexpected backend path: %s", r.URL.Path)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{
			"deploymentId":"dep_url",
			"appId":"app_url",
			"runtimeSecrets":{},
			"generatedEnv":{
				"NETHERA_PUBLIC_HOST":"myapp-web-dep-url.sg.nethera.io",
				"NETHERA_PUBLIC_URL":"https://myapp-web-dep-url.sg.nethera.io",
				"NETHERA_WEB_HOST":"myapp-web-dep-url.sg.nethera.io",
				"NETHERA_WEB_URL":"https://myapp-web-dep-url.sg.nethera.io"
			},
			"imagePullCredentials":[]
		}`))
	}))
	defer backend.Close()

	job := &deployJob{
		ID:            "job_url",
		DeploymentID:  "dep_url",
		ApplicationID: "app_url",
		ComposeYAML: `# nethera-app-name: myapp
services:
  web:
    image: nginx
    nethera:
      public: 80
  worker:
    image: alpine
    command: ["sleep", "3600"]
`,
	}
	logs, _, err := runComposeDeployment(job, "10.100.0.2", backend.URL, "machine-token")
	if err != nil {
		t.Fatalf("run compose deployment: %v\nlogs: %s", err, strings.Join(logs, "\n"))
	}

	deploymentDir := filepath.Join(stateDir, "deployments", "dep_url")
	envData, err := os.ReadFile(filepath.Join(deploymentDir, ".env"))
	if err != nil {
		t.Fatalf("read generated env file: %v", err)
	}
	if got, want := string(envData), "NETHERA_PUBLIC_HOST=myapp-web-dep-url.sg.nethera.io\nNETHERA_PUBLIC_URL=https://myapp-web-dep-url.sg.nethera.io\nNETHERA_WEB_HOST=myapp-web-dep-url.sg.nethera.io\nNETHERA_WEB_URL=https://myapp-web-dep-url.sg.nethera.io\n"; got != want {
		t.Fatalf("unexpected env file contents:\n%s", got)
	}

	composeData, err := os.ReadFile(filepath.Join(deploymentDir, "docker-compose.generated.yml"))
	if err != nil {
		t.Fatalf("read generated compose: %v", err)
	}
	generatedCompose := string(composeData)
	if strings.Count(generatedCompose, filepath.Join(deploymentDir, ".env")) != 2 {
		t.Fatalf("generated env file should be attached to every service:\n%s", generatedCompose)
	}
	if strings.Contains(generatedCompose, "NETHERA_PUBLIC_URL") || strings.Contains(generatedCompose, "NETHERA_WEB_URL") || strings.Contains(generatedCompose, "NETHERA_PUBLIC_HOST") {
		t.Fatalf("generated compose leaked generated env values instead of env_file reference:\n%s", generatedCompose)
	}
}

func TestMergeLANEndpointEnvAddsPreferLANAliases(t *testing.T) {
	env := mergeLANEndpointEnv(map[string]string{
		"NETHERA_PUBLIC_HOST": "myapp-web-dep-url.sg.nethera.io",
	}, []publicEndpointReport{
		{
			ServiceName: "web",
			PreferLAN:   true,
			LANHost:     "192.168.1.50",
			LANPort:     8080,
		},
	})

	for name, want := range map[string]string{
		"NETHERA_PUBLIC_HOST":  "myapp-web-dep-url.sg.nethera.io",
		"NETHERA_WEB_LAN_HOST": "192.168.1.50:8080",
		"NETHERA_WEB_LAN_URL":  "http://192.168.1.50:8080",
		"NETHERA_LAN_HOST":     "192.168.1.50:8080",
		"NETHERA_LAN_URL":      "http://192.168.1.50:8080",
	} {
		if got := env[name]; got != want {
			t.Fatalf("env[%s] = %q, want %q", name, got, want)
		}
	}
}

func TestAppendDeploymentEnvAddsComposeInterpolationVariables(t *testing.T) {
	env := appendDeploymentEnv([]string{"PATH=/usr/bin"}, map[string]string{
		"NETHERA_LAN_HOST":    "192.168.1.50:32783",
		"NETHERA_PUBLIC_HOST": "nextcloud.example.com",
	})
	got := strings.Join(env, "\n")
	for _, want := range []string{
		"PATH=/usr/bin",
		"NETHERA_LAN_HOST=192.168.1.50:32783",
		"NETHERA_PUBLIC_HOST=nextcloud.example.com",
	} {
		if !strings.Contains(got, want) {
			t.Fatalf("appendDeploymentEnv() missing %q in:\n%s", want, got)
		}
	}
}

func TestRunComposeDestroyRemovesLocalDeploymentMetadata(t *testing.T) {
	stateDir := t.TempDir()
	t.Setenv("NETHERA_STATE_DIR", stateDir)

	binDir := t.TempDir()
	dockerLog := filepath.Join(t.TempDir(), "docker.log")
	dockerPath := filepath.Join(binDir, "docker")
	fakeDocker := "#!/bin/sh\n" +
		"printf '%s\\n' \"$*\" >> \"" + dockerLog + "\"\n" +
		"if [ \"$1\" = \"ps\" ]; then printf 'web\\trunning\\n'; fi\n" +
		"exit 0\n"
	if err := os.WriteFile(dockerPath, []byte(fakeDocker), 0o755); err != nil {
		t.Fatalf("write fake docker: %v", err)
	}
	t.Setenv("PATH", binDir+string(os.PathListSeparator)+os.Getenv("PATH"))

	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/agent/deployments/dep_destroy/secrets" {
			t.Fatalf("unexpected backend path: %s", r.URL.Path)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"deploymentId":"dep_destroy","appId":"app_destroy","runtimeSecrets":{},"generatedEnv":{},"imagePullCredentials":[]}`))
	}))
	defer backend.Close()

	deploy := &deployJob{
		ID:            "job_deploy",
		DeploymentID:  "dep_destroy",
		ApplicationID: "app_destroy",
		ComposeYAML: `# nethera-app-name: destroy-app
services:
  web:
    image: nginx
    nethera:
      public: 80
`,
	}
	logs, _, err := runComposeDeployment(deploy, "10.100.0.2", backend.URL, "machine-token")
	if err != nil {
		t.Fatalf("run compose deploy: %v\nlogs: %s", err, strings.Join(logs, "\n"))
	}
	deploymentDir := filepath.Join(stateDir, "deployments", "dep_destroy")
	if _, err := os.Stat(filepath.Join(deploymentDir, "deployment.json")); err != nil {
		t.Fatalf("expected local deployment metadata after deploy: %v", err)
	}

	destroyJob := &deployJob{
		ID:            "job_destroy",
		DeploymentID:  "dep_destroy",
		ApplicationID: "app_destroy",
		ComposeYAML: `# nethera-app-name: destroy-app
# nethera-action: destroy
services:
  web:
    image: nginx
    nethera:
      public: 80
`,
	}
	logs, _, err = runComposeDeployment(destroyJob, "10.100.0.2", backend.URL, "machine-token")
	if err != nil {
		t.Fatalf("run compose destroy: %v\nlogs: %s", err, strings.Join(logs, "\n"))
	}
	if _, err := os.Stat(deploymentDir); !os.IsNotExist(err) {
		t.Fatalf("deployment metadata dir still exists after destroy: %v", err)
	}

	dockerInvocations, err := os.ReadFile(dockerLog)
	if err != nil {
		t.Fatalf("read fake docker log: %v", err)
	}
	if !strings.Contains(string(dockerInvocations), " down --remove-orphans") {
		t.Fatalf("compose down was not invoked:\n%s", dockerInvocations)
	}
	if strings.Contains(string(dockerInvocations), "--volumes") {
		t.Fatalf("compose down unexpectedly removed volumes:\n%s", dockerInvocations)
	}
}

func TestRunComposeDestroyCanRemoveVolumes(t *testing.T) {
	stateDir := t.TempDir()
	t.Setenv("NETHERA_STATE_DIR", stateDir)

	binDir := t.TempDir()
	dockerLog := filepath.Join(t.TempDir(), "docker.log")
	dockerPath := filepath.Join(binDir, "docker")
	fakeDocker := "#!/bin/sh\n" +
		"printf '%s\\n' \"$*\" >> \"" + dockerLog + "\"\n" +
		"exit 0\n"
	if err := os.WriteFile(dockerPath, []byte(fakeDocker), 0o755); err != nil {
		t.Fatalf("write fake docker: %v", err)
	}
	t.Setenv("PATH", binDir+string(os.PathListSeparator)+os.Getenv("PATH"))

	destroyJob := &deployJob{
		ID:            "job_destroy_volumes",
		DeploymentID:  "dep_destroy_volumes",
		ApplicationID: "app_destroy_volumes",
		ComposeYAML: `# nethera-app-name: destroy-app
# nethera-action: destroy
# nethera-destroy-volumes: true
services:
  web:
    image: nginx
    volumes:
      - data:/data
volumes:
  data:
`,
	}
	logs, _, err := runComposeDeployment(destroyJob, "10.100.0.2", "http://127.0.0.1:1", "machine-token")
	if err != nil {
		t.Fatalf("run compose destroy with volumes: %v\nlogs: %s", err, strings.Join(logs, "\n"))
	}

	dockerInvocations, err := os.ReadFile(dockerLog)
	if err != nil {
		t.Fatalf("read fake docker log: %v", err)
	}
	if !strings.Contains(string(dockerInvocations), " down --remove-orphans --volumes") {
		t.Fatalf("compose down did not include --volumes:\n%s", dockerInvocations)
	}
}

func TestExtractPostDeployCommands(t *testing.T) {
	commands, err := extractPostDeployCommands(`services:
  web:
    image: nginx
    nethera:
      postDeploy:
        - echo ready
        - |
          until test -f /tmp/ready; do sleep 1; done
          echo done
`)
	if err != nil {
		t.Fatalf("extract postDeploy: %v", err)
	}
	if len(commands) != 2 {
		t.Fatalf("commands len = %d, want 2", len(commands))
	}
	if commands[0].ServiceName != "web" || commands[0].Command != "echo ready" {
		t.Fatalf("unexpected first command: %#v", commands[0])
	}
	if !strings.Contains(commands[1].Command, "until test -f /tmp/ready") {
		t.Fatalf("block command was not preserved: %#v", commands[1])
	}
}

func TestExtractPostDeployCommandsRejectsLegacySetup(t *testing.T) {
	_, err := extractPostDeployCommands(`services:
  web:
    image: nginx
    nethera:
      setup:
        - echo old
`)
	if err == nil || !strings.Contains(err.Error(), "renamed to nethera.postDeploy") {
		t.Fatalf("error = %v, want setup rename error", err)
	}
}

func TestRunComposeDeploymentRunsPostDeployCommands(t *testing.T) {
	stateDir := t.TempDir()
	t.Setenv("NETHERA_STATE_DIR", stateDir)

	binDir := t.TempDir()
	dockerLog := filepath.Join(t.TempDir(), "docker.log")
	dockerPath := filepath.Join(binDir, "docker")
	fakeDocker := "#!/bin/sh\n" +
		"printf '%s\\n' \"$*\" >> \"" + dockerLog + "\"\n" +
		"case \" $* \" in\n" +
		"  *\" ps \"*) printf 'web\\trunning\\n' ;;\n" +
		"  *\" echo post-ready\"*) printf 'post deploy ran\\n' ;;\n" +
		"esac\n" +
		"exit 0\n"
	if err := os.WriteFile(dockerPath, []byte(fakeDocker), 0o755); err != nil {
		t.Fatalf("write fake docker: %v", err)
	}
	t.Setenv("PATH", binDir+string(os.PathListSeparator)+os.Getenv("PATH"))

	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/agent/deployments/dep_post/secrets" {
			t.Fatalf("unexpected backend path: %s", r.URL.Path)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"deploymentId":"dep_post","appId":"app_post","runtimeSecrets":{},"generatedEnv":{},"imagePullCredentials":[]}`))
	}))
	defer backend.Close()

	job := &deployJob{
		ID:            "job_post",
		DeploymentID:  "dep_post",
		ApplicationID: "app_post",
		ComposeYAML: `# nethera-app-name: post-app
services:
  web:
    image: alpine
    nethera:
      postDeploy:
        - echo post-ready
`,
	}
	logs, _, err := runComposeDeployment(job, "10.100.0.2", backend.URL, "machine-token")
	if err != nil {
		t.Fatalf("run compose deployment: %v\nlogs: %s", err, strings.Join(logs, "\n"))
	}
	logText := strings.Join(logs, "\n")
	if !strings.Contains(logText, "postDeploy web: echo post-ready") || !strings.Contains(logText, "postDeploy output: post deploy ran") {
		t.Fatalf("postDeploy logs missing command/output:\n%s", logText)
	}
	dockerInvocations, err := os.ReadFile(dockerLog)
	if err != nil {
		t.Fatalf("read fake docker log: %v", err)
	}
	invocationText := string(dockerInvocations)
	if !strings.Contains(invocationText, " up -d --remove-orphans") {
		t.Fatalf("compose up was not invoked:\n%s", invocationText)
	}
	if !strings.Contains(invocationText, " exec -T web sh -lc true") {
		t.Fatalf("postDeploy readiness exec was not invoked:\n%s", invocationText)
	}
	if !strings.Contains(invocationText, " exec -T web sh -lc echo post-ready") {
		t.Fatalf("postDeploy command exec was not invoked:\n%s", invocationText)
	}
}

func TestMergeDeploymentEnvRejectsGeneratedSecretCollision(t *testing.T) {
	_, err := mergeDeploymentEnv(
		map[string]string{"NETHERA_PUBLIC_URL": "secret-value"},
		map[string]string{"NETHERA_PUBLIC_URL": "https://example.nethera.io"},
	)
	if err == nil || !strings.Contains(err.Error(), "conflicts") {
		t.Fatalf("error = %v, want generated env collision", err)
	}
}

func TestRunComposeDeploymentUsesImagePullCredentialsWithoutInjectingThem(t *testing.T) {
	stateDir := t.TempDir()
	t.Setenv("NETHERA_STATE_DIR", stateDir)

	binDir := t.TempDir()
	dockerLog := filepath.Join(t.TempDir(), "docker.log")
	stdinLog := filepath.Join(t.TempDir(), "stdin.log")
	dockerPath := filepath.Join(binDir, "docker")
	fakeDocker := "#!/bin/sh\n" +
		"printf '%s DOCKER_CONFIG=%s\\n' \"$*\" \"$DOCKER_CONFIG\" >> \"" + dockerLog + "\"\n" +
		"if [ \"$1\" = \"login\" ]; then cat > \"" + stdinLog + "\"; fi\n" +
		"if [ \"$1\" = \"ps\" ]; then printf 'web\\trunning\\n'; fi\n" +
		"exit 0\n"
	if err := os.WriteFile(dockerPath, []byte(fakeDocker), 0o755); err != nil {
		t.Fatalf("write fake docker: %v", err)
	}
	t.Setenv("PATH", binDir+string(os.PathListSeparator)+os.Getenv("PATH"))

	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/agent/deployments/dep_private/secrets" {
			t.Fatalf("unexpected backend path: %s", r.URL.Path)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{
			"deploymentId":"dep_private",
			"appId":"app_private",
			"runtimeSecrets":{"OPENAI_API_KEY":"sk-test-123"},
			"imagePullCredentials":[{"registry":"ghcr.io","username":"tim","password":"ghp_secret_token"}]
		}`))
	}))
	defer backend.Close()

	job := &deployJob{
		ID:            "job_private",
		DeploymentID:  "dep_private",
		ApplicationID: "app_private",
		ComposeYAML: `# nethera-app-name: private-app
services:
  web:
    image: ghcr.io/tim/private-app:latest
    nethera:
      imagePullCredentials:
        - registry: ghcr.io
          usernameSecret: GHCR_USERNAME
          passwordSecret: GHCR_TOKEN
      secrets:
        - OPENAI_API_KEY
`,
	}
	logs, _, err := runComposeDeployment(job, "10.100.0.2", backend.URL, "machine-token")
	if err != nil {
		t.Fatalf("run compose deployment: %v\nlogs: %s", err, strings.Join(logs, "\n"))
	}

	deploymentDir := filepath.Join(stateDir, "deployments", "dep_private")
	envData, err := os.ReadFile(filepath.Join(deploymentDir, ".env"))
	if err != nil {
		t.Fatalf("read generated env file: %v", err)
	}
	if got, want := string(envData), "OPENAI_API_KEY=sk-test-123\n"; got != want {
		t.Fatalf("unexpected env file contents:\n%s", got)
	}
	dockerConfigInfo, err := os.Stat(filepath.Join(deploymentDir, "docker-config"))
	if err != nil {
		t.Fatalf("stat docker config dir: %v", err)
	}
	if dockerConfigInfo.Mode().Perm() != 0o700 {
		t.Fatalf("docker config permissions = %v, want 0700", dockerConfigInfo.Mode().Perm())
	}

	composeData, err := os.ReadFile(filepath.Join(deploymentDir, "docker-compose.generated.yml"))
	if err != nil {
		t.Fatalf("read generated compose: %v", err)
	}
	generatedCompose := string(composeData)
	if strings.Contains(generatedCompose, "imagePullCredentials") || strings.Contains(generatedCompose, "GHCR_TOKEN") || strings.Contains(generatedCompose, "ghp_secret_token") {
		t.Fatalf("generated compose leaked image pull credential metadata or value:\n%s", generatedCompose)
	}
	if strings.Contains(string(envData), "GHCR_USERNAME") || strings.Contains(string(envData), "GHCR_TOKEN") || strings.Contains(string(envData), "ghp_secret_token") {
		t.Fatalf("runtime env leaked image pull credentials:\n%s", envData)
	}

	dockerInvocations, err := os.ReadFile(dockerLog)
	if err != nil {
		t.Fatalf("read fake docker log: %v", err)
	}
	dockerLogText := string(dockerInvocations)
	if !strings.Contains(dockerLogText, "login ghcr.io -u tim --password-stdin") {
		t.Fatalf("docker login was not invoked safely:\n%s", dockerLogText)
	}
	if !strings.Contains(dockerLogText, "compose -p nethera_private-app") || !strings.Contains(dockerLogText, " pull ") {
		t.Fatalf("docker compose pull was not invoked:\n%s", dockerLogText)
	}
	if strings.Contains(dockerLogText, "ghp_secret_token") {
		t.Fatalf("docker command log leaked registry token:\n%s", dockerLogText)
	}
	stdinData, err := os.ReadFile(stdinLog)
	if err != nil {
		t.Fatalf("read login stdin: %v", err)
	}
	if string(stdinData) != "ghp_secret_token" {
		t.Fatalf("docker login did not receive password on stdin")
	}
}

func TestExtractRequiredSecretNamesRejectsLocalProjectReferences(t *testing.T) {
	cases := []struct {
		name    string
		compose string
		want    string
	}{
		{
			name: "build",
			compose: `services:
  web:
    build: .
`,
			want: "Local build contexts are not supported yet.",
		},
		{
			name: "relative volume",
			compose: `services:
  web:
    image: nginx
    volumes:
      - ./data:/data
`,
			want: `Local file reference "./data" is not supported yet.`,
		},
		{
			name: "env file",
			compose: `services:
  web:
    image: nginx
    env_file:
      - .env
`,
			want: "Local env_file is not supported.",
		},
		{
			name: "config file",
			compose: `services:
  web:
    image: nginx
configs:
  app_config:
    file: ./config.yml
`,
			want: `Local file reference "./config.yml" is not supported yet.`,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := extractRequiredSecretNames(tc.compose)
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("error = %v, want containing %q", err, tc.want)
			}
		})
	}
}

func TestExtractRequiredSecretNamesAllowsTargetMachineAndNamedVolumes(t *testing.T) {
	_, err := extractRequiredSecretNames(`services:
  comfyui:
    image: ghcr.io/example/comfyui:latest
    volumes:
      - /mnt/models:/models
      - postgres-data:/var/lib/postgresql/data
volumes:
  postgres-data:
`)
	if err != nil {
		t.Fatalf("expected allowed volumes, got: %v", err)
	}
}

func TestGenerateComposeFileProcessesNetheraPublic(t *testing.T) {
	generated, allocatedPorts, _, endpoints, err := generateComposeFile(`services:
  web:
    image: nginx
    nethera:
      public: 80
    ports:
      - "80"
      - "127.0.0.1:80"
      - "127.0.0.1:8080:80"
`, "myapp", "dep_123", "app_123", "10.100.0.2", nil, "/var/lib/nethera/deployments/dep_123/.env", nil, nil)
	if err != nil {
		t.Fatalf("generate compose: %v", err)
	}
	if allocatedPorts["web:80"] == 0 {
		t.Fatalf("allocated public port missing: %#v", allocatedPorts)
	}
	if len(endpoints) != 1 || endpoints[0].ServiceName != "web" || endpoints[0].TargetPort != 80 {
		t.Fatalf("unexpected endpoints: %#v", endpoints)
	}
	if !strings.Contains(generated, "10.100.0.2:20000:80") {
		t.Fatalf("generated compose missing wireguard public binding:\n%s", generated)
	}
	if strings.Contains(generated, "- 127.0.0.1:80\n") || strings.Contains(generated, "- \"127.0.0.1:80\"\n") {
		t.Fatalf("generated compose kept auto-published localhost binding:\n%s", generated)
	}
	if strings.Contains(generated, "127.0.0.1:8080:80") {
		t.Fatalf("generated compose kept extra mapping for Nethera-owned target port:\n%s", generated)
	}
	if strings.Contains(generated, "nethera:") {
		t.Fatalf("generated compose leaked nethera config:\n%s", generated)
	}
}

func TestApplyPublicEndpointWithPreferLAN(t *testing.T) {
	serviceNode := &yaml.Node{Kind: yaml.MappingNode, Tag: "!!map"}
	portsNode := &yaml.Node{Kind: yaml.SequenceNode, Tag: "!!seq", Content: []*yaml.Node{
		scalarStringNode("18080"),
	}}
	yamlSetMappingValue(serviceNode, "ports", portsNode)
	publicNode := scalarStringNode("18080")
	route, err := applyPublicEndpoint("web", "myapp", "dep_123", serviceNode, publicNode, portsNode, "10.100.0.2", nil, map[int]bool{}, map[int]bool{}, true, "127.0.0.1")
	if err != nil {
		t.Fatalf("apply public endpoint: %v", err)
	}
	if !route.PreferLAN || route.LANHost != "127.0.0.1" || route.LANPort != 18080 {
		t.Fatalf("unexpected LAN endpoint report: %#v", route)
	}
	rendered, err := marshalYAML(serviceNode)
	if err != nil {
		t.Fatalf("marshal service: %v", err)
	}
	if !strings.Contains(rendered, "10.100.0.2:20000:18080") {
		t.Fatalf("missing WireGuard binding:\n%s", rendered)
	}
	if !strings.Contains(rendered, "127.0.0.1:18080:18080") {
		t.Fatalf("missing LAN binding:\n%s", rendered)
	}
}

func TestApplyPublicEndpointWithPreferLANKeepsTargetPortWhenAlreadyBound(t *testing.T) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	defer listener.Close()
	targetPort := listener.Addr().(*net.TCPAddr).Port

	serviceNode := &yaml.Node{Kind: yaml.MappingNode, Tag: "!!map"}
	portsNode := &yaml.Node{Kind: yaml.SequenceNode, Tag: "!!seq", Content: []*yaml.Node{
		scalarStringNode(strconv.Itoa(targetPort)),
	}}
	yamlSetMappingValue(serviceNode, "ports", portsNode)
	route, err := applyPublicEndpoint("web", "myapp", "dep_123", serviceNode, scalarStringNode(strconv.Itoa(targetPort)), portsNode, "10.100.0.2", nil, map[int]bool{}, map[int]bool{}, true, "127.0.0.1")
	if err != nil {
		t.Fatalf("apply public endpoint: %v", err)
	}
	if route.LANPort != targetPort {
		t.Fatalf("expected LAN endpoint to keep target port across redeploys, got route %#v", route)
	}
}

func TestGenerateComposeFileSkipsReservedPublicPorts(t *testing.T) {
	generated, allocatedPorts, _, endpoints, err := generateComposeFileWithReservedPorts(`services:
  web:
    image: nginx
    nethera:
      public: 80
    ports:
      - "80"
`, "myapp", "dep_123", "app_123", "10.100.0.2", nil, []int{20000}, "/var/lib/nethera/deployments/dep_123/.env", nil, nil)
	if err != nil {
		t.Fatalf("generate compose: %v", err)
	}
	if allocatedPorts["web:80"] != 20001 {
		t.Fatalf("expected reserved port to be skipped, got: %#v", allocatedPorts)
	}
	if len(endpoints) != 1 || endpoints[0].HostPort != 20001 {
		t.Fatalf("unexpected endpoints: %#v", endpoints)
	}
	if !strings.Contains(generated, "10.100.0.2:20001:80") {
		t.Fatalf("generated compose missing alternate wireguard binding:\n%s", generated)
	}
}

func TestGenerateComposeFileSkipsDockerRejectedPublicPortOnRetry(t *testing.T) {
	generated, allocatedPorts, _, endpoints, err := generateComposeFileWithReservedPorts(`services:
  comfyui:
    image: nginx
    nethera:
      public: 8188
`, "comfyui", "dep_123", "app_123", "10.110.0.4", map[string]int{"comfyui:8188": 20002}, []int{20002}, "/var/lib/nethera/deployments/dep_123/.env", nil, nil)
	if err != nil {
		t.Fatalf("generate compose: %v", err)
	}
	if allocatedPorts["comfyui:8188"] == 20002 {
		t.Fatalf("expected rejected previous port to be skipped, got: %#v", allocatedPorts)
	}
	if allocatedPorts["comfyui:8188"] != 20000 {
		t.Fatalf("expected next available public port, got: %#v", allocatedPorts)
	}
	if len(endpoints) != 1 || endpoints[0].HostPort != 20000 || endpoints[0].TargetPort != 8188 {
		t.Fatalf("unexpected endpoints: %#v", endpoints)
	}
	if !strings.Contains(generated, "10.110.0.4:20000:8188") {
		t.Fatalf("generated compose missing retried wireguard binding:\n%s", generated)
	}
	if strings.Contains(generated, "10.110.0.4:20002:8188") {
		t.Fatalf("generated compose kept rejected port:\n%s", generated)
	}
}

func TestIsRetryableDockerPortPublishError(t *testing.T) {
	output := `Error response from daemon: ports are not available: exposing port TCP 10.110.0.4:20002 -> 127.0.0.1:0: /forwards/expose returned unexpected status: 500`
	if !isRetryableDockerPortPublishError(output) {
		t.Fatalf("expected Docker Desktop port publish failure to be retryable")
	}
	if isRetryableDockerPortPublishError("Error response from daemon: pull access denied") {
		t.Fatalf("image pull errors should not be treated as port publish failures")
	}
}

func TestOccupiedLocalTCPPortsDetectsBoundPort(t *testing.T) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	defer listener.Close()
	port := listener.Addr().(*net.TCPAddr).Port

	occupied := occupiedLocalTCPPorts("127.0.0.1", port, port)
	if len(occupied) != 1 || occupied[0] != port {
		t.Fatalf("expected occupied port %d, got %#v", port, occupied)
	}
}

func TestMergeReservedPortsDeduplicatesAndSorts(t *testing.T) {
	merged := mergeReservedPorts([]int{20002, 20000}, []int{20001, 20000, -1})
	if !reflect.DeepEqual(merged, []int{20000, 20001, 20002}) {
		t.Fatalf("unexpected merged ports: %#v", merged)
	}
}

func TestGenerateComposeFileProcessesNetheraPublicList(t *testing.T) {
	_, allocatedPorts, _, endpoints, err := generateComposeFile(`services:
  web:
    image: nginx
    nethera:
      public: [80]
    ports:
      - "80"
`, "myapp", "dep_123", "app_123", "10.100.0.2", nil, "/var/lib/nethera/deployments/dep_123/.env", nil, nil)
	if err != nil {
		t.Fatalf("generate compose: %v", err)
	}
	if allocatedPorts["web:80"] == 0 {
		t.Fatalf("allocated public port missing: %#v", allocatedPorts)
	}
	if len(endpoints) != 1 || endpoints[0].TargetPort != 80 {
		t.Fatalf("unexpected endpoints: %#v", endpoints)
	}
}

func TestGenerateComposeFileProcessesNetheraPublicWithoutComposePorts(t *testing.T) {
	generated, allocatedPorts, _, endpoints, err := generateComposeFile(`services:
  web:
    image: nginx
    nethera:
      public: 8080
`, "myapp", "dep_123", "app_123", "10.100.0.2", nil, "/var/lib/nethera/deployments/dep_123/.env", nil, nil)
	if err != nil {
		t.Fatalf("generate compose: %v", err)
	}
	if allocatedPorts["web:8080"] == 0 {
		t.Fatalf("allocated public port missing: %#v", allocatedPorts)
	}
	if len(endpoints) != 1 || endpoints[0].ServiceName != "web" || endpoints[0].TargetPort != 8080 {
		t.Fatalf("unexpected endpoints: %#v", endpoints)
	}
	if !strings.Contains(generated, "10.100.0.2:20000:8080") {
		t.Fatalf("generated compose missing wireguard public binding:\n%s", generated)
	}
	if strings.Contains(generated, "nethera:") {
		t.Fatalf("generated compose leaked nethera config:\n%s", generated)
	}
}

func TestGenerateComposeFileProcessesHostNetworkPublic(t *testing.T) {
	generated, allocatedPorts, _, endpoints, err := generateComposeFile(`services:
  homeassistant:
    image: ghcr.io/home-assistant/home-assistant:stable
    network_mode: host
    nethera:
      public: 8123
`, "ha", "dep_123", "app_123", "10.100.0.2", nil, "/var/lib/nethera/deployments/dep_123/.env", nil, nil)
	if err != nil {
		t.Fatalf("generate compose: %v", err)
	}
	if allocatedPorts["homeassistant:8123"] != 8123 {
		t.Fatalf("host network public port not reported as target port: %#v", allocatedPorts)
	}
	if len(endpoints) != 1 || endpoints[0].ServiceName != "homeassistant" || endpoints[0].HostPort != 8123 || endpoints[0].TargetPort != 8123 {
		t.Fatalf("unexpected endpoints: %#v", endpoints)
	}
	if strings.Contains(generated, "10.100.0.2:") || strings.Contains(generated, "ports:") {
		t.Fatalf("host network service should not get docker port bindings:\n%s", generated)
	}
	if !strings.Contains(generated, "network_mode: host") {
		t.Fatalf("generated compose missing host network mode:\n%s", generated)
	}
	if strings.Contains(generated, "nethera:") {
		t.Fatalf("generated compose leaked nethera config:\n%s", generated)
	}
}

func TestGenerateComposeFileRejectsDuplicateHostNetworkPublicPorts(t *testing.T) {
	_, _, _, _, err := generateComposeFile(`services:
  first:
    image: nginx
    network_mode: host
    nethera:
      public: 8123
  second:
    image: nginx
    network_mode: host
    nethera:
      public: 8123
`, "ha", "dep_123", "app_123", "10.100.0.2", nil, "/var/lib/nethera/deployments/dep_123/.env", nil, nil)
	if err == nil || !strings.Contains(err.Error(), "already used") {
		t.Fatalf("error = %v, want duplicate host public port error", err)
	}
}

func TestGenerateComposeFileAddsManagedFileMounts(t *testing.T) {
	generated, _, _, _, err := generateComposeFile(`services:
  web:
    image: nginx
    volumes:
      - /mnt/cache:/cache
    nethera:
      files:
        nginx.conf:
          source: ./nginx.conf
          target: /etc/nginx/conf.d/default.conf
`, "myapp", "dep_123", "app_123", "10.100.0.2", nil, "/var/lib/nethera/deployments/dep_123/.env", []managedFileMount{
		{
			ServiceName: "web",
			Name:        "nginx.conf",
			Target:      "/etc/nginx/conf.d/default.conf",
			HostPath:    "/var/lib/nethera/deployments/dep_123/files/web/nginx.conf",
		},
	}, nil)
	if err != nil {
		t.Fatalf("generate compose: %v", err)
	}
	for _, want := range []string{
		"- /mnt/cache:/cache",
		"- /var/lib/nethera/deployments/dep_123/files/web/nginx.conf:/etc/nginx/conf.d/default.conf:ro",
	} {
		if !strings.Contains(generated, want) {
			t.Fatalf("generated compose missing %q:\n%s", want, generated)
		}
	}
	for _, forbidden := range []string{"nethera:", "./nginx.conf"} {
		if strings.Contains(generated, forbidden) {
			t.Fatalf("generated compose leaked %q:\n%s", forbidden, generated)
		}
	}
}

func TestGenerateComposeFileRejectsManagedFileVolumeConflict(t *testing.T) {
	_, _, _, _, err := generateComposeFile(`services:
  web:
    image: nginx
    volumes:
      - /mnt/nginx:/etc/nginx/conf.d/default.conf
`, "myapp", "dep_123", "app_123", "10.100.0.2", nil, "/var/lib/nethera/deployments/dep_123/.env", []managedFileMount{
		{
			ServiceName: "web",
			Name:        "nginx.conf",
			Target:      "/etc/nginx/conf.d/default.conf",
			HostPath:    "/var/lib/nethera/deployments/dep_123/files/web/nginx.conf",
		},
	}, nil)
	if err == nil || !strings.Contains(err.Error(), "conflicts with an existing volume mount") {
		t.Fatalf("expected managed file volume conflict, got %v", err)
	}
}

func TestRunCommandStreamingCapturesAndEmitsOutput(t *testing.T) {
	emitted := []string{}
	output, err := runCommandStreaming(exec.CommandContext(context.Background(), "sh", "-c", "printf 'first\\nsecond\\rthird'"), func(stream, line string) {
		emitted = append(emitted, stream+":"+line)
	})
	if err != nil {
		t.Fatalf("run streaming command: %v", err)
	}
	if string(output) != "first\nsecond\rthird" {
		t.Fatalf("captured output = %q", string(output))
	}
	want := []string{"stdout:first", "stdout:second", "stdout:third"}
	if strings.Join(emitted, "|") != strings.Join(want, "|") {
		t.Fatalf("emitted = %#v, want %#v", emitted, want)
	}
}

func TestRunCommandStreamingCapsCapturedOutput(t *testing.T) {
	output, err := runCommandStreaming(exec.CommandContext(context.Background(), "sh", "-c", "yes 0123456789 | head -n 40000"), nil)
	if err != nil {
		t.Fatalf("run streaming command: %v", err)
	}
	if len(output) > commandCaptureMaxOutputBytes {
		t.Fatalf("captured output is not capped: %d > %d", len(output), commandCaptureMaxOutputBytes)
	}
	if !strings.Contains(string(output), "0123456789") {
		t.Fatalf("expected capped output to retain command output")
	}
}

func TestFormatCommandOutputSplitsDockerProgressCarriageReturns(t *testing.T) {
	lines := formatCommandOutput([]byte("layer Downloading [> ] 1MB/2MB\rlayer Downloading [=>] 2MB/2MB\nlocal error: tls: bad record MAC"))
	want := []string{
		"docker output: layer Downloading [> ] 1MB/2MB",
		"docker output: layer Downloading [=>] 2MB/2MB",
		"docker output: local error: tls: bad record MAC",
	}
	if strings.Join(lines, "|") != strings.Join(want, "|") {
		t.Fatalf("lines = %#v, want %#v", lines, want)
	}
}

func TestCompactJobCompleteLogsDropsRepetitiveDockerProgress(t *testing.T) {
	logs := []string{"project: open-webui", "running: docker compose up -d"}
	for index := 0; index < 1000; index += 1 {
		logs = append(logs, "docker output: ed76190d92fe Downloading [> ] 15MB/2.95GB")
	}
	logs = append(logs, "docker output: local error: tls: bad record MAC", "docker compose exit code: 18")
	compacted := compactJobCompleteLogs(logs)
	joined := strings.Join(compacted, "\n")
	if len(compacted) >= len(logs) {
		t.Fatalf("expected compacted logs, got %d original %d", len(compacted), len(logs))
	}
	if !strings.Contains(joined, "omitted") {
		t.Fatalf("expected omitted progress summary in %q", joined)
	}
	if !strings.Contains(joined, "local error: tls: bad record MAC") || !strings.Contains(joined, "docker compose exit code: 18") {
		t.Fatalf("expected final error to be preserved in %q", joined)
	}
	if logPayloadBytes(compacted) > jobCompleteMaxLogBytes {
		t.Fatalf("compacted payload too large: %d", logPayloadBytes(compacted))
	}
}

func TestRunCommandStreamingHonoursCancellation(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	started := time.Now()
	_, err := runCommandStreaming(exec.CommandContext(ctx, "sh", "-c", "sleep 10"), func(_, _ string) {})
	if err == nil {
		t.Fatal("expected command cancellation error")
	}
	if ctx.Err() != context.DeadlineExceeded {
		t.Fatalf("context error = %v", ctx.Err())
	}
	if elapsed := time.Since(started); elapsed > time.Second {
		t.Fatalf("cancelled command took %s", elapsed)
	}
}

func TestDeployLogStreamerSendsWebSocketFrames(t *testing.T) {
	received := make(chan deployLogFrame, 1)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/agent/deploy-jobs/job-1/events-ws" {
			t.Fatalf("path = %s", r.URL.Path)
		}
		if r.Header.Get("Authorization") != "Bearer machine-token" {
			t.Fatalf("authorization header = %q", r.Header.Get("Authorization"))
		}
		conn, err := websocket.Accept(w, r, nil)
		if err != nil {
			t.Fatalf("accept websocket: %v", err)
		}
		defer conn.Close(websocket.StatusNormalClosure, "test complete")
		_, payload, err := conn.Read(r.Context())
		if err != nil {
			t.Fatalf("read websocket frame: %v", err)
		}
		var frame deployLogFrame
		if err := json.Unmarshal(payload, &frame); err != nil {
			t.Fatalf("decode deploy log frame: %v", err)
		}
		received <- frame
	}))
	defer server.Close()

	streamer := startDeployLogStreamer(context.Background(), server.URL, "machine-token", "job-1")
	streamer.Emit("stdout", "pulling image")
	streamer.Close()
	frame := <-received
	if frame.Type != "log" || frame.Stream != "stdout" || frame.Line != "pulling image" || frame.Time == "" {
		t.Fatalf("frame = %#v", frame)
	}
}
