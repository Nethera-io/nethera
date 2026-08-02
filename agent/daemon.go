package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"sync/atomic"
	"syscall"
	"time"
)

type localDeployLogSink struct {
	remote             deployLogSink
	lastProgressLogAt  time.Time
	progressLogSpacing time.Duration
}

func newLocalDeployLogSink(remote deployLogSink) *localDeployLogSink {
	return &localDeployLogSink{remote: remote, progressLogSpacing: 5 * time.Second}
}

func (sink *localDeployLogSink) Emit(stream, line string) {
	line = strings.TrimSpace(line)
	if line == "" {
		return
	}
	if sink.remote != nil {
		sink.remote(stream, line)
	}
	if isHighFrequencyDockerProgressLine(line) {
		now := time.Now()
		if !sink.lastProgressLogAt.IsZero() && now.Sub(sink.lastProgressLogAt) < sink.progressLogSpacing {
			return
		}
		sink.lastProgressLogAt = now
	}
	stream = strings.TrimSpace(stream)
	if stream == "" {
		stream = "deploy"
	}
	fmt.Printf("deploy %s: %s\n", stream, line)
}

func isHighFrequencyDockerProgressLine(line string) bool {
	return strings.Contains(line, "Downloading [") ||
		strings.Contains(line, "Extracting [") ||
		strings.Contains(line, "Pulling fs layer") ||
		strings.Contains(line, "Waiting") ||
		strings.Contains(line, "Verifying Checksum")
}

func runDaemon(args []string) {
	fs := flag.NewFlagSet("daemon", flag.ExitOnError)
	backendURL := fs.String("backend", defaultBackendURL(), "backend base URL")
	interval := fs.Duration("interval", 5*time.Second, "poll interval")
	configPath := fs.String("config", defaultMachineConfigPath(), "machine config path")
	waitTimeout := fs.Duration("timeout", 5*time.Minute, "maximum time to wait for pairing")
	regionCode := fs.String("region", defaultRegionCode(), "region code")
	autoRepairAuth := fs.Bool("auto-repair-auth", defaultAutoRepairAuth(), "automatically re-pair if stored machine credentials are rejected")
	fs.Parse(args)

	if err := ensureWireGuardPrivileges(); err != nil {
		fmt.Printf("startup check failed: %v\n", err)
		os.Exit(1)
	}

	releaseLock, err := acquireDaemonLock()
	if err != nil {
		fmt.Printf("failed to start daemon: %v\n", err)
		os.Exit(1)
	}
	defer releaseLock()

	machineCreds, err := waitForMachineConfig(*configPath, *interval)
	if err != nil {
		fmt.Printf("failed to prepare machine credentials: %v\n", err)
		os.Exit(1)
	}

	var networkConfig *wireGuardNetworkResponse
	forcePairMachineAuth := func(source string, reason string) bool {
		fmt.Printf("machine auth rejected by %s; %s\n", source, reason)
		if _, pairErr := ensureMachinePairing(*backendURL, *configPath, *waitTimeout, true, *regionCode); pairErr != nil {
			fmt.Printf("failed to recover pairing: %v\n", pairErr)
			return false
		}
		reloaded, loadErr := loadMachineConfig(*configPath)
		if loadErr != nil {
			fmt.Printf("failed to reload machine config: %v\n", loadErr)
			return false
		}
		machineCreds = reloaded
		if reloadedNetworkConfig, provisionErr := ensureWireGuardProvisioning(*backendURL, machineCreds.MachineToken); provisionErr != nil {
			fmt.Printf("wireguard reprovision failed: %v\n", provisionErr)
			return false
		} else {
			networkConfig = reloadedNetworkConfig
		}
		fmt.Println("wireguard reprovision completed")
		fmt.Printf("pairing recovered; now using machine %s\n", machineCreds.MachineID)
		return true
	}
	repairMachineAuth := func(source string) bool {
		if !*autoRepairAuth {
			fmt.Printf("machine credentials rejected by %s; restart the Nethera agent service to generate a new pairing code\n", source)
			return false
		}
		return forcePairMachineAuth(source, "restarting pairing because auto auth repair is enabled")
	}

	networkConfig, _, err = reconcileWireGuardProvisioning(*backendURL, machineCreds.MachineToken, networkConfig)
	if err != nil {
		if isUnauthorizedError(err) {
			if !forcePairMachineAuth("api/machines/network", "starting pairing for this restarted service") {
				fmt.Printf("wireguard provisioning failed: %v\n", err)
				os.Exit(1)
			}
		} else {
			fmt.Printf("wireguard provisioning failed: %v\n", err)
		}
	} else {
		fmt.Println("wireguard provisioning completed")
	}

	fmt.Printf("using stored machine credentials from %s\n", *configPath)
	if _, err := resolveDockerBinary(); err != nil {
		fmt.Printf("startup check failed: %v\n", err)
		os.Exit(1)
	}
	fmt.Println("docker runtime available")
	machineWireGuardIP := ""
	if networkConfig != nil {
		machineWireGuardIP = wireGuardIPFromAddress(networkConfig.Interface.Address)
	}
	fmt.Printf("machine process started for %s; polling for deploy jobs\n", machineCreds.MachineID)
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	nextPoll := *interval
	activeLogStreams := map[string]bool{}
	activeCopySessions := map[string]bool{}
	var localDeploymentsReconciled atomic.Bool
	var deploymentReconcileRunning atomic.Bool
	lastDeploymentSelfHealAt := time.Time{}
	localDeploymentReconcileSkipState := ""
	networkProvisioningSkipReason := ""
	for {
		select {
		case <-ctx.Done():
			fmt.Println("machine process stopping")
			return
		case <-time.After(nextPoll):
		}
		pollResponse, err := pollAgent(*backendURL, machineCreds.MachineToken, nil)
		if err != nil {
			if isUnauthorizedError(err) {
				if repairMachineAuth("api/agent/poll") {
					continue
				}
				fmt.Println("machine process stopping because stored machine credentials were rejected; restart the service to re-pair")
				return
			}
			fmt.Printf("poll error: %v\n", err)
			nextPoll = boundedBackoff(nextPoll)
			continue
		}
		nextPoll = durationFromPollAfter(pollResponse.PollAfterSeconds, *interval)
		if pollResponse.AgentUpdate.Available {
			shouldAttempt, delayReason := shouldAttemptAgentUpdate(pollResponse.AgentUpdate)
			if !shouldAttempt {
				if delayReason != "" {
					fmt.Println(delayReason)
				}
				continue
			}
			if err := performAgentUpdate(pollResponse.AgentUpdate); err != nil {
				fmt.Printf("Agent update failed: %s\n", shortUpdateError(err))
				continue
			}
			return
		}
		refreshedNetworkConfig, wireGuardChanged, wireGuardErr := reconcileWireGuardProvisioning(*backendURL, machineCreds.MachineToken, networkConfig)
		if wireGuardErr != nil {
			if isUnauthorizedError(wireGuardErr) {
				if repairMachineAuth("api/machines/network") {
					continue
				}
				fmt.Println("machine process stopping because stored machine credentials were rejected; restart the service to re-pair")
				return
			}
			skipReason := wireGuardErr.Error()
			if networkProvisioningSkipReason != skipReason {
				fmt.Printf("deployment work waiting for wireguard provisioning: %v\n", wireGuardErr)
				networkProvisioningSkipReason = skipReason
			}
			continue
		}
		networkConfig = refreshedNetworkConfig
		if wireGuardChanged {
			fmt.Println("wireguard reprovision completed")
		}
		networkProvisioningSkipReason = ""
		if networkConfig != nil {
			machineWireGuardIP = wireGuardIPFromAddress(networkConfig.Interface.Address)
		}
		if !localDeploymentsReconciled.Load() {
			if strings.TrimSpace(pollResponse.MachineState) != "" && pollResponse.MachineState != "active" {
				if localDeploymentReconcileSkipState != pollResponse.MachineState {
					fmt.Printf("deployment reconcile waiting because machine state is %s\n", pollResponse.MachineState)
					localDeploymentReconcileSkipState = pollResponse.MachineState
				}
			} else if deploymentReconcileRunning.CompareAndSwap(false, true) {
				startLocalDeploymentReconcile(ctx, "deployment reconcile", machineWireGuardIP, &deploymentReconcileRunning, func() {
					localDeploymentsReconciled.Store(true)
				})
			}
		}
		job := firstDeployJob(pollResponse.Jobs)
		if job == nil && localDeploymentsReconciled.Load() && (strings.TrimSpace(pollResponse.MachineState) == "" || pollResponse.MachineState == "active") {
			reconcileInterval := time.Duration(agentLogStreamEnvInt("DEPLOYMENT_SELF_HEAL_INTERVAL_SECONDS", 60)) * time.Second
			if reconcileInterval < 30*time.Second {
				reconcileInterval = 30 * time.Second
			}
			if !deploymentReconcileRunning.Load() && time.Since(lastDeploymentSelfHealAt) >= reconcileInterval && localDeploymentsNeedReconcile() {
				lastDeploymentSelfHealAt = time.Now()
				fmt.Println("deployment self-heal detected local drift; reconciling desired deployments")
				if deploymentReconcileRunning.CompareAndSwap(false, true) {
					startLocalDeploymentReconcile(ctx, "deployment self-heal", machineWireGuardIP, &deploymentReconcileRunning, nil)
				}
			}
		}
		for _, target := range pollResponse.LogStreamTargets {
			if strings.TrimSpace(target.TargetID) == "" || activeLogStreams[target.TargetID] {
				continue
			}
			activeLogStreams[target.TargetID] = true
			fmt.Printf("received log stream target %s for deployment %s\n", target.TargetID, target.DeploymentID)
			go func(target logStreamTargetPayload) {
				if err := streamLogTarget(ctx, *backendURL, machineCreds.MachineToken, machineCreds.MachineID, target); err != nil {
					fmt.Printf("log stream target %s failed: %v\n", target.TargetID, err)
				}
			}(target)
		}
		for _, copySession := range pollResponse.CopySessions {
			if strings.TrimSpace(copySession.ID) == "" || activeCopySessions[copySession.ID] {
				continue
			}
			activeCopySessions[copySession.ID] = true
			fmt.Printf("received copy session %s (%s)\n", copySession.ID, copySession.Operation)
			go handleCopySession(ctx, *backendURL, machineCreds.MachineToken, copySession)
		}
		if job == nil {
			continue
		}
		if deploymentReconcileRunning.Load() {
			fmt.Printf("received %s job %s; waiting for local deployment reconciliation to finish\n", job.Type, job.ID)
			continue
		}
		fmt.Printf("received %s job %s\n", job.Type, job.ID)
		jobCtx, cancelJob := context.WithTimeout(ctx, time.Duration(agentLogStreamEnvInt("DEPLOY_JOB_TIMEOUT_SECONDS", 2700))*time.Second)
		var cancellationRequested atomic.Bool
		stopJobHeartbeat := startActiveJobHeartbeat(ctx, *backendURL, machineCreds.MachineToken, job, durationFromPollAfter(pollResponse.PollAfterSeconds, *interval), func() {
			cancellationRequested.Store(true)
			cancelJob()
		})
		deployLogs := startDeployLogStreamer(ctx, *backendURL, machineCreds.MachineToken, job.ID)
		localDeployLogs := newLocalDeployLogSink(deployLogs.Emit)
		localDeployLogs.Emit("deploy", fmt.Sprintf("%s job started", job.Type))
		logLines, publicEndpoints, err := runJobWithContext(jobCtx, job, machineWireGuardIP, *backendURL, machineCreds.MachineToken, localDeployLogs.Emit)
		cancelJob()
		if err != nil {
			localDeployLogs.Emit("stderr", err.Error())
		} else {
			localDeployLogs.Emit("deploy", fmt.Sprintf("%s completed successfully", job.Type))
		}
		deployLogs.Close()
		stopJobHeartbeat()
		if cancellationRequested.Load() {
			logLines = append(logLines, "deployment cancelled by replacement request")
			fmt.Printf("deployment %s cancelled\n", job.ID)
			if completeErr := markJobComplete(*backendURL, job.ID, machineCreds.MachineToken, "cancelled", logLines, nil); completeErr != nil {
				fmt.Printf("failed to report cancelled job: %v\n", completeErr)
			}
			continue
		}
		if ctx.Err() != nil {
			fmt.Println("machine process stopping with deployment still resumable")
			return
		}
		if errors.Is(jobCtx.Err(), context.DeadlineExceeded) {
			err = fmt.Errorf("deployment exceeded the %s job timeout", time.Duration(agentLogStreamEnvInt("DEPLOY_JOB_TIMEOUT_SECONDS", 2700))*time.Second)
			logLines = append([]string{err.Error()}, logLines...)
		}
		if err != nil {
			fmt.Printf("deployment failed: %v\n", err)
			if len(logLines) > 0 {
				fmt.Println("--- deploy logs (failed) ---")
				for _, line := range logLines {
					fmt.Println(line)
				}
				fmt.Println("----------------------------")
			}
			if completeErr := markJobComplete(*backendURL, job.ID, machineCreds.MachineToken, "failed", append([]string{err.Error()}, logLines...), nil); completeErr != nil {
				if isUnauthorizedError(completeErr) {
					if job.Type == "deregister_machine" {
						fmt.Println("deregistration cleanup reported; stopping machine process")
						return
					}
					if repairMachineAuth("deploy/complete") {
						if retryErr := markJobComplete(*backendURL, job.ID, machineCreds.MachineToken, "failed", append([]string{err.Error()}, logLines...), nil); retryErr != nil {
							fmt.Printf("failed to update job after repair: %v\n", retryErr)
						}
					} else {
						fmt.Printf("failed to update job: %v\n", completeErr)
						fmt.Println("machine process stopping because stored machine credentials were rejected; restart the service to re-pair")
						return
					}
				} else {
					fmt.Printf("failed to update job: %v\n", completeErr)
				}
			}
			if job.Type == "deregister_machine" {
				fmt.Println("deregistration cleanup reported; stopping machine process")
				return
			}
			continue
		}
		fmt.Println("--- deploy logs ---")
		for _, line := range logLines {
			fmt.Println(line)
		}
		fmt.Println("------------------")
		fmt.Printf("reporting %d public endpoint(s)\n", len(publicEndpoints))
		if err := markJobComplete(*backendURL, job.ID, machineCreds.MachineToken, "succeeded", logLines, publicEndpoints); err != nil {
			if isUnauthorizedError(err) {
				if job.Type == "deregister_machine" {
					fmt.Println("deregistration cleanup completed; stopping machine process")
					return
				}
				if repairMachineAuth("deploy/complete") {
					if retryErr := markJobComplete(*backendURL, job.ID, machineCreds.MachineToken, "succeeded", logLines, publicEndpoints); retryErr != nil {
						fmt.Printf("failed to update job after repair: %v\n", retryErr)
					}
				} else {
					fmt.Printf("failed to update job: %v\n", err)
					fmt.Println("machine process stopping because stored machine credentials were rejected; restart the service to re-pair")
					return
				}
			} else {
				fmt.Printf("failed to update job: %v\n", err)
			}
		}
		if job.Type == "deregister_machine" {
			fmt.Println("deregistration cleanup completed; stopping machine process")
			return
		}
	}
}

func startActiveJobHeartbeat(ctx context.Context, backendURL string, token string, job *deployJob, interval time.Duration, onCancel func()) func() {
	if interval <= 0 {
		interval = 5 * time.Second
	}
	heartbeatCtx, cancel := context.WithCancel(ctx)
	done := make(chan struct{})
	go func() {
		defer close(done)
		ticker := time.NewTicker(interval)
		defer ticker.Stop()
		for {
			select {
			case <-heartbeatCtx.Done():
				return
			case <-ticker.C:
				response, err := pollAgent(backendURL, token, job)
				if err != nil {
					fmt.Printf("active job heartbeat failed: %v\n", err)
					continue
				}
				for _, jobID := range response.CancelJobIDs {
					if jobID == job.ID && onCancel != nil {
						onCancel()
						return
					}
				}
			}
		}
	}()
	return func() {
		cancel()
		<-done
	}
}

func startLocalDeploymentReconcile(ctx context.Context, label string, machineWireGuardIP string, running *atomic.Bool, onComplete func()) {
	go func() {
		defer running.Store(false)
		reconcileLogs, reconcileErr := reconcileLocalDeployments(machineWireGuardIP)
		if reconcileErr != nil {
			fmt.Printf("%s warning: %v\n", label, reconcileErr)
			for _, line := range reconcileLogs {
				fmt.Println(line)
			}
		} else if len(reconcileLogs) > 0 {
			fmt.Printf("--- %s logs ---\n", label)
			for _, line := range reconcileLogs {
				fmt.Println(line)
			}
			fmt.Println("----------------------")
		}
		if ctx.Err() == nil && onComplete != nil {
			onComplete()
		}
	}()
}

func isUnauthorizedError(err error) bool {
	var statusErr *httpStatusError
	return errors.As(err, &statusErr) && statusErr.Status == http.StatusUnauthorized
}

func waitForMachineConfig(configPath string, interval time.Duration) (machineConfig, error) {
	cfg, err := loadMachineConfig(configPath)
	if err != nil {
		return machineConfig{}, err
	}
	if cfg.MachineID != "" && cfg.MachineToken != "" {
		return cfg, nil
	}

	fmt.Println("Agent installed but not configured.")
	fmt.Println("Start pairing from this machine with:")
	fmt.Printf("  sudo nethera-agent enroll --config %s\n", configPath)

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	nextLog := time.Now().Add(5 * time.Minute)
	for {
		select {
		case <-ctx.Done():
			return machineConfig{}, fmt.Errorf("machine configuration was not found before shutdown")
		case <-time.After(interval):
		}
		cfg, err := loadMachineConfig(configPath)
		if err != nil {
			return machineConfig{}, err
		}
		if cfg.MachineID != "" && cfg.MachineToken != "" {
			fmt.Printf("loaded machine configuration from %s\n", configPath)
			return cfg, nil
		}
		if time.Now().After(nextLog) {
			fmt.Println("Agent is waiting for machine configuration.")
			nextLog = time.Now().Add(5 * time.Minute)
		}
	}
}
