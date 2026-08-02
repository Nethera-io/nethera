package main

import (
	"fmt"
	"strings"
)

type deployJob struct {
	ID                 string                `json:"id"`
	Type               string                `json:"type"`
	MachineID          string                `json:"machineId"`
	ApplicationID      string                `json:"appId"`
	DeploymentID       string                `json:"deploymentId"`
	ComposeYAML        string                `json:"composeYAML"`
	ManagedFiles       []managedFileSnapshot `json:"managedFiles"`
	ReservedHostPorts  []int                 `json:"reservedHostPorts"`
	Status             string                `json:"status"`
	CleanupDeployments bool                  `json:"cleanupDeployments"`
	CleanupWireGuard   bool                  `json:"cleanupWireGuard"`
}

type deployJobPayload struct {
	ID                 string                `json:"id"`
	MachineID          string                `json:"machineId"`
	AppID              string                `json:"appId"`
	DeploymentID       string                `json:"deploymentId"`
	ComposeYAML        string                `json:"composeYAML"`
	ManagedFiles       []managedFileSnapshot `json:"managedFiles"`
	ReservedHostPorts  []int                 `json:"reservedHostPorts"`
	Status             string                `json:"status"`
	Type               string                `json:"type"`
	CleanupDeployments bool                  `json:"cleanupDeployments"`
	CleanupWireGuard   bool                  `json:"cleanupWireGuard"`
}

type managedFileSnapshot struct {
	ServiceName string `json:"serviceName"`
	Name        string `json:"name"`
	Source      string `json:"source"`
	Target      string `json:"target"`
	Mode        string `json:"mode"`
	Content     string `json:"content"`
}

type managedFileMount struct {
	ServiceName string
	Name        string
	Target      string
	HostPath    string
}

type agentPollResponse struct {
	ServerTime       string                   `json:"serverTime"`
	PollAfterSeconds int                      `json:"pollAfterSeconds"`
	MachineState     string                   `json:"machineState"`
	SuspendedReason  string                   `json:"suspendedReason,omitempty"`
	Jobs             []deployJobPayload       `json:"jobs"`
	CancelJobIDs     []string                 `json:"cancelJobIds"`
	LogStreamTargets []logStreamTargetPayload `json:"logStreamTargets"`
	CopySessions     []copySessionPayload     `json:"copySessions"`
	AgentUpdate      agentUpdatePayload       `json:"agentUpdate"`
}

type agentUpdatePayload struct {
	Available bool   `json:"available"`
	Version   string `json:"version"`
	Mandatory bool   `json:"mandatory"`
	URL       string `json:"url"`
	SHA256    string `json:"sha256"`
	Reason    string `json:"reason"`
	Error     string `json:"error"`
}

type logStreamTargetPayload struct {
	SessionID    string `json:"sessionId"`
	TargetID     string `json:"targetId"`
	DeploymentID string `json:"deploymentId"`
	ServiceName  string `json:"serviceName"`
	TailLines    int    `json:"tailLines"`
	Follow       bool   `json:"follow"`
}

type copySessionPayload struct {
	ID           string         `json:"id"`
	Operation    string         `json:"operation"`
	RemotePath   string         `json:"remotePath"`
	SessionToken string         `json:"sessionToken"`
	Manifest     map[string]any `json:"manifest"`
	ExpiresAt    string         `json:"expiresAt"`
}

type agentLogFrame struct {
	Type         string `json:"type"`
	TargetID     string `json:"targetId,omitempty"`
	DeploymentID string `json:"deploymentId,omitempty"`
	MachineID    string `json:"machineId,omitempty"`
	Service      string `json:"service,omitempty"`
	Stream       string `json:"stream,omitempty"`
	Time         string `json:"time,omitempty"`
	Line         string `json:"line,omitempty"`
	Message      string `json:"message,omitempty"`
	Reason       string `json:"reason,omitempty"`
}

type deploymentSecretsResponse struct {
	DeploymentID         string                      `json:"deploymentId"`
	AppID                string                      `json:"appId"`
	Secrets              map[string]string           `json:"secrets"`
	RuntimeSecrets       map[string]string           `json:"runtimeSecrets"`
	GeneratedEnv         map[string]string           `json:"generatedEnv"`
	ImagePullCredentials []imagePullCredentialSecret `json:"imagePullCredentials"`
}

type imagePullCredentialSecret struct {
	Registry string `json:"registry"`
	Username string `json:"username"`
	Password string `json:"password"`
}

type deploymentSecretBundle struct {
	RuntimeSecrets       map[string]string
	GeneratedEnv         map[string]string
	ImagePullCredentials []imagePullCredentialSecret
}

type publicEndpointReport struct {
	ServiceName string `json:"serviceName"`
	Subdomain   string `json:"subdomain"`
	HostPort    int    `json:"hostPort"`
	TargetPort  int    `json:"targetPort"`
	PreferLAN   bool   `json:"preferLan,omitempty"`
	LANHost     string `json:"lanHost,omitempty"`
	LANPort     int    `json:"lanPort,omitempty"`
}

type machineConfig struct {
	BackendURL   string `json:"backendUrl"`
	MachineID    string `json:"machineId"`
	MachineToken string `json:"machineToken"`
	Environment  string `json:"environment,omitempty"`
	APIURL       string `json:"apiUrl,omitempty"`
}

type agentUpdateState struct {
	LastAttemptAt       string `json:"lastUpdateAttemptAt,omitempty"`
	LastError           string `json:"lastUpdateError,omitempty"`
	NextAttemptAfter    string `json:"nextUpdateAttemptAfter,omitempty"`
	ConsecutiveFailures int    `json:"consecutiveFailures,omitempty"`
}

type wireGuardNetworkResponse struct {
	MachineID string                     `json:"machineId"`
	Interface wireGuardInterfaceSettings `json:"interface"`
	Peer      wireGuardPeerSettings      `json:"peer"`
}

type wireGuardInterfaceSettings struct {
	PrivateKey string `json:"privateKey"`
	Address    string `json:"address"`
}

type wireGuardPeerSettings struct {
	PublicKey           string `json:"publicKey"`
	Endpoint            string `json:"endpoint"`
	AllowedIPs          string `json:"allowedIPs"`
	PersistentKeepalive int    `json:"persistentKeepalive"`
}

type deploymentMetadata struct {
	DeploymentID         string         `json:"deploymentId"`
	ApplicationID        string         `json:"applicationId"`
	ComposeHash          string         `json:"composeHash"`
	ProjectName          string         `json:"projectName"`
	GeneratedComposePath string         `json:"generatedComposePath"`
	AllocatedPorts       map[string]int `json:"allocatedPorts"`
	LastAppliedAt        string         `json:"lastAppliedAt"`
	SelfHealDisabled     bool           `json:"selfHealDisabled,omitempty"`
}

type deploymentStatusReport struct {
	DeploymentID     string                  `json:"deploymentId"`
	Status           string                  `json:"status"`
	ComposeHash      string                  `json:"composeHash"`
	SelfHealDisabled bool                    `json:"selfHealDisabled,omitempty"`
	Containers       []containerStatusReport `json:"containers"`
}

type containerStatusReport struct {
	Name         string `json:"name"`
	State        string `json:"state"`
	RestartCount int    `json:"restartCount"`
}

type cpuSample struct {
	total uint64
	idle  uint64
}

var previousCPUSample *cpuSample

type httpStatusError struct {
	Endpoint string
	Status   int
	Details  string
}

func (e *httpStatusError) Error() string {
	if e.Details != "" {
		return fmt.Sprintf("unexpected status %d from %s: %s", e.Status, e.Endpoint, e.Details)
	}
	return fmt.Sprintf("unexpected status %d from %s", e.Status, e.Endpoint)
}

func summarizeBody(body []byte) string {
	trimmed := strings.TrimSpace(string(body))
	if trimmed == "" {
		return ""
	}
	if len(trimmed) > 240 {
		return trimmed[:240] + "..."
	}
	return trimmed
}
