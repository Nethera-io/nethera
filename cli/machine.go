package main

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strings"
)

func runMachine(args []string) {
	fs := flag.NewFlagSet("machine", flag.ExitOnError)
	backendURL := fs.String("backend", defaultBackendURL(), "backend base URL")
	regionCode := fs.String("region", "", "region code for machine pairing")
	cleanupDeployments := fs.Bool("cleanup-deployments", false, "also stop Nethera-managed Compose projects on the machine")
	cleanupWireGuard := fs.Bool("cleanup-wireguard", false, "also remove Nethera-managed WireGuard configuration if supported")
	force := fs.Bool("force", false, "skip confirmation prompt")
	fs.Parse(args)

	action := strings.TrimSpace(fs.Arg(0))
	if action == "select" {
		runTarget(fs.Args()[1:])
		return
	}
	if action == "list" || action == "stats" {
		token, err := loadAuthToken(*backendURL)
		if err != nil {
			fmt.Println(err)
			os.Exit(1)
		}

		machines, err := listMachines(*backendURL, token)
		if err != nil {
			fmt.Printf("failed to list machines: %v\n", err)
			os.Exit(1)
		}
		if action == "stats" {
			printMachineStats(machines)
		} else {
			printMachines(machines)
		}
		return
	}
	if action == "remove" {
		fmt.Println("`neth machine remove` has been removed because it was ambiguous.")
		fmt.Println("Use `neth machine disable <machine>` to stop management without deregistering.")
		fmt.Println("Use `neth machine deregister <machine>` to remove the machine from Nethera.")
		os.Exit(1)
	}
	if action == "deregister" {
		effectiveCleanupDeployments := *cleanupDeployments || argFlagPresent(args, "--cleanup-deployments")
		effectiveCleanupWireGuard := *cleanupWireGuard || argFlagPresent(args, "--cleanup-wireguard")
		effectiveForce := *force || argFlagPresent(args, "--force")
		machineRef := strings.TrimSpace(fs.Arg(1))
		if machineRef == "" {
			fmt.Println("usage: neth machine deregister <machine>")
			os.Exit(1)
		}
		token, err := loadAuthToken(*backendURL)
		if err != nil {
			fmt.Println(err)
			os.Exit(1)
		}
		machines, err := listMachines(*backendURL, token)
		if err != nil {
			fmt.Printf("failed to list machines: %v\n", err)
			os.Exit(1)
		}
		machine, err := resolveMachineReference(machines, machineRef)
		if err != nil {
			fmt.Println(err)
			os.Exit(1)
		}
		if !effectiveForce {
			confirmed, err := confirmMachineRemoval(machine.Name, effectiveCleanupDeployments, effectiveCleanupWireGuard)
			if err != nil {
				fmt.Printf("failed to read confirmation: %v\n", err)
				os.Exit(1)
			}
			if !confirmed {
				fmt.Println("machine removal cancelled")
				os.Exit(1)
			}
		}
		result, err := deregisterMachine(*backendURL, token, machine.ID, effectiveCleanupDeployments, effectiveCleanupWireGuard, false)
		if err != nil {
			var bandwidthErr *bandwidthLimitConfirmationError
			if errors.As(err, &bandwidthErr) {
				fmt.Println(bandwidthErr.Message)
				confirmed, confirmErr := promptYesNoDefaultNo("Deregister machine anyway?")
				if confirmErr != nil {
					fmt.Printf("failed to read confirmation: %v\n", confirmErr)
					os.Exit(1)
				}
				if !confirmed {
					fmt.Println("machine deregistration cancelled")
					os.Exit(1)
				}
				result, err = deregisterMachine(*backendURL, token, machine.ID, effectiveCleanupDeployments, effectiveCleanupWireGuard, true)
				if err != nil {
					fmt.Printf("failed to remove machine: %v\n", err)
					os.Exit(1)
				}
			} else {
				fmt.Printf("failed to remove machine: %v\n", err)
				os.Exit(1)
			}
		}
		fmt.Printf("Machine %q removal state: %s\n", result.Machine.Name, result.Machine.LifecycleStatus)
		if result.CleanupJobID != "" {
			fmt.Printf("Cleanup job queued: %s\n", result.CleanupJobID)
		}
		return
	}
	if action == "enable" || action == "disable" {
		effectiveForce := *force || argFlagPresent(args, "--force")
		machineRef := strings.TrimSpace(fs.Arg(1))
		if machineRef == "" {
			fmt.Printf("usage: neth machine %s <machine>\n", action)
			os.Exit(1)
		}
		token, err := loadAuthToken(*backendURL)
		if err != nil {
			fmt.Println(err)
			os.Exit(1)
		}
		machines, err := listMachines(*backendURL, token)
		if err != nil {
			fmt.Printf("failed to list machines: %v\n", err)
			os.Exit(1)
		}
		machine, err := resolveMachineReference(machines, machineRef)
		if err != nil {
			fmt.Println(err)
			os.Exit(1)
		}
		if action == "disable" && !effectiveForce {
			confirmed, err := confirmMachineDisable(machine.Name)
			if err != nil {
				fmt.Printf("failed to read confirmation: %v\n", err)
				os.Exit(1)
			}
			if !confirmed {
				fmt.Println("machine disable cancelled")
				os.Exit(1)
			}
		}
		if err := updateMachineManagement(*backendURL, token, machine.ID, action == "enable", false); err != nil {
			var bandwidthErr *bandwidthLimitConfirmationError
			if action == "disable" && errors.As(err, &bandwidthErr) {
				fmt.Println(bandwidthErr.Message)
				confirmed, confirmErr := promptYesNoDefaultNo("Disable management anyway?")
				if confirmErr != nil {
					fmt.Printf("failed to read confirmation: %v\n", confirmErr)
					os.Exit(1)
				}
				if !confirmed {
					fmt.Println("machine disable cancelled")
					os.Exit(1)
				}
				if retryErr := updateMachineManagement(*backendURL, token, machine.ID, false, true); retryErr != nil {
					fmt.Println(retryErr)
					os.Exit(1)
				}
			} else {
				fmt.Println(err)
				os.Exit(1)
			}
		}
		if action == "enable" {
			fmt.Printf("Management enabled for %s.\n", machine.Name)
		} else {
			fmt.Printf("Management disabled for %s.\n", machine.Name)
			fmt.Println("Existing containers were not stopped.")
		}
		return
	}
	if action != "pair" {
		fmt.Println("usage: neth machine list")
		fmt.Println("       neth machine stats")
		fmt.Println("       neth machine enable <machine>")
		fmt.Println("       neth machine disable <machine>")
		fmt.Println("       neth machine deregister <machine> [--cleanup-deployments] [--cleanup-wireguard]")
		fmt.Println("       neth machine pair <pair-code>")
		fmt.Println("       neth machine select [dir]")
		os.Exit(1)
	}
	pairCode := strings.TrimSpace(fs.Arg(1))
	if pairCode == "" {
		fmt.Print("Pair code: ")
		reader := bufio.NewReader(os.Stdin)
		value, err := reader.ReadString('\n')
		if err != nil {
			fmt.Printf("failed to read pair code: %v\n", err)
			os.Exit(1)
		}
		pairCode = strings.TrimSpace(value)
	}
	if pairCode == "" {
		fmt.Println("pair code is required")
		os.Exit(1)
	}

	machineName := strings.TrimSpace(fs.Arg(2))

	token, err := loadAuthToken(*backendURL)
	if err != nil {
		fmt.Println(err)
		os.Exit(1)
	}
	// The agent selects the default region because its network path is the one
	// that matters. Only send a region from the CLI when the user explicitly
	// overrides it with --region.
	selectedRegionCode := strings.TrimSpace(*regionCode)

	result, statusCode, errMsg, err := requestMachinePairing(*backendURL, token, pairCode, machineName, selectedRegionCode)
	if err != nil {
		fmt.Printf("failed to request enrollment token: %v\n", err)
		os.Exit(1)
	}
	if statusCode == http.StatusBadRequest && strings.Contains(strings.ToLower(errMsg), "machinename is required") && machineName == "" {
		fmt.Print("Machine name: ")
		reader := bufio.NewReader(os.Stdin)
		value, readErr := reader.ReadString('\n')
		if readErr != nil {
			fmt.Printf("failed to read machine name: %v\n", readErr)
			os.Exit(1)
		}
		machineName = strings.TrimSpace(value)
		if machineName == "" {
			fmt.Println("machine name is required")
			os.Exit(1)
		}

		result, statusCode, errMsg, err = requestMachinePairing(*backendURL, token, pairCode, machineName, selectedRegionCode)
		if err != nil {
			fmt.Printf("failed to request enrollment token: %v\n", err)
			os.Exit(1)
		}
	}
	if statusCode != http.StatusOK {
		fmt.Println(errMsg)
		os.Exit(1)
	}

	if strings.TrimSpace(result.Message) != "" {
		fmt.Println(result.Message)
	} else {
		fmt.Println("Machine paired successfully.")
	}
}

func requestMachinePairing(backendURL, token, pairCode, machineName string, regionCode string) (pairResponse, int, string, error) {
	requestBody := map[string]string{"pairCode": strings.ToUpper(pairCode)}
	if strings.TrimSpace(machineName) != "" {
		requestBody["machineName"] = strings.TrimSpace(machineName)
	}
	if strings.TrimSpace(regionCode) != "" {
		requestBody["regionCode"] = strings.TrimSpace(regionCode)
	}

	payload, err := json.Marshal(requestBody)
	if err != nil {
		return pairResponse{}, 0, "", err
	}

	req, err := http.NewRequest(http.MethodPost, backendURL+"/api/machines/pair", bytes.NewReader(payload))
	if err != nil {
		return pairResponse{}, 0, "", err
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Content-Type", "application/json")

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return pairResponse{}, 0, "", err
	}
	defer resp.Body.Close()

	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK {
		var errorBody struct {
			Error string `json:"error"`
		}
		if json.Unmarshal(body, &errorBody) == nil && strings.TrimSpace(errorBody.Error) != "" {
			return pairResponse{}, resp.StatusCode, errorBody.Error, nil
		}
		return pairResponse{}, resp.StatusCode, fmt.Sprintf("pairing request failed with status %d", resp.StatusCode), nil
	}

	var result pairResponse
	if err := json.Unmarshal(body, &result); err != nil {
		return pairResponse{}, 0, "", fmt.Errorf("failed to decode pairing response: %w", err)
	}
	return result, resp.StatusCode, "", nil
}

func resolveMachineReference(machines []machineSummary, ref string) (machineSummary, error) {
	var matches []machineSummary
	for _, machine := range machines {
		if machine.ID == ref || strings.EqualFold(machine.Name, ref) {
			matches = append(matches, machine)
		}
	}
	if len(matches) == 0 {
		return machineSummary{}, fmt.Errorf("machine %q not found", ref)
	}
	if len(matches) > 1 {
		return machineSummary{}, fmt.Errorf("machine %q is ambiguous; use the machine id", ref)
	}
	return matches[0], nil
}

func confirmMachineRemoval(machineName string, cleanupDeployments bool, cleanupWireGuard bool) (bool, error) {
	fmt.Printf("This will remove machine %q from Nethera and disable its public endpoints.\n", machineName)
	if cleanupDeployments {
		fmt.Println("Nethera will ask the agent to stop Nethera-managed Compose projects on this machine.")
		fmt.Println("Non-Nethera Docker containers will not be touched.")
	} else {
		fmt.Println("Local containers will keep running.")
	}
	if cleanupWireGuard {
		fmt.Println("Nethera will also request WireGuard cleanup if the agent supports it.")
	}
	fmt.Print("Continue? [y/N] ")
	reader := bufio.NewReader(os.Stdin)
	value, err := reader.ReadString('\n')
	if err != nil {
		return false, err
	}
	normalized := strings.TrimSpace(strings.ToLower(value))
	return normalized == "y" || normalized == "yes", nil
}

func confirmMachineDisable(machineName string) (bool, error) {
	fmt.Printf("This will disable Nethera management for machine %q.\n", machineName)
	fmt.Println("Public endpoints on this machine will stop routing and Nethera will not send new jobs while management is disabled.")
	fmt.Println("Existing containers will not be stopped.")
	return promptYesNoDefaultNo("Continue with disabling management?")
}

func deregisterMachine(backendURL, token, machineID string, cleanupDeployments bool, cleanupWireGuard bool, confirmBandwidthLimitChange bool) (*deregisterMachineResponse, error) {
	payload, err := json.Marshal(map[string]bool{
		"cleanupDeployments":          cleanupDeployments,
		"cleanupWireGuard":            cleanupWireGuard,
		"confirmBandwidthLimitChange": confirmBandwidthLimitChange,
	})
	if err != nil {
		return nil, err
	}
	req, err := http.NewRequest(http.MethodPost, strings.TrimRight(backendURL, "/")+"/api/machines/"+url.PathEscape(machineID)+"/deregister", bytes.NewReader(payload))
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
		body, _ := io.ReadAll(resp.Body)
		var errorBody struct {
			Error                string `json:"error"`
			Code                 string `json:"code"`
			ConfirmationRequired bool   `json:"confirmationRequired"`
		}
		if json.Unmarshal(body, &errorBody) == nil && errorBody.ConfirmationRequired && errorBody.Code == "bandwidth_limit_confirmation_required" {
			message := strings.TrimSpace(errorBody.Error)
			if message == "" {
				message = "Deregistering this machine will reduce the workspace bandwidth allowance below current usage."
			}
			return nil, &bandwidthLimitConfirmationError{Message: message}
		}
		if strings.TrimSpace(errorBody.Error) != "" {
			return nil, fmt.Errorf("%s", errorBody.Error)
		}
		if trimmed := strings.TrimSpace(string(body)); trimmed != "" {
			return nil, fmt.Errorf("machine removal failed: %s", trimmed)
		}
		return nil, fmt.Errorf("machine removal failed with status %d", resp.StatusCode)
	}
	var result deregisterMachineResponse
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return nil, err
	}
	return &result, nil
}

type bandwidthLimitConfirmationError struct {
	Message string
}

func (err *bandwidthLimitConfirmationError) Error() string {
	return err.Message
}

func updateMachineManagement(backendURL, token, machineID string, enabled bool, confirmBandwidthLimitChange bool) error {
	action := "disable-management"
	if enabled {
		action = "enable-management"
	}
	payload := bytes.NewReader(nil)
	if !enabled && confirmBandwidthLimitChange {
		body, err := json.Marshal(map[string]bool{"confirmBandwidthLimitChange": true})
		if err != nil {
			return err
		}
		payload = bytes.NewReader(body)
	}
	req, err := http.NewRequest(http.MethodPost, strings.TrimRight(backendURL, "/")+"/api/machines/"+url.PathEscape(machineID)+"/"+action, payload)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+token)
	if !enabled && confirmBandwidthLimitChange {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		var errorBody struct {
			Error                string `json:"error"`
			Code                 string `json:"code"`
			ConfirmationRequired bool   `json:"confirmationRequired"`
		}
		if json.Unmarshal(body, &errorBody) == nil && errorBody.ConfirmationRequired && errorBody.Code == "bandwidth_limit_confirmation_required" {
			message := strings.TrimSpace(errorBody.Error)
			if message == "" {
				message = "Disabling this machine will reduce the workspace bandwidth allowance below current usage."
			}
			return &bandwidthLimitConfirmationError{Message: message}
		}
		if strings.TrimSpace(errorBody.Error) != "" {
			return fmt.Errorf("%s", errorBody.Error)
		}
		if trimmed := strings.TrimSpace(string(body)); trimmed != "" {
			return fmt.Errorf("machine management update failed: %s", trimmed)
		}
		return fmt.Errorf("machine management update failed with status %d", resp.StatusCode)
	}
	return nil
}

func argFlagPresent(args []string, name string) bool {
	for _, arg := range args {
		if arg == name || strings.HasPrefix(arg, name+"=") {
			return true
		}
	}
	return false
}
