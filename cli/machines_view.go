package main

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"time"
)

func listMachines(backendURL, token string) ([]machineSummary, error) {
	req, err := http.NewRequest(http.MethodGet, backendURL+"/api/machines", nil)
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
		return nil, fmt.Errorf("%s", formatHTTPError(resp, "failed to list machines"))
	}
	var payload struct {
		Machines []machineSummary `json:"machines"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&payload); err != nil {
		return nil, err
	}
	return payload.Machines, nil
}

func printMachines(machines []machineSummary) {
	activeMachines := activeMachineSummaries(machines)
	if len(activeMachines) == 0 {
		fmt.Println("no active machines registered to your account")
		return
	}

	fmt.Println("Machines registered to your account:")
	for _, machine := range activeMachines {
		availability := "offline"
		if machine.IsAvailable {
			availability = "available"
		}
		apps := machineAppsLabel(machine)
		region := machine.RegionCode
		if region == "" {
			region = "unknown"
		}
		management := machineManagementLabel(machine)
		if management == "active" {
			fmt.Printf(" - %s [%s] (%s) - apps: %s\n", machine.Name, region, availability, apps)
		} else {
			fmt.Printf(" - %s [%s] (%s, management: %s) - apps: %s\n", machine.Name, region, availability, management, apps)
		}
	}
}

func printMachineStats(machines []machineSummary) {
	activeMachines := activeMachineSummaries(machines)
	if len(activeMachines) == 0 {
		fmt.Println("no active machines registered to your account")
		return
	}

	fmt.Println("Machine stats:")
	for _, machine := range activeMachines {
		availability := "offline"
		if machine.IsAvailable {
			availability = "available"
		}
		region := machine.RegionCode
		if region == "" {
			region = "unknown"
		}
		version := strings.TrimSpace(machine.AgentVersion)
		if version == "" {
			version = "unknown"
		}
		lastSeen := strings.TrimSpace(machine.LastSeenAt)
		if lastSeen == "" {
			lastSeen = "never"
		}
		fmt.Printf(" - %s [%s] (%s)\n", machine.Name, region, availability)
		fmt.Printf("   management: %s\n", machineManagementLabel(machine))
		fmt.Printf("   agent: %s, last seen: %s\n", version, lastSeen)
		fmt.Printf("   wireguard: %s, docker: %s\n", boolStatus(snapshotBool(machine.StatusSnapshot, "wireguard", "up")), boolStatus(snapshotBool(machine.StatusSnapshot, "docker", "up")))
		if gpu := formatGPUDiagnostics(machine.StatusSnapshot); gpu != "" {
			fmt.Printf("   gpu: %s\n", gpu)
		}
		fmt.Printf("   deployments: running %d, degraded %d, failed %d\n",
			snapshotInt(machine.StatusSnapshot, "deployments", "running"),
			snapshotInt(machine.StatusSnapshot, "deployments", "degraded"),
			snapshotInt(machine.StatusSnapshot, "deployments", "failed"),
		)
		if cpu := snapshotFloat(machine.StatusSnapshot, "cpu", "utilisationPercent"); cpu >= 0 {
			fmt.Printf("   cpu: %.1f%%\n", cpu)
		}
		if memory := formatBytesPair(machine.StatusSnapshot, "memory"); memory != "" {
			fmt.Printf("   memory: %s\n", memory)
		}
		if disk := formatBytesPair(machine.StatusSnapshot, "disk"); disk != "" {
			fmt.Printf("   disk: %s\n", disk)
		}
	}
}

func machineManagementLabel(machine machineSummary) string {
	state := strings.TrimSpace(machine.ManagementState)
	if state == "" {
		state = "active"
	}
	if state == "suspended" && strings.TrimSpace(machine.SuspendedReason) != "" {
		return "suspended (" + machine.SuspendedReason + ")"
	}
	return state
}

func activeMachineSummaries(machines []machineSummary) []machineSummary {
	activeMachines := make([]machineSummary, 0, len(machines))
	for _, machine := range machines {
		if machine.LifecycleStatus == "deregistered" {
			continue
		}
		activeMachines = append(activeMachines, machine)
	}
	return activeMachines
}

func snapshotBool(snapshot map[string]any, section, key string) bool {
	value, ok := nestedSnapshotValue(snapshot, section, key)
	if !ok {
		return false
	}
	boolValue, _ := value.(bool)
	return boolValue
}

func snapshotInt(snapshot map[string]any, section, key string) int {
	value, ok := nestedSnapshotValue(snapshot, section, key)
	if !ok {
		return 0
	}
	switch typed := value.(type) {
	case float64:
		return int(typed)
	case int:
		return typed
	default:
		return 0
	}
}

func snapshotFloat(snapshot map[string]any, section, key string) float64 {
	value, ok := nestedSnapshotValue(snapshot, section, key)
	if !ok {
		return -1
	}
	switch typed := value.(type) {
	case float64:
		return typed
	case int:
		return float64(typed)
	default:
		return -1
	}
}

func nestedSnapshotValue(snapshot map[string]any, section, key string) (any, bool) {
	if snapshot == nil {
		return nil, false
	}
	rawSection, ok := snapshot[section]
	if !ok {
		return nil, false
	}
	sectionMap, ok := rawSection.(map[string]any)
	if !ok {
		return nil, false
	}
	value, ok := sectionMap[key]
	return value, ok
}

func boolStatus(value bool) string {
	if value {
		return "up"
	}
	return "down"
}

func formatBytesPair(snapshot map[string]any, section string) string {
	used := snapshotInt(snapshot, section, "usedBytes")
	available := snapshotInt(snapshot, section, "availableBytes")
	if used == 0 && available == 0 {
		return ""
	}
	return fmt.Sprintf("%s used, %s available", formatBytes(int64(used)), formatBytes(int64(available)))
}

func formatGPUDiagnostics(snapshot map[string]any) string {
	if snapshot == nil {
		return ""
	}
	rawChecks, ok := snapshot["gpu"].([]any)
	if !ok || len(rawChecks) == 0 {
		return ""
	}
	hasError := false
	hasWarning := false
	firstProblem := ""
	hostDetail := ""
	for _, raw := range rawChecks {
		check, ok := raw.(map[string]any)
		if !ok {
			continue
		}
		name, _ := check["name"].(string)
		status, _ := check["status"].(string)
		message, _ := check["message"].(string)
		detail, _ := check["detail"].(string)
		if name == "host_nvidia_smi" && status == "ok" && strings.TrimSpace(detail) != "" {
			hostDetail = strings.TrimSpace(detail)
		}
		if status == "error" {
			hasError = true
			if firstProblem == "" {
				firstProblem = message
			}
		}
		if status == "warning" {
			hasWarning = true
			if firstProblem == "" {
				firstProblem = message
			}
		}
	}
	if hasError {
		if firstProblem != "" {
			return "not ready - " + firstProblem
		}
		return "not ready"
	}
	if hasWarning {
		if firstProblem != "" {
			return "needs attention - " + firstProblem
		}
		return "needs attention"
	}
	if hostDetail != "" {
		return "ready - " + hostDetail
	}
	return "ready"
}

func formatBytes(value int64) string {
	if value <= 0 {
		return "0 B"
	}
	units := []string{"B", "KiB", "MiB", "GiB", "TiB"}
	amount := float64(value)
	unit := 0
	for amount >= 1024 && unit < len(units)-1 {
		amount /= 1024
		unit += 1
	}
	if unit == 0 {
		return fmt.Sprintf("%d %s", value, units[unit])
	}
	return fmt.Sprintf("%.1f %s", amount, units[unit])
}

func formatTimestamp(value string) string {
	trimmed := strings.TrimSpace(value)
	if trimmed == "" {
		return "unknown"
	}
	parsed, err := time.Parse(time.RFC3339, trimmed)
	if err != nil {
		return trimmed
	}
	return parsed.Local().Format("2006-01-02 15:04")
}
