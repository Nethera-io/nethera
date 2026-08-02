package main

import (
	"regexp"
)

type deployRequest struct {
	ComposeYAML        string                `json:"composeYAML"`
	AppName            string                `json:"appName,omitempty"`
	AppID              string                `json:"appId,omitempty"`
	MachineName        string                `json:"machineName,omitempty"`
	DesiredTargetNames []string              `json:"desiredTargetNames,omitempty"`
	ManagedFiles       []managedFileSnapshot `json:"managedFiles,omitempty"`
	ReplaceJobID       string                `json:"replaceJobId,omitempty"`
}

type activeDeployJob struct {
	ID                string `json:"id"`
	Status            string `json:"status"`
	DeploymentID      string `json:"deploymentId"`
	MachineID         string `json:"machineId"`
	MachineName       string `json:"machineName"`
	CreatedAt         string `json:"createdAt"`
	StartedAt         string `json:"startedAt"`
	HeartbeatAt       string `json:"heartbeatAt"`
	CancelRequestedAt string `json:"cancelRequestedAt"`
}

type activeDeployJobResponse struct {
	Job *activeDeployJob `json:"job"`
}

type deployJob struct {
	ID           string                  `json:"id"`
	Status       string                  `json:"status"`
	Logs         string                  `json:"logs"`
	Subdomains   []string                `json:"subdomains,omitempty"`
	Endpoints    []deployEndpointSummary `json:"endpoints,omitempty"`
	PendingAgent bool                    `json:"pendingAgent,omitempty"`
	Message      string                  `json:"message,omitempty"`
}

type deployEndpointSummary struct {
	ServiceName string `json:"serviceName"`
	Hostname    string `json:"hostname"`
	AuthMode    string `json:"authMode"`
	PreferLAN   bool   `json:"preferLan,omitempty"`
	LANHost     string `json:"lanHost,omitempty"`
	LANPort     int    `json:"lanPort,omitempty"`
}

type endpointAccessTokenResult struct {
	ID            string `json:"id"`
	TokenID       string `json:"tokenId"`
	AppID         string `json:"appId"`
	ServiceName   string `json:"serviceName"`
	Token         string `json:"token"`
	Name          string `json:"name"`
	ExpiresAt     string `json:"expiresAt"`
	RevokedAt     string `json:"revokedAt"`
	CreatedAt     string `json:"createdAt"`
	Created       bool   `json:"created"`
	AlreadyExists bool   `json:"alreadyExists"`
}

type endpointAccessTokenResponse struct {
	Tokens []endpointAccessTokenResult `json:"tokens"`
}

type deploymentEndpoint struct {
	ServiceName string `json:"serviceName"`
	Subdomain   string `json:"subdomain"`
	AuthMode    string `json:"authMode"`
	HostPort    int    `json:"hostPort"`
	TargetPort  int    `json:"targetPort"`
}

type deploymentTarget struct {
	DeploymentID  string `json:"deploymentId"`
	MachineID     string `json:"machineId"`
	MachineName   string `json:"machineName"`
	RegionCode    string `json:"regionCode"`
	EndpointCount int    `json:"endpointCount"`
}

type deploymentTargetsResponse struct {
	Targets []deploymentTarget `json:"targets"`
}

type machineSummary struct {
	ID                string         `json:"id"`
	Name              string         `json:"name"`
	RegionCode        string         `json:"regionCode"`
	IsAvailable       bool           `json:"isAvailable"`
	RunningApps       []string       `json:"runningApps"`
	LastSeenAt        string         `json:"lastSeenAt"`
	StatusUpdatedAt   string         `json:"statusUpdatedAt"`
	AgentVersion      string         `json:"agentVersion"`
	StatusSnapshot    map[string]any `json:"statusSnapshot"`
	LifecycleStatus   string         `json:"lifecycleStatus"`
	ManagementEnabled bool           `json:"managementEnabled"`
	ManagementState   string         `json:"managementState"`
	SuspendedReason   string         `json:"suspendedReason"`
}

type netheraManifest struct {
	AppID   string
	AppName string
	Targets []string
	Compose string
}

type managedFileSnapshot struct {
	ServiceName string `json:"serviceName"`
	Name        string `json:"name"`
	Source      string `json:"source"`
	Target      string `json:"target"`
	Mode        string `json:"mode"`
	Content     string `json:"content"`
}

type authConfig struct {
	CurrentEnvironment string                       `json:"currentEnvironment,omitempty"`
	Environments       map[string]environmentConfig `json:"environments,omitempty"`
}

type environmentConfig struct {
	APIURL           string `json:"apiUrl"`
	DownloadsBaseURL string `json:"downloadsBaseUrl,omitempty"`
}

type sessionConfig struct {
	BackendURL  string `json:"backendUrl,omitempty"`
	Token       string `json:"token,omitempty"`
	Email       string `json:"email,omitempty"`
	Workspace   string `json:"workspace,omitempty"`
	Plan        string `json:"plan,omitempty"`
	Role        string `json:"role,omitempty"`
	Environment string `json:"environment,omitempty"`
}

type humanAuthResponse struct {
	Status string `json:"status,omitempty"`
	Token  string `json:"token,omitempty"`
	User   struct {
		Email string `json:"email"`
		Name  string `json:"name"`
	} `json:"user"`
	Workspace struct {
		Name string `json:"name"`
		Slug string `json:"slug"`
	} `json:"workspace"`
	Role string `json:"role"`
	Plan struct {
		Code string `json:"code"`
		Name string `json:"name"`
	} `json:"plan"`
}

type cliLoginStartResponse struct {
	RequestID           string `json:"requestId"`
	PollToken           string `json:"pollToken"`
	BrowserURL          string `json:"browserUrl"`
	ExpiresAt           string `json:"expiresAt"`
	PollIntervalSeconds int    `json:"pollIntervalSeconds"`
}

type pairResponse struct {
	MachineID         string `json:"machineId"`
	MachineToken      string `json:"machineToken"`
	RegionCode        string `json:"regionCode"`
	ManagementEnabled bool   `json:"managementEnabled"`
	MachineState      string `json:"machineState"`
	DisabledReason    string `json:"disabledReason"`
	Message           string `json:"message"`
}

type apiErrorResponse struct {
	Error string `json:"error"`
}

type copySessionEnvelope struct {
	Session      copySessionInfo `json:"session"`
	SessionToken string          `json:"sessionToken"`
}

type copySessionStatusEnvelope struct {
	Session copySessionInfo `json:"session"`
}

type copySessionInfo struct {
	ID           string `json:"id"`
	MachineID    string `json:"machineId"`
	Operation    string `json:"operation"`
	RemotePath   string `json:"remotePath"`
	Status       string `json:"status"`
	ListenerHost string `json:"listenerHost"`
	ListenerPort int    `json:"listenerPort"`
	ErrorMessage string `json:"errorMessage"`
	ExpiresAt    string `json:"expiresAt"`
}

type monthlyUsageResponse struct {
	Month        string `json:"month"`
	Organization struct {
		Name           string `json:"name"`
		BandwidthState string `json:"bandwidthState"`
		Plan           struct {
			Name               string `json:"name"`
			MonthlyBandwidthGB *int   `json:"monthlyBandwidthGb"`
		} `json:"plan"`
	} `json:"organization"`
	Usage struct {
		BytesIn    string `json:"bytesIn"`
		BytesOut   string `json:"bytesOut"`
		TotalBytes string `json:"totalBytes"`
		Requests   string `json:"requests"`
		UpdatedAt  string `json:"updatedAt"`
	} `json:"usage"`
}

type deregisterMachineResponse struct {
	Machine struct {
		ID              string `json:"id"`
		Name            string `json:"name"`
		LifecycleStatus string `json:"lifecycleStatus"`
	} `json:"machine"`
	CleanupJobID string `json:"cleanupJobId"`
}

type appBinding struct {
	AppID       string `json:"appId"`
	WorkspaceID string `json:"workspaceId,omitempty"`
}

type appReference struct {
	ID             string `json:"id"`
	Name           string `json:"name"`
	OrganizationID string `json:"organizationId,omitempty"`
}

type appResolveResponse struct {
	App appReference `json:"app"`
}

type appSecretsResponse struct {
	AppID   string `json:"appId"`
	AppName string `json:"appName"`
	Secrets []struct {
		Name      string `json:"name"`
		UpdatedAt string `json:"updatedAt"`
	} `json:"secrets"`
}

type appSecretMetadata struct {
	Name      string `json:"name"`
	AppID     string `json:"appId"`
	UpdatedAt string `json:"updatedAt"`
}

type appSecretRevealResponse struct {
	Name      string `json:"name"`
	AppID     string `json:"appId"`
	UpdatedAt string `json:"updatedAt"`
	Value     string `json:"value"`
}

type logStreamTargetSummary struct {
	DeploymentID string `json:"deploymentId"`
	MachineID    string `json:"machineId"`
	MachineName  string `json:"machineName"`
}

type logStreamCreateResponse struct {
	ID          string                   `json:"id"`
	Status      string                   `json:"status"`
	TargetCount int                      `json:"targetCount"`
	EventsURL   string                   `json:"eventsUrl"`
	Targets     []logStreamTargetSummary `json:"targets"`
}

type logStreamEvent struct {
	Type         string `json:"type"`
	Status       string `json:"status"`
	Message      string `json:"message"`
	TargetID     string `json:"targetId"`
	DeploymentID string `json:"deploymentId"`
	MachineID    string `json:"machineId"`
	MachineName  string `json:"machineName"`
	Service      string `json:"service"`
	Stream       string `json:"stream"`
	Time         string `json:"time"`
	Line         string `json:"line"`
	Reason       string `json:"reason"`
}

var secretNamePattern = regexp.MustCompile(`^[A-Z_][A-Z0-9_]*$`)
