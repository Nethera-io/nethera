package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/signal"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
)

func runDeploy(args []string) {
	runComposeAction(args, "deploy")
}

func runDestroy(args []string) {
	runComposeAction(args, "destroy")
}

func runComposeAction(args []string, action string) {
	fs := flag.NewFlagSet("deploy", flag.ExitOnError)
	backendURL := fs.String("backend", defaultBackendURL(), "backend base URL")
	manifestPath := fs.String("manifest", "", "nethera manifest path")
	verbose := fs.Bool("verbose", false, "show full deployment logs")
	replaceActive := fs.Bool("replace", false, "replace an active deployment on the target machine")
	waitActive := fs.Bool("wait", false, "wait for an active deployment before submitting this deployment")
	noToken := fs.Bool("no-token", false, "do not prompt to create endpoint bearer tokens for auth: token services")
	destroyVolumes := fs.Bool("volumes", false, "also remove Docker Compose named and anonymous volumes when destroying")
	yes := fs.Bool("yes", false, "skip confirmation prompts")
	fs.Parse(args)
	if *replaceActive && *waitActive {
		fmt.Println("--replace and --wait cannot be used together")
		os.Exit(1)
	}
	if *destroyVolumes && action != "destroy" {
		fmt.Println("--volumes can only be used with neth destroy")
		os.Exit(1)
	}

	inputPath := strings.TrimSpace(fs.Arg(0))
	if inputPath == "" {
		inputPath = "."
	}

	resolvedManifestPath, err := resolveNetheraManifestPath(inputPath)
	if err != nil {
		fmt.Printf("failed to resolve nethera manifest: %v\n", err)
		os.Exit(1)
	}

	if override := strings.TrimSpace(*manifestPath); override != "" {
		resolvedManifestPath = override
	}
	manifest, err := loadManifest(resolvedManifestPath)
	if err != nil {
		fmt.Printf("failed to read manifest: %v\n", err)
		os.Exit(1)
	}
	if len(manifest.Targets) == 0 {
		fmt.Println("no targets configured; run 'neth init' first")
		os.Exit(1)
	}
	if strings.TrimSpace(manifest.Compose) == "" {
		fmt.Printf("manifest %s does not contain compose content\n", resolvedManifestPath)
		os.Exit(1)
	}

	token, err := loadAuthToken(*backendURL)
	if err != nil {
		fmt.Println(err)
		os.Exit(1)
	}

	appName := strings.TrimSpace(manifest.AppName)
	if appName == "" {
		appName = defaultAppNameFromDir(filepath.Dir(resolvedManifestPath))
	}
	appID := strings.TrimSpace(manifest.AppID)
	if appID == "" {
		registeredAppID, registerErr := ensureAppRegistration(*backendURL, token, appName)
		if registerErr != nil {
			fmt.Printf("failed to register app: %v\n", registerErr)
			os.Exit(1)
		}
		appID = registeredAppID
		manifest.AppID = appID
		manifest.AppName = appName
		if saveErr := saveManifest(resolvedManifestPath, manifest); saveErr != nil {
			fmt.Printf("failed to persist app id in manifest: %v\n", saveErr)
			os.Exit(1)
		}
		fmt.Printf("registered app id: %s\n", appID)
	}
	composePayload := annotateComposeMetadata(manifest.Compose, appName, action, *destroyVolumes)
	managedFiles := []managedFileSnapshot{}
	if action == "deploy" {
		managedFiles, err = collectManagedFiles(manifest.Compose, resolvedManifestPath)
		if err != nil {
			fmt.Printf("failed to collect managed files: %v\n", err)
			os.Exit(1)
		}
		serviceFailureSummaries, serviceFailureErr := extractServiceFailureSummaries(manifest.Compose)
		if serviceFailureErr != nil {
			fmt.Printf("failed to inspect service failure alert config: %v\n", serviceFailureErr)
			os.Exit(1)
		}
		for _, summary := range serviceFailureSummaries {
			fmt.Println("Service failure alerts")
			fmt.Printf("  Service: %s\n", summary.ServiceName)
			fmt.Printf("  Trigger: no reachable backend for %s\n", summary.After)
			if len(summary.WebhookSecrets) > 0 {
				fmt.Printf("  Webhook secrets: %s\n", strings.Join(summary.WebhookSecrets, ", "))
			}
			fmt.Printf("  Emails: %s\n", strings.Join(summary.Emails, ", "))
		}
	}

	targets := manifest.Targets
	desiredTargetNames := normalizeDesiredTargetNames(manifest.Targets)
	if action == "deploy" {
		if warning, warnErr := gpuDeploymentWarning(*backendURL, token, manifest.Compose, targets); warnErr != nil {
			fmt.Printf("warning: could not inspect GPU readiness: %v\n", warnErr)
		} else if warning != "" {
			fmt.Println(warning)
		}
	}
	if !*yes && action == "destroy" {
		confirmed, confirmErr := confirmDestroyTargets(appName, targets, *destroyVolumes)
		if confirmErr != nil {
			fmt.Printf("failed to read confirmation: %v\n", confirmErr)
			os.Exit(1)
		}
		if !confirmed {
			fmt.Println("destroy cancelled")
			os.Exit(1)
		}
	}
	if !*yes && action == "deploy" && len(desiredTargetNames) > 0 {
		currentTargets, targetErr := fetchDeploymentTargets(*backendURL, token, appID)
		if targetErr != nil {
			fmt.Printf("failed to inspect current deployment targets: %v\n", targetErr)
			os.Exit(1)
		}
		removedTargets := deploymentTargetsScheduledForRemoval(currentTargets, desiredTargetNames)
		if len(removedTargets) > 0 {
			confirmed, confirmErr := confirmRemovedDeploymentTargets(appName, removedTargets)
			if confirmErr != nil {
				fmt.Printf("failed to read confirmation: %v\n", confirmErr)
				os.Exit(1)
			}
			if !confirmed {
				fmt.Println("deployment cancelled")
				os.Exit(1)
			}
		}
	}
	if action == "deploy" && len(targets) > 1 {
		warnings, warnErr := writableVolumeWarnings(manifest.Compose)
		if warnErr != nil {
			fmt.Printf("failed to inspect compose volumes: %v\n", warnErr)
			os.Exit(1)
		}
		if len(warnings) > 0 {
			fmt.Println("warning: this app uses writable volumes and is being deployed to multiple machines.")
			fmt.Println("Each machine will use its own local volume or host path. Nethera does not replicate application state between machines.")
			for _, warning := range warnings {
				fmt.Printf(" - %s\n", warning)
			}
			if !*yes {
				confirmed, confirmErr := promptYesNoDefaultNo("Continue with multi-machine deployment?")
				if confirmErr != nil {
					fmt.Printf("failed to read confirmation: %v\n", confirmErr)
					os.Exit(1)
				}
				if !confirmed {
					fmt.Println("deployment cancelled")
					os.Exit(1)
				}
			}
		}
	}
	endpointsByMachine := map[string][]deployEndpointSummary{}
	machineOrder := make([]string, 0, len(targets))
	firstEndpointDeploy := false
	tokenServiceNames := []string{}
	if action == "deploy" && !*noToken {
		tokenServiceNames, err = extractTokenAuthServiceNames(manifest.Compose)
		if err != nil {
			fmt.Printf("failed to inspect endpoint token services: %v\n", err)
			os.Exit(1)
		}
	}

	for _, target := range targets {
		replaceJobID := ""
		targetHadEndpoints := false
		activeJob, activeErr := fetchActiveDeployJob(*backendURL, token, appID, target)
		if activeErr != nil {
			fmt.Printf("failed to inspect active deployment for %s: %v\n", target, activeErr)
			os.Exit(1)
		}
		if activeJob != nil {
			choice := ""
			switch {
			case *replaceActive:
				choice = "replace"
			case *waitActive:
				choice = "wait"
			default:
				if !deployOutputIsTerminal() {
					fmt.Printf("deployment %s is already %s on %s; rerun with --wait or --replace\n", activeJob.ID, activeJob.Status, target)
					os.Exit(1)
				}
				choice, err = promptActiveDeploymentChoice(*activeJob, action)
				if err != nil {
					fmt.Printf("failed to read deployment choice: %v\n", err)
					os.Exit(1)
				}
			}
			if choice == "exit" {
				fmt.Printf("deployment %s continues on %s\n", activeJob.ID, target)
				return
			}
			if choice == "wait" {
				_, detached, waitErr := waitForDeployJob(*backendURL, token, activeJob.ID, action, target, *verbose)
				if waitErr != nil {
					fmt.Printf("failed while waiting for deployment %s: %v\n", activeJob.ID, waitErr)
					return
				}
				if detached {
					return
				}
				fmt.Printf("continuing with the new deployment on %s\n", target)
			} else {
				replaceJobID = activeJob.ID
				fmt.Printf("requesting replacement of deployment %s on %s\n", activeJob.ID, target)
			}
		}
		if action == "deploy" {
			existingEndpoints, listErr := fetchDeploymentEndpoints(*backendURL, token, appID, target)
			if listErr != nil {
				fmt.Printf("failed to inspect current endpoints for %s: %v\n", target, listErr)
				os.Exit(1)
			}
			targetHadEndpoints = len(existingEndpoints) > 0
			desiredPublicServices, parseErr := extractPublicServiceNames(manifest.Compose)
			if parseErr != nil {
				fmt.Printf("failed to inspect compose public services: %v\n", parseErr)
				os.Exit(1)
			}
			endpointsToRemove := endpointsScheduledForRemoval(existingEndpoints, desiredPublicServices)
			if len(endpointsToRemove) > 0 {
				fmt.Printf("warning: this deploy will remove %d endpoint(s) on %s:\n", len(endpointsToRemove), target)
				for _, endpoint := range endpointsToRemove {
					fmt.Printf(" - %s -> %s\n", endpoint.ServiceName, endpoint.Subdomain)
				}
				if !*yes {
					confirmed, confirmErr := promptYesNoDefaultNo("Continue with deployment?")
					if confirmErr != nil {
						fmt.Printf("failed to read confirmation: %v\n", confirmErr)
						os.Exit(1)
					}
					if !confirmed {
						fmt.Println("deployment cancelled")
						os.Exit(1)
					}
				}
			}
		}

		fmt.Printf("%sing to %s\n", action, target)
		req := deployRequest{ComposeYAML: composePayload, AppName: appName, AppID: appID, MachineName: target, DesiredTargetNames: desiredTargetNames, ManagedFiles: managedFiles, ReplaceJobID: replaceJobID}
		payload, _ := json.Marshal(req)
		httpReq, err := http.NewRequest(http.MethodPost, *backendURL+"/deploy", bytes.NewReader(payload))
		if err != nil {
			fmt.Printf("failed to create deploy request: %v\n", err)
			os.Exit(1)
		}
		httpReq.Header.Set("Authorization", "Bearer "+token)
		httpReq.Header.Set("Content-Type", "application/json")

		resp, err := http.DefaultClient.Do(httpReq)
		if err != nil {
			fmt.Printf("failed to submit deploy: %v\n", err)
			os.Exit(1)
		}
		if resp.StatusCode != http.StatusOK {
			body, _ := io.ReadAll(resp.Body)
			var errorBody struct {
				Error string `json:"error"`
			}
			if json.Unmarshal(body, &errorBody) == nil && strings.TrimSpace(errorBody.Error) != "" {
				fmt.Printf("%s request rejected: %s\n", action, errorBody.Error)
			} else {
				fmt.Printf("%s request rejected with status %d\n", action, resp.StatusCode)
			}
			_ = resp.Body.Close()
			os.Exit(1)
		}
		var result deployJob
		if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
			fmt.Printf("failed to decode deploy response: %v\n", err)
			_ = resp.Body.Close()
			os.Exit(1)
		}
		_ = resp.Body.Close()
		if result.PendingAgent {
			message := strings.TrimSpace(result.Message)
			if message == "" {
				message = fmt.Sprintf("Deployment saved. Machine %s is unreachable; Nethera will apply it when the agent reconnects.", target)
			}
			fmt.Println(message)
			continue
		}
		if *verbose {
			fmt.Printf("%s job created: %s\n", action, result.ID)
		}
		job, detached, waitErr := waitForDeployJob(*backendURL, token, result.ID, action, target, *verbose)
		if waitErr != nil {
			fmt.Printf("failed to wait for job status: %v\n", waitErr)
			return
		}
		if detached {
			return
		}
		if job != nil && job.Status != "succeeded" {
			os.Exit(1)
		}
		if job != nil && job.Status == "succeeded" && action == "deploy" {
			cleaned := normalizeDeployEndpoints(job)
			if len(cleaned) > 0 {
				if !targetHadEndpoints {
					firstEndpointDeploy = true
				}
				if _, exists := endpointsByMachine[target]; !exists {
					machineOrder = append(machineOrder, target)
				}
				endpointsByMachine[target] = cleaned
			}
		}
	}

	if action == "deploy" {
		printDeployEndpointsByMachine(machineOrder, endpointsByMachine)
		if firstEndpointDeploy {
			printFirstDeployEndpointNote()
		}
		if !*noToken && len(tokenServiceNames) > 0 {
			tokens, err := promptForEndpointAccessTokens(*backendURL, token, appID, tokenServiceNames)
			if err != nil {
				fmt.Printf("failed to create endpoint token: %v\n", err)
				os.Exit(1)
			}
			printEndpointAccessTokens(tokens)
		}
	}
}

func printFirstDeployEndpointNote() {
	fmt.Println()
	fmt.Println("Some apps take a while to accept traffic after the container starts.")
	fmt.Println("If the URL is briefly unavailable, wait a moment and refresh; Nethera will route traffic as soon as the service is reachable.")
}

func normalizeSubdomains(subdomains []string) []string {
	cleaned := make([]string, 0, len(subdomains))
	seen := map[string]bool{}
	for _, entry := range subdomains {
		trimmed := strings.TrimSpace(entry)
		if trimmed == "" || seen[trimmed] {
			continue
		}
		seen[trimmed] = true
		cleaned = append(cleaned, trimmed)
	}
	return cleaned
}

func promptActiveDeploymentChoice(job activeDeployJob, action string) (string, error) {
	startedAt := job.StartedAt
	if strings.TrimSpace(startedAt) == "" {
		startedAt = job.CreatedAt
	}
	duration := "unknown duration"
	if parsed, err := time.Parse(time.RFC3339, startedAt); err == nil {
		duration = time.Since(parsed).Round(time.Second).String()
	}
	heartbeat := "no heartbeat"
	if parsed, err := time.Parse(time.RFC3339, job.HeartbeatAt); err == nil {
		heartbeat = fmt.Sprintf("last heartbeat %s ago", time.Since(parsed).Round(time.Second))
	}
	operation := "deployment"
	replaceLabel := "Replace with this deployment"
	if action == "destroy" {
		operation = "deployment job"
		replaceLabel = "Cancel it and destroy this app"
	}
	fmt.Printf("\nA %s is already %s on %s.\n", operation, job.Status, job.MachineName)
	fmt.Printf("Job: %s · %s · %s\n\n", job.ID, duration, heartbeat)
	fmt.Println("  [w] Watch and wait (recommended)")
	fmt.Printf("  [r] %s\n", replaceLabel)
	fmt.Println("  [e] Leave it running and exit")
	reader := bufio.NewReader(os.Stdin)
	for {
		fmt.Print("Choose [w/r/e]: ")
		value, err := reader.ReadString('\n')
		if err != nil {
			return "", err
		}
		switch strings.ToLower(strings.TrimSpace(value)) {
		case "", "w", "wait", "watch":
			return "wait", nil
		case "r", "replace", "override":
			return "replace", nil
		case "e", "exit", "leave":
			return "exit", nil
		default:
			fmt.Println("Enter w to wait, r to continue, or e to exit.")
		}
	}
}

func deployJobIsActive(status string) bool {
	switch strings.ToLower(strings.TrimSpace(status)) {
	case "queued", "deploying", "running", "cancel_requested":
		return true
	default:
		return false
	}
}

func waitForDeployJob(backendURL, token, jobID, action, target string, verbose bool) (*deployJob, bool, error) {
	waitContext, stopSignals := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stopSignals()
	streamResults := startDeployJobEventStream(waitContext, backendURL, token, jobID)
	pollTicker := time.NewTicker(2 * time.Second)
	defer pollTicker.Stop()
	var logPane *deployLogPane
	if !verbose && deployOutputIsTerminal() {
		logPane = newDeployLogPane(os.Stdout, progressLabel(action, target), 10)
		logPane.Add("Waiting for the machine agent...")
	}
	closePane := func() {
		if logPane != nil {
			logPane.Close()
		}
	}
	liveLogCounts := map[string]int{}
	lastStatus := ""
	for {
		select {
		case <-waitContext.Done():
			closePane()
			fmt.Printf("\nDetached from %s job %s.\n", action, jobID)
			fmt.Printf("The %s is continuing on %s. Re-run `neth %s` to watch or replace it.\n", action, target, action)
			return nil, true, nil
		case streamResult, open := <-streamResults:
			if !open {
				streamResults = nil
				continue
			}
			if streamResult.Err != nil {
				message := fmt.Sprintf("live deploy logs unavailable: %v", streamResult.Err)
				if logPane != nil {
					logPane.Add(message)
				} else {
					fmt.Fprintln(os.Stderr, message)
				}
				continue
			}
			if streamResult.Event == nil {
				continue
			}
			line := strings.TrimSpace(streamResult.Event.Line)
			if line == "" && streamResult.Event.Type == "error" {
				line = strings.TrimSpace(streamResult.Event.Message)
			}
			if line == "" && streamResult.Event.Type == "status" && streamResult.Event.Status != lastStatus {
				line = "Deployment status: " + streamResult.Event.Status
				lastStatus = streamResult.Event.Status
			}
			if line == "" {
				continue
			}
			liveLogCounts[line] += 1
			if logPane != nil {
				logPane.Add(line)
			} else if streamResult.Event.Type != "status" || verbose {
				fmt.Println(sanitizeDeployLogLine(line))
			}
			continue
		case <-pollTicker.C:
		}

		job, err := fetchJob(backendURL, jobID, token)
		if err != nil {
			closePane()
			return nil, false, err
		}
		if deployJobIsActive(job.Status) {
			if verbose && job.Status != lastStatus {
				fmt.Printf("status: %s\n", job.Status)
			}
			lastStatus = job.Status
			continue
		}
		stopSignals()
		closePane()
		if verbose {
			fmt.Printf("status: %s\n", job.Status)
		}
		formattedLogs := formatDeployLogs(job.Logs)
		switch job.Status {
		case "failed":
			if verbose {
				fmt.Println("deploy logs:")
				for _, line := range formattedLogs {
					if liveLogCounts[line] > 0 {
						liveLogCounts[line]--
						continue
					}
					fmt.Printf("  - %s\n", line)
				}
			} else {
				printActionFailure(action, formattedLogs)
			}
		case "succeeded":
			if verbose {
				for _, line := range formattedLogs {
					if liveLogCounts[line] > 0 {
						liveLogCounts[line]--
						continue
					}
					fmt.Printf("  - %s\n", line)
				}
			} else {
				printActionSuccess(action)
			}
		case "cancelled", "superseded":
			fmt.Printf("\nDeployment %s.\n", job.Status)
		default:
			fmt.Printf("\nDeployment ended with status %s.\n", job.Status)
		}
		return job, false, nil
	}
}

func normalizeDesiredTargetNames(targets []string) []string {
	cleaned := make([]string, 0, len(targets))
	seen := map[string]bool{}
	for _, target := range targets {
		trimmed := strings.TrimSpace(target)
		if trimmed == "" || seen[trimmed] {
			continue
		}
		seen[trimmed] = true
		cleaned = append(cleaned, trimmed)
	}
	return cleaned
}

func gpuDeploymentWarning(backendURL, token, composeContent string, targets []string) (string, error) {
	info, err := inspectGPUReservations(composeContent)
	if err != nil {
		return "", err
	}
	if !info.UsesGPU {
		return "", nil
	}

	machines, err := listMachines(backendURL, token)
	if err != nil {
		return "", err
	}
	machinesByName := map[string]machineSummary{}
	for _, machine := range machines {
		machinesByName[strings.ToLower(strings.TrimSpace(machine.Name))] = machine
	}

	lines := []string{}
	for _, target := range normalizeDesiredTargetNames(targets) {
		machine, ok := machinesByName[strings.ToLower(strings.TrimSpace(target))]
		if !ok {
			continue
		}
		gpuStatus := formatGPUDiagnostics(machine.StatusSnapshot)
		if gpuStatus == "" {
			lines = append(lines, fmt.Sprintf(" - %s: GPU diagnostics are not available yet. Update/restart the Nethera agent, then run `neth machine stats`.", target))
			continue
		}
		if strings.HasPrefix(gpuStatus, "ready") {
			continue
		}
		lines = append(lines, fmt.Sprintf(" - %s: %s", target, gpuStatus))
	}
	if len(lines) == 0 {
		return "", nil
	}

	var builder strings.Builder
	builder.WriteString("warning: this deployment requests GPU access, but Nethera found possible GPU configuration issues:\n")
	for _, line := range lines {
		builder.WriteString(line)
		builder.WriteByte('\n')
	}
	return strings.TrimRight(builder.String(), "\n"), nil
}

type gpuReservationInfo struct {
	UsesGPU bool
}

func inspectGPUReservations(composeContent string) (gpuReservationInfo, error) {
	var root yaml.Node
	if err := yaml.Unmarshal([]byte(composeContent), &root); err != nil {
		return gpuReservationInfo{}, fmt.Errorf("compose yaml is invalid: %w", err)
	}
	if len(root.Content) == 0 || root.Content[0].Kind != yaml.MappingNode {
		return gpuReservationInfo{}, nil
	}
	servicesNode := mappingValue(root.Content[0], "services")
	if servicesNode == nil || servicesNode.Kind != yaml.MappingNode {
		return gpuReservationInfo{}, nil
	}
	result := gpuReservationInfo{}
	for index := 0; index < len(servicesNode.Content); index += 2 {
		serviceNode := servicesNode.Content[index+1]
		if serviceNode.Kind != yaml.MappingNode {
			continue
		}
		devicesNode := serviceDevicesNode(serviceNode)
		if devicesNode == nil || devicesNode.Kind != yaml.SequenceNode {
			continue
		}
		for _, deviceNode := range devicesNode.Content {
			if deviceNode.Kind != yaml.MappingNode {
				continue
			}
			if deviceRequestsGPU(deviceNode) {
				result.UsesGPU = true
			}
		}
	}
	return result, nil
}

func serviceDevicesNode(serviceNode *yaml.Node) *yaml.Node {
	deployNode := mappingValue(serviceNode, "deploy")
	if deployNode == nil || deployNode.Kind != yaml.MappingNode {
		return nil
	}
	resourcesNode := mappingValue(deployNode, "resources")
	if resourcesNode == nil || resourcesNode.Kind != yaml.MappingNode {
		return nil
	}
	reservationsNode := mappingValue(resourcesNode, "reservations")
	if reservationsNode == nil || reservationsNode.Kind != yaml.MappingNode {
		return nil
	}
	return mappingValue(reservationsNode, "devices")
}

func deviceRequestsGPU(deviceNode *yaml.Node) bool {
	capabilitiesNode := mappingValue(deviceNode, "capabilities")
	if capabilitiesNode == nil {
		return false
	}
	switch capabilitiesNode.Kind {
	case yaml.SequenceNode:
		for _, capability := range capabilitiesNode.Content {
			if strings.EqualFold(strings.TrimSpace(capability.Value), "gpu") {
				return true
			}
		}
	case yaml.ScalarNode:
		return strings.Contains(strings.ToLower(capabilitiesNode.Value), "gpu")
	}
	return false
}

func targetNameSet(targets []string) map[string]bool {
	set := map[string]bool{}
	for _, target := range targets {
		trimmed := strings.TrimSpace(target)
		if trimmed != "" {
			set[trimmed] = true
		}
	}
	return set
}

func deploymentTargetsScheduledForRemoval(current []deploymentTarget, desiredTargetNames []string) []deploymentTarget {
	desired := targetNameSet(desiredTargetNames)
	removed := make([]deploymentTarget, 0)
	for _, target := range current {
		machineName := strings.TrimSpace(target.MachineName)
		if machineName != "" && !desired[machineName] {
			removed = append(removed, target)
		}
	}
	sort.SliceStable(removed, func(i, j int) bool {
		return removed[i].MachineName < removed[j].MachineName
	})
	return removed
}

func confirmRemovedDeploymentTargets(appName string, removed []deploymentTarget) (bool, error) {
	fmt.Printf("warning: this deploy will remove %d target(s) from app %s:\n", len(removed), appName)
	for _, target := range removed {
		line := fmt.Sprintf(" - %s", target.MachineName)
		if target.RegionCode != "" {
			line += fmt.Sprintf(" [%s]", target.RegionCode)
		}
		if target.EndpointCount > 0 {
			line += fmt.Sprintf(" (%d endpoint(s))", target.EndpointCount)
		}
		fmt.Println(line)
	}
	fmt.Println("Nethera will remove these endpoints now and queue cleanup on the removed machine target(s).")
	fmt.Println("If a removed machine is offline, cleanup will run when the agent reconnects.")
	fmt.Println("Existing local containers for this app may be stopped by the cleanup job; volumes are not deleted.")
	return promptYesNoDefaultNo("Continue with target change?")
}

func confirmDestroyTargets(appName string, targets []string, destroyVolumes bool) (bool, error) {
	fmt.Printf("warning: this will destroy app %s on %d target(s):\n", appName, len(targets))
	for _, target := range normalizeDesiredTargetNames(targets) {
		fmt.Printf(" - %s\n", target)
	}
	fmt.Println("Nethera will remove public endpoints for this app and stop its Nethera-managed Compose project on those machine(s).")
	if destroyVolumes {
		fmt.Println("Docker Compose named and anonymous volumes for this app will also be deleted.")
		fmt.Println("Host bind mount directories are not deleted.")
	} else {
		fmt.Println("Docker volumes are not deleted. Use --volumes to delete Compose-managed volumes too.")
	}
	return promptYesNoDefaultNo("Continue with destroy?")
}

func normalizeDeployEndpoints(job *deployJob) []deployEndpointSummary {
	cleaned := make([]deployEndpointSummary, 0)
	seen := map[string]bool{}
	for _, endpoint := range job.Endpoints {
		hostname := strings.TrimSpace(endpoint.Hostname)
		if hostname == "" || seen[hostname] {
			continue
		}
		seen[hostname] = true
		authMode := strings.ToLower(strings.TrimSpace(endpoint.AuthMode))
		if authMode == "" {
			authMode = "none"
		}
		cleaned = append(cleaned, deployEndpointSummary{
			ServiceName: strings.TrimSpace(endpoint.ServiceName),
			Hostname:    hostname,
			AuthMode:    authMode,
			PreferLAN:   endpoint.PreferLAN,
			LANHost:     strings.TrimSpace(endpoint.LANHost),
			LANPort:     endpoint.LANPort,
		})
	}
	if len(cleaned) > 0 {
		return cleaned
	}
	for _, subdomain := range normalizeSubdomains(job.Subdomains) {
		cleaned = append(cleaned, deployEndpointSummary{
			Hostname: subdomain,
			AuthMode: "none",
		})
	}
	return cleaned
}

func printDeployEndpointsByMachine(machineOrder []string, endpointsByMachine map[string][]deployEndpointSummary) {
	total := 0
	for _, machine := range machineOrder {
		total += len(endpointsByMachine[machine])
	}
	if total == 0 {
		return
	}

	fmt.Println("endpoints:")
	for _, group := range groupedDeployEndpoints(machineOrder, endpointsByMachine) {
		label := strings.Join(group.Machines, ", ")
		if len(group.Machines) > 1 {
			label += " (load balanced)"
		}
		fmt.Printf(" - %s:\n", label)
		for _, endpoint := range group.Endpoints {
			fmt.Printf("   - %s\n", endpoint.Hostname)
			fmt.Printf("     %s\n", endpointAuthDescription(endpoint.AuthMode))
			if lanURL := deployEndpointLANURL(endpoint); lanURL != "" {
				fmt.Printf("     LAN: %s\n", lanURL)
			}
		}
	}
}

type deployEndpointPrintGroup struct {
	Machines  []string
	Endpoints []deployEndpointSummary
}

func groupedDeployEndpoints(machineOrder []string, endpointsByMachine map[string][]deployEndpointSummary) []deployEndpointPrintGroup {
	type endpointGroup struct {
		endpoint deployEndpointSummary
		machines []string
	}
	groupsByKey := map[string]*endpointGroup{}
	groupOrder := []string{}
	for _, machine := range machineOrder {
		for _, endpoint := range endpointsByMachine[machine] {
			key := strings.ToLower(strings.TrimSpace(endpoint.Hostname)) + "\x00" + strings.ToLower(strings.TrimSpace(endpoint.AuthMode))
			if key == "\x00" {
				continue
			}
			group := groupsByKey[key]
			if group == nil {
				group = &endpointGroup{endpoint: endpoint}
				groupsByKey[key] = group
				groupOrder = append(groupOrder, key)
			}
			if !stringSliceContains(group.machines, machine) {
				group.machines = append(group.machines, machine)
			}
		}
	}

	shared := []deployEndpointPrintGroup{}
	printedSharedKeys := map[string]bool{}
	for _, key := range groupOrder {
		group := groupsByKey[key]
		if group == nil || len(group.machines) <= 1 {
			continue
		}
		shared = append(shared, deployEndpointPrintGroup{
			Machines:  group.machines,
			Endpoints: []deployEndpointSummary{group.endpoint},
		})
		printedSharedKeys[key] = true
	}

	perMachine := []deployEndpointPrintGroup{}
	for _, machine := range machineOrder {
		endpoints := []deployEndpointSummary{}
		for _, endpoint := range endpointsByMachine[machine] {
			key := strings.ToLower(strings.TrimSpace(endpoint.Hostname)) + "\x00" + strings.ToLower(strings.TrimSpace(endpoint.AuthMode))
			if printedSharedKeys[key] {
				continue
			}
			endpoints = append(endpoints, endpoint)
		}
		if len(endpoints) == 0 {
			continue
		}
		perMachine = append(perMachine, deployEndpointPrintGroup{
			Machines:  []string{machine},
			Endpoints: endpoints,
		})
	}
	return append(shared, perMachine...)
}

func stringSliceContains(values []string, needle string) bool {
	for _, value := range values {
		if value == needle {
			return true
		}
	}
	return false
}

func printDeploySubdomains(subdomains []string) {
	cleaned := normalizeSubdomains(subdomains)
	if len(cleaned) == 0 {
		return
	}
	fmt.Println("endpoints:")
	for _, subdomain := range cleaned {
		fmt.Printf(" - %s\n", subdomain)
	}
}

func endpointAuthDescription(authMode string) string {
	switch strings.ToLower(strings.TrimSpace(authMode)) {
	case "login":
		return "🔒 Protected by Nethera login"
	case "token":
		return "🔑 API token required"
	default:
		return "🌐 Public - anyone with this URL can access"
	}
}

func deployEndpointLANURL(endpoint deployEndpointSummary) string {
	if !endpoint.PreferLAN {
		return ""
	}
	host := strings.TrimSpace(endpoint.LANHost)
	if host == "" || endpoint.LANPort <= 0 || endpoint.LANPort > 65535 {
		return ""
	}
	return (&url.URL{
		Scheme: "http",
		Host:   net.JoinHostPort(host, fmt.Sprintf("%d", endpoint.LANPort)),
	}).String()
}

func printEndpointAccessTokens(tokens []endpointAccessTokenResult) {
	if len(tokens) == 0 {
		return
	}
	created := []endpointAccessTokenResult{}
	existing := []endpointAccessTokenResult{}
	for _, token := range tokens {
		if token.Token != "" && token.Created {
			created = append(created, token)
		} else if token.AlreadyExists {
			existing = append(existing, token)
		}
	}
	if len(created) > 0 {
		fmt.Println("endpoint tokens:")
	}
	for _, token := range created {
		fmt.Printf(" - %s:\n", token.ServiceName)
		fmt.Printf("   %s\n", token.Token)
	}
	if len(created) > 0 {
		fmt.Println("Store these tokens securely. Nethera cannot show them again.")
	}
	if len(existing) > 0 {
		fmt.Println("endpoint tokens already exist:")
		for _, token := range existing {
			name := strings.TrimSpace(token.Name)
			if name == "" {
				name = "API token for " + token.ServiceName
			}
			fmt.Printf(" - %s: existing active token \"%s\"\n", token.ServiceName, name)
		}
		fmt.Println("Use `neth endpoint token list` or the dashboard endpoint page to manage tokens.")
	}
}

func promptForEndpointAccessTokens(backendURL, token, appID string, serviceNames []string) ([]endpointAccessTokenResult, error) {
	created := []endpointAccessTokenResult{}
	for _, serviceName := range serviceNames {
		existing, err := listEndpointAccessTokens(backendURL, token, appID, serviceName)
		if err != nil {
			return nil, err
		}
		activeNames := []string{}
		for _, existingToken := range existing {
			if strings.TrimSpace(existingToken.RevokedAt) != "" {
				continue
			}
			if strings.TrimSpace(existingToken.ExpiresAt) != "" {
				if expiresAt, parseErr := time.Parse(time.RFC3339, existingToken.ExpiresAt); parseErr == nil && expiresAt.Before(time.Now()) {
					continue
				}
			}
			name := strings.TrimSpace(existingToken.Name)
			if name == "" {
				name = "API token for " + serviceName
			}
			activeNames = append(activeNames, name)
		}
		if len(activeNames) > 0 {
			fmt.Printf("endpoint token already exists for %s: %s\n", serviceName, strings.Join(activeNames, ", "))
			continue
		}

		fmt.Printf("The %s endpoint requires an API token for access.\n", serviceName)
		confirmed, confirmErr := promptYesNoDefaultNo("Create one now?")
		if confirmErr != nil {
			return nil, confirmErr
		}
		if !confirmed {
			fmt.Printf("No token created for %s. Create one later with `neth endpoint token create %s`.\n", serviceName, serviceName)
			continue
		}
		name := ""
		for strings.TrimSpace(name) == "" {
			var promptErr error
			name, promptErr = promptLine("Token name: ")
			if promptErr != nil {
				return nil, promptErr
			}
			if strings.TrimSpace(name) == "" {
				fmt.Println("token name is required")
			}
		}
		tokenResult, createErr := createEndpointAccessTokenForService(backendURL, token, appID, serviceName, name, "")
		if createErr != nil {
			return nil, createErr
		}
		created = append(created, *tokenResult)
	}
	return created, nil
}

func ensureAppRegistration(backendURL, token, appName string) (string, error) {
	requestBody := map[string]string{"app_name": appName}
	payload, err := json.Marshal(requestBody)
	if err != nil {
		return "", err
	}

	req, err := http.NewRequest(http.MethodPost, backendURL+"/apps/register", bytes.NewReader(payload))
	if err != nil {
		return "", err
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Content-Type", "application/json")

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		return "", fmt.Errorf("app registration failed with status %d: %s", resp.StatusCode, strings.TrimSpace(string(body)))
	}

	var result struct {
		ID string `json:"id"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return "", err
	}
	if strings.TrimSpace(result.ID) == "" {
		return "", fmt.Errorf("app registration returned an empty id")
	}
	return result.ID, nil
}

func validateSecretName(name string) error {
	if strings.TrimSpace(name) == "" {
		return fmt.Errorf("secret name is required")
	}
	if len(name) > 128 {
		return fmt.Errorf("secret name must be 128 characters or fewer")
	}
	if !secretNamePattern.MatchString(name) {
		return fmt.Errorf("secret name must start with A-Z or _ and contain only A-Z, 0-9, and _")
	}
	return nil
}

func resolveSecretAppContext(backendURL, token, appOverride string) (appReference, error) {
	if strings.TrimSpace(appOverride) != "" {
		return resolveRemoteApp(backendURL, token, appOverride)
	}
	manifestPath, err := findNetheraManifestUpward(".")
	if err == nil && manifestPath != "" {
		manifest, loadErr := loadManifest(manifestPath)
		if loadErr != nil {
			return appReference{}, loadErr
		}
		if strings.TrimSpace(manifest.AppID) != "" {
			return resolveRemoteApp(backendURL, token, manifest.AppID)
		}
		if strings.TrimSpace(manifest.AppName) != "" {
			appID, registerErr := ensureAppRegistration(backendURL, token, manifest.AppName)
			if registerErr != nil {
				return appReference{}, registerErr
			}
			manifest.AppID = appID
			if saveErr := saveManifest(manifestPath, manifest); saveErr != nil {
				return appReference{}, saveErr
			}
			return appReference{ID: appID, Name: manifest.AppName}, nil
		}
	}
	return appReference{}, fmt.Errorf("No Nethera app found in this directory.\nRun this command inside a directory with nethera.yml, or pass --app <app>.")
}

func resolveRemoteApp(backendURL, token, app string) (appReference, error) {
	req, err := http.NewRequest(http.MethodGet, strings.TrimRight(backendURL, "/")+"/api/apps/resolve?app="+url.QueryEscape(strings.TrimSpace(app)), nil)
	if err != nil {
		return appReference{}, err
	}
	req.Header.Set("Authorization", "Bearer "+token)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return appReference{}, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return appReference{}, fmt.Errorf("%s", formatHTTPError(resp, "failed to resolve app"))
	}
	var result appResolveResponse
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return appReference{}, err
	}
	if strings.TrimSpace(result.App.ID) == "" {
		return appReference{}, fmt.Errorf("app resolve returned an empty id")
	}
	if strings.TrimSpace(result.App.Name) == "" {
		result.App.Name = result.App.ID
	}
	return result.App, nil
}

func putAppSecret(backendURL, token, appID, name, value string) (*appSecretMetadata, error) {
	payload, err := json.Marshal(map[string]string{"value": value})
	if err != nil {
		return nil, err
	}
	req, err := http.NewRequest(http.MethodPut, strings.TrimRight(backendURL, "/")+"/api/apps/"+url.PathEscape(appID)+"/secrets/"+url.PathEscape(name), bytes.NewReader(payload))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("%s", formatHTTPError(resp, "secret save failed"))
	}
	var result appSecretMetadata
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return nil, err
	}
	return &result, nil
}

func listAppSecrets(backendURL, token, appID string) (*appSecretsResponse, error) {
	req, err := http.NewRequest(http.MethodGet, strings.TrimRight(backendURL, "/")+"/api/apps/"+url.PathEscape(appID)+"/secrets", nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+token)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("%s", formatHTTPError(resp, "secret list failed"))
	}
	var result appSecretsResponse
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return nil, err
	}
	return &result, nil
}

func revealAppSecret(backendURL, token, appID, name string) (*appSecretRevealResponse, error) {
	req, err := http.NewRequest(http.MethodGet, strings.TrimRight(backendURL, "/")+"/api/apps/"+url.PathEscape(appID)+"/secrets/"+url.PathEscape(name)+"/reveal", nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+token)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("%s", formatHTTPError(resp, "secret reveal failed"))
	}
	var result appSecretRevealResponse
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return nil, err
	}
	return &result, nil
}

func deleteAppSecret(backendURL, token, appID, name string) error {
	req, err := http.NewRequest(http.MethodDelete, strings.TrimRight(backendURL, "/")+"/api/apps/"+url.PathEscape(appID)+"/secrets/"+url.PathEscape(name), nil)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+token)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("%s", formatHTTPError(resp, "secret delete failed"))
	}
	return nil
}

func progressLabel(action, target string) string {
	title := strings.Title(action)
	if title == "" {
		title = "Working"
	}
	return fmt.Sprintf("%s on %s", title, target)
}

func formatDeployLogs(raw string) []string {
	trimmed := strings.TrimSpace(raw)
	if trimmed == "" || trimmed == "[]" {
		return nil
	}

	var lines []string
	if err := json.Unmarshal([]byte(trimmed), &lines); err == nil {
		out := make([]string, 0, len(lines))
		for _, line := range lines {
			line = strings.TrimSpace(line)
			if line == "" {
				continue
			}
			out = append(out, line)
		}
		return out
	}

	return []string{trimmed}
}

type failureSummary struct {
	Reason string
	Docker string
}

func printActionFailure(action string, lines []string) {
	summary := summarizeFailure(lines)
	title := "Operation"
	if action == "deploy" || action == "apply" {
		title = "Deployment"
	} else if action == "destroy" {
		title = "Destroy"
	}

	fmt.Printf("\n✗ %s failed\n\n", title)
	if strings.TrimSpace(summary.Reason) != "" {
		fmt.Println("Reason:")
		fmt.Println(summary.Reason)
		fmt.Println()
	}
	fmt.Println("Docker:")
	fmt.Println(summary.Docker)
	fmt.Println()
	if action == "destroy" {
		fmt.Println("Run `neth destroy --verbose` for full destroy logs.")
	} else {
		fmt.Println("Run `neth deploy --verbose` for full deployment logs.")
	}
}

func printActionSuccess(action string) {
	title := "Operation"
	if action == "deploy" {
		title = "Deployment"
	} else if action == "destroy" {
		title = "Destroy"
	}
	fmt.Printf("\n✓ %s succeeded\n", title)
}

func summarizeFailure(lines []string) failureSummary {
	platformLine := extractPrimaryPlatformError(lines)
	dockerLine := extractPrimaryDockerError(lines)
	lower := strings.ToLower(dockerLine)

	summary := failureSummary{Docker: dockerLine}
	if platformLine != "" {
		summary.Reason = platformLine
		if summary.Docker == "" || !strings.Contains(strings.ToLower(summary.Docker), "error") {
			summary.Docker = "No Docker error reported."
		}
		return summary
	}
	switch {
	case strings.Contains(lower, "port is already allocated") || strings.Contains(lower, "bind for 0.0.0.0"):
		summary.Reason = extractPortReason(dockerLine)
	case strings.Contains(lower, "pull access denied") || strings.Contains(lower, "error pulling image") || strings.Contains(lower, "requested access to the resource is denied"):
		summary.Reason = "Image pull failed on the target machine."
	case strings.Contains(lower, "manifest unknown") || strings.Contains(lower, "not found") || strings.Contains(lower, "no such image"):
		summary.Reason = "The requested container image could not be found."
	case strings.Contains(lower, "container exited") || strings.Contains(lower, "exited with code") || strings.Contains(lower, "is restarting"):
		summary.Reason = "A container started but exited immediately."
	case strings.Contains(lower, "variable is not set") || strings.Contains(lower, "environment variable") || strings.Contains(lower, "defaulting to a blank string"):
		summary.Reason = "A required environment variable is missing for this compose application."
	case strings.Contains(lower, "compose file is invalid") || strings.Contains(lower, "validating") || strings.Contains(lower, "yaml") || strings.Contains(lower, "empty compose file"):
		summary.Reason = "The compose file is invalid for this deployment."
	case strings.Contains(lower, "cannot connect to the docker daemon") || strings.Contains(lower, "is the docker daemon running") || strings.Contains(lower, "error during connect") || strings.Contains(lower, "docker daemon"):
		summary.Reason = "Docker is unavailable on the target machine."
	default:
		summary.Reason = ""
	}

	if summary.Docker == "" {
		summary.Docker = firstNonEmpty(lines)
	}
	return summary
}

func extractPrimaryPlatformError(lines []string) string {
	for _, line := range lines {
		trimmed := strings.TrimSpace(line)
		if trimmed == "" || strings.HasPrefix(trimmed, "docker output:") {
			continue
		}
		lower := strings.ToLower(trimmed)
		if strings.Contains(lower, "public endpoint port") ||
			strings.Contains(lower, "endpoint") && strings.Contains(lower, "already used") ||
			strings.Contains(lower, "plan") && strings.Contains(lower, "allows") ||
			strings.Contains(lower, "upgrade") ||
			strings.Contains(lower, "route capacity") ||
			strings.Contains(lower, "agent reported an invalid endpoint") {
			return trimmed
		}
	}
	return ""
}

func extractPrimaryDockerError(lines []string) string {
	for index := len(lines) - 1; index >= 0; index -= 1 {
		trimmed := strings.TrimSpace(lines[index])
		if trimmed == "" {
			continue
		}
		if strings.HasPrefix(trimmed, "docker output:") {
			body := strings.TrimSpace(strings.TrimPrefix(trimmed, "docker output:"))
			lower := strings.ToLower(body)
			if strings.Contains(lower, "error response from daemon") ||
				strings.Contains(lower, "failed") ||
				strings.Contains(lower, "denied") ||
				strings.Contains(lower, "not found") ||
				strings.Contains(lower, "invalid") ||
				strings.Contains(lower, "pull access denied") ||
				strings.Contains(lower, "empty compose file") {
				return body
			}
		}
	}

	for _, line := range lines {
		trimmed := strings.TrimSpace(line)
		if trimmed == "" {
			continue
		}
		if strings.HasPrefix(trimmed, "docker output:") {
			body := strings.TrimSpace(strings.TrimPrefix(trimmed, "docker output:"))
			lower := strings.ToLower(body)
			if strings.Contains(lower, "error") || strings.Contains(lower, "failed") || strings.Contains(lower, "denied") || strings.Contains(lower, "not found") || strings.Contains(lower, "invalid") || strings.Contains(lower, "empty compose file") {
				return body
			}
		}
	}
	for _, line := range lines {
		trimmed := strings.TrimSpace(line)
		if strings.HasPrefix(trimmed, "docker output:") {
			return strings.TrimSpace(strings.TrimPrefix(trimmed, "docker output:"))
		}
	}
	for _, line := range lines {
		trimmed := strings.TrimSpace(line)
		if trimmed == "" {
			continue
		}
		if strings.HasPrefix(trimmed, "project:") || strings.HasPrefix(trimmed, "app:") || strings.HasPrefix(trimmed, "action:") || strings.HasPrefix(trimmed, "running:") || strings.HasPrefix(trimmed, "docker compose exit code:") {
			continue
		}
		return trimmed
	}
	return "deployment failed"
}

func extractPortReason(dockerLine string) string {
	const marker = "bind for "
	lower := strings.ToLower(dockerLine)
	index := strings.Index(lower, marker)
	if index == -1 {
		return "A required host port is already in use on the target machine."
	}
	remainder := dockerLine[index+len(marker):]
	port := remainder
	if end := strings.Index(port, " failed"); end != -1 {
		port = port[:end]
	}
	port = strings.TrimSpace(port)
	if port == "" {
		return "A required host port is already in use on the target machine."
	}
	return fmt.Sprintf("Port %s is already in use on the target machine.", port)
}

func firstNonEmpty(lines []string) string {
	for _, line := range lines {
		trimmed := strings.TrimSpace(line)
		if trimmed != "" {
			return trimmed
		}
	}
	return "deployment failed"
}

func fetchJob(backendURL, jobID, token string) (*deployJob, error) {
	req, err := http.NewRequest(http.MethodGet, backendURL+"/deploy/"+jobID, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+token)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("unexpected status %d", resp.StatusCode)
	}
	var job deployJob
	if err := json.NewDecoder(resp.Body).Decode(&job); err != nil {
		return nil, err
	}
	return &job, nil
}

func fetchDeploymentEndpoints(backendURL, token, appID, machineName string) ([]deploymentEndpoint, error) {
	queryURL := fmt.Sprintf("%s/deploy/endpoints?appId=%s&machineName=%s", backendURL, url.QueryEscape(appID), url.QueryEscape(machineName))
	req, err := http.NewRequest(http.MethodGet, queryURL, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+token)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("%s", formatEndpointInspectionError(resp, machineName))
	}
	var payload struct {
		Endpoints []deploymentEndpoint `json:"endpoints"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&payload); err != nil {
		return nil, err
	}
	return payload.Endpoints, nil
}

func fetchDeploymentTargets(backendURL, token, appID string) ([]deploymentTarget, error) {
	queryURL := fmt.Sprintf("%s/deploy/targets?appId=%s", backendURL, url.QueryEscape(appID))
	req, err := http.NewRequest(http.MethodGet, queryURL, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+token)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("%s", formatHTTPError(resp, "failed to inspect current deployment targets"))
	}
	var payload deploymentTargetsResponse
	if err := json.NewDecoder(resp.Body).Decode(&payload); err != nil {
		return nil, err
	}
	return payload.Targets, nil
}

func fetchActiveDeployJob(backendURL, token, appID, machineName string) (*activeDeployJob, error) {
	queryURL := fmt.Sprintf("%s/deploy/active?appId=%s&machineName=%s", strings.TrimRight(backendURL, "/"), url.QueryEscape(appID), url.QueryEscape(machineName))
	req, err := http.NewRequest(http.MethodGet, queryURL, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+token)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("%s", formatHTTPError(resp, "failed to inspect active deployment"))
	}
	var payload activeDeployJobResponse
	if err := json.NewDecoder(resp.Body).Decode(&payload); err != nil {
		return nil, err
	}
	return payload.Job, nil
}

func formatEndpointInspectionError(resp *http.Response, machineName string) string {
	if resp.StatusCode == http.StatusNotFound {
		return fmt.Sprintf("nethera.yml references machine %q, but that machine is not registered to your account. Run `neth machine list` to see registered machines.", machineName)
	}
	return formatHTTPError(resp, "failed to inspect current endpoints")
}

func formatHTTPError(resp *http.Response, fallback string) string {
	body, err := io.ReadAll(resp.Body)
	if err == nil {
		var apiError apiErrorResponse
		if json.Unmarshal(body, &apiError) == nil {
			message := strings.TrimSpace(apiError.Error)
			if message != "" {
				return message
			}
		}
	}
	return fmt.Sprintf("%s: unexpected status %d", fallback, resp.StatusCode)
}

func extractPublicServiceNames(composeYAML string) (map[string]bool, error) {
	var payload map[string]interface{}
	if err := yaml.Unmarshal([]byte(composeYAML), &payload); err != nil {
		return nil, err
	}
	servicesValue, ok := payload["services"]
	if !ok {
		return map[string]bool{}, nil
	}
	services, ok := servicesValue.(map[string]interface{})
	if !ok {
		return nil, fmt.Errorf("services must be a mapping")
	}
	publicServices := map[string]bool{}
	for name, raw := range services {
		serviceMap, ok := raw.(map[string]interface{})
		if !ok {
			continue
		}
		netheraValue := serviceMap["nethera"]
		netheraMap, _ := netheraValue.(map[string]interface{})
		publicValue, hasPublic := netheraMap["public"]
		if hasPublic && isPublicEnabled(publicValue) {
			publicServices[name] = true
		}
	}
	return publicServices, nil
}

func extractTokenAuthServiceNames(composeYAML string) ([]string, error) {
	var payload map[string]interface{}
	if err := yaml.Unmarshal([]byte(composeYAML), &payload); err != nil {
		return nil, err
	}
	servicesValue, ok := payload["services"]
	if !ok {
		return nil, nil
	}
	services, ok := servicesValue.(map[string]interface{})
	if !ok {
		return nil, fmt.Errorf("services must be a mapping")
	}
	serviceNames := []string{}
	for name, raw := range services {
		serviceMap, ok := raw.(map[string]interface{})
		if !ok {
			continue
		}
		netheraMap, _ := serviceMap["nethera"].(map[string]interface{})
		if !isPublicEnabled(netheraMap["public"]) {
			continue
		}
		authMode := strings.TrimSpace(fmt.Sprint(netheraMap["auth"]))
		if strings.EqualFold(authMode, "token") {
			serviceNames = append(serviceNames, name)
		}
	}
	sort.Strings(serviceNames)
	return serviceNames, nil
}

func isPublicEnabled(value interface{}) bool {
	switch typed := value.(type) {
	case bool:
		return typed
	case int, int64, float64:
		return true
	case string:
		trimmed := strings.TrimSpace(typed)
		return trimmed != "" && !strings.EqualFold(trimmed, "false")
	case []interface{}:
		return len(typed) > 0
	case map[string]interface{}:
		return true
	default:
		return false
	}
}

func writableVolumeWarnings(composeYAML string) ([]string, error) {
	var root yaml.Node
	if err := yaml.Unmarshal([]byte(composeYAML), &root); err != nil {
		return nil, err
	}
	if len(root.Content) == 0 || root.Content[0].Kind != yaml.MappingNode {
		return nil, fmt.Errorf("compose yaml must be a mapping")
	}
	servicesNode := yamlNodeMappingValue(root.Content[0], "services")
	if servicesNode == nil {
		return nil, nil
	}
	if servicesNode.Kind != yaml.MappingNode {
		return nil, fmt.Errorf("services must be a mapping")
	}
	warnings := []string{}
	for index := 0; index+1 < len(servicesNode.Content); index += 2 {
		serviceName := strings.TrimSpace(servicesNode.Content[index].Value)
		serviceNode := servicesNode.Content[index+1]
		if serviceName == "" || serviceNode.Kind != yaml.MappingNode {
			continue
		}
		volumesNode := yamlNodeMappingValue(serviceNode, "volumes")
		if volumesNode == nil {
			continue
		}
		if volumesNode.Kind != yaml.SequenceNode {
			return nil, fmt.Errorf("service %s volumes must be a list", serviceName)
		}
		for _, volumeNode := range volumesNode.Content {
			label, writable := describeWritableVolume(volumeNode)
			if writable {
				warnings = append(warnings, fmt.Sprintf("%s volume %s is writable", serviceName, label))
			}
		}
	}
	return warnings, nil
}

func describeWritableVolume(node *yaml.Node) (string, bool) {
	switch node.Kind {
	case yaml.ScalarNode:
		raw := strings.TrimSpace(node.Value)
		if raw == "" {
			return "", false
		}
		parts := strings.Split(raw, ":")
		if len(parts) >= 3 && strings.EqualFold(parts[len(parts)-1], "ro") {
			return raw, false
		}
		if len(parts) >= 2 {
			return raw, true
		}
	case yaml.MappingNode:
		readOnlyNode := yamlNodeMappingValue(node, "read_only")
		if readOnlyNode == nil {
			readOnlyNode = yamlNodeMappingValue(node, "readonly")
		}
		if readOnlyNode != nil && strings.EqualFold(strings.TrimSpace(readOnlyNode.Value), "true") {
			return "", false
		}
		typeNode := yamlNodeMappingValue(node, "type")
		volumeType := strings.TrimSpace(typeNodeValue(typeNode))
		if volumeType == "tmpfs" {
			return "", false
		}
		sourceNode := yamlNodeMappingValue(node, "source")
		if sourceNode == nil {
			sourceNode = yamlNodeMappingValue(node, "src")
		}
		targetNode := yamlNodeMappingValue(node, "target")
		if targetNode == nil {
			targetNode = yamlNodeMappingValue(node, "dst")
		}
		source := typeNodeValue(sourceNode)
		target := typeNodeValue(targetNode)
		label := strings.TrimSpace(source + ":" + target)
		if label == ":" {
			label = volumeType
		}
		return label, volumeType == "bind" || volumeType == "volume" || volumeType == ""
	}
	return "", false
}

func typeNodeValue(node *yaml.Node) string {
	if node == nil {
		return ""
	}
	return strings.TrimSpace(node.Value)
}

func yamlNodeMappingValue(node *yaml.Node, key string) *yaml.Node {
	if node == nil || node.Kind != yaml.MappingNode {
		return nil
	}
	for index := 0; index+1 < len(node.Content); index += 2 {
		if node.Content[index].Value == key {
			return node.Content[index+1]
		}
	}
	return nil
}

type serviceFailureSummary struct {
	ServiceName    string
	WebhookSecrets []string
	Emails         []string
	After          string
}

func extractServiceFailureSummaries(composeYAML string) ([]serviceFailureSummary, error) {
	var root yaml.Node
	if err := yaml.Unmarshal([]byte(composeYAML), &root); err != nil {
		return nil, err
	}
	if len(root.Content) == 0 || root.Content[0].Kind != yaml.MappingNode {
		return nil, nil
	}
	servicesNode := yamlNodeMappingValue(root.Content[0], "services")
	if servicesNode == nil {
		return nil, nil
	}
	if servicesNode.Kind != yaml.MappingNode {
		return nil, fmt.Errorf("services must be a mapping")
	}
	summaries := []serviceFailureSummary{}
	for index := 0; index+1 < len(servicesNode.Content); index += 2 {
		serviceName := strings.TrimSpace(servicesNode.Content[index].Value)
		serviceNode := servicesNode.Content[index+1]
		if serviceName == "" || serviceNode.Kind != yaml.MappingNode {
			continue
		}
		netheraNode := yamlNodeMappingValue(serviceNode, "nethera")
		if netheraNode == nil || netheraNode.Kind != yaml.MappingNode {
			continue
		}
		serviceFailureNode := yamlNodeMappingValue(netheraNode, "onServiceFailure")
		if serviceFailureNode == nil || serviceFailureNode.Kind != yaml.MappingNode {
			continue
		}
		webhookSecrets := yamlStringList(yamlNodeMappingValue(serviceFailureNode, "webhookSecrets"))
		emails := yamlStringList(yamlNodeMappingValue(serviceFailureNode, "emails"))
		if len(emails) == 0 {
			emails = []string{"owners"}
		}
		after := typeNodeValue(yamlNodeMappingValue(serviceFailureNode, "after"))
		if after == "" {
			after = "60s"
		}
		summaries = append(summaries, serviceFailureSummary{ServiceName: serviceName, WebhookSecrets: webhookSecrets, Emails: emails, After: after})
	}
	return summaries, nil
}

func yamlStringList(node *yaml.Node) []string {
	if node == nil {
		return nil
	}
	if node.Kind == yaml.ScalarNode {
		value := strings.TrimSpace(node.Value)
		if value == "" {
			return nil
		}
		return []string{value}
	}
	if node.Kind != yaml.SequenceNode {
		return nil
	}
	values := []string{}
	for _, item := range node.Content {
		value := strings.TrimSpace(item.Value)
		if value != "" {
			values = append(values, value)
		}
	}
	return values
}

func endpointsScheduledForRemoval(existing []deploymentEndpoint, desiredPublicServices map[string]bool) []deploymentEndpoint {
	removed := make([]deploymentEndpoint, 0)
	for _, endpoint := range existing {
		if !desiredPublicServices[endpoint.ServiceName] {
			removed = append(removed, endpoint)
		}
	}
	return removed
}
