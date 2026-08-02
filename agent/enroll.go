package main

import (
	"bytes"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"flag"
	"fmt"
	"net/http"
	"os"
	"runtime"
	"strings"
	"time"
)

func runEnroll(args []string) {
	fs := flag.NewFlagSet("enroll", flag.ExitOnError)
	backendURL := fs.String("backend", defaultBackendURL(), "backend base URL")
	configPath := fs.String("config", defaultMachineConfigPath(), "machine config path")
	waitTimeout := fs.Duration("timeout", 5*time.Minute, "maximum time to wait for pairing")
	regionCode := fs.String("region", strings.TrimSpace(os.Getenv("NETH_REGION")), "region code override")
	fs.Parse(args)

	result, err := ensureMachinePairing(*backendURL, *configPath, *waitTimeout, false, *regionCode)
	if err != nil {
		fmt.Printf("enrollment failed: %v\n", err)
		os.Exit(1)
	}
	fmt.Printf("machine enrolled: %s\n", result.MachineID)
}

func ensureMachinePairing(backendURL, configPath string, waitTimeout time.Duration, forcePair bool, regionCode string) (machineConfig, error) {
	enrollmentEnvironment := agentEnvironmentForBackend(backendURL)
	existing, err := loadMachineConfigFile(configPath)
	if err != nil {
		return machineConfig{}, err
	}
	existingEnvironment := strings.TrimSpace(existing.Environment)
	existingBackendURL := strings.TrimRight(strings.TrimSpace(existing.BackendURL), "/")
	existingAPIURL := strings.TrimRight(strings.TrimSpace(existing.APIURL), "/")
	normalizedBackendURL := strings.TrimRight(strings.TrimSpace(backendURL), "/")
	existingMatchesEnvironment := existingEnvironment == "" || existingEnvironment == enrollmentEnvironment
	existingMatchesBackend := (existingBackendURL == "" || existingBackendURL == normalizedBackendURL) &&
		(existingAPIURL == "" || existingAPIURL == normalizedBackendURL)
	if existing.MachineToken != "" && existing.MachineID != "" && !forcePair && existingMatchesEnvironment && existingMatchesBackend {
		return existing, nil
	}

	if forcePair {
		fmt.Printf("Existing machine credentials rejected for %s. Starting re-pair...\n", strings.TrimSpace(existing.MachineID))
	} else if existing.MachineToken != "" && existing.MachineID != "" && (!existingMatchesEnvironment || !existingMatchesBackend) {
		if existingEnvironment == "" {
			existingEnvironment = "unknown"
		}
		fmt.Printf("Existing machine credentials are for %s, but this enrollment is for %s. Starting pairing...\n", existingEnvironment, enrollmentEnvironment)
	} else {
		fmt.Println("No existing machine credentials found. Starting pairing...")
	}
	secretBytes := make([]byte, 32)
	if _, err := rand.Read(secretBytes); err != nil {
		return machineConfig{}, fmt.Errorf("failed to generate enrollment secret: %w", err)
	}
	secret := hex.EncodeToString(secretBytes)
	selectedRegionCode := selectEnrollmentRegion(backendURL, regionCode)

	priorMachineID := strings.TrimSpace(existing.MachineID)
	payload, _ := json.Marshal(map[string]any{
		"hostname":   mustHostname(),
		"os":         runtime.GOOS,
		"arch":       runtime.GOARCH,
		"secret":     secret,
		"machineId":  priorMachineID,
		"regionCode": selectedRegionCode,
	})
	resp, err := http.Post(backendURL+"/api/machines/enroll/start", "application/json", bytes.NewReader(payload))
	if err != nil {
		return machineConfig{}, fmt.Errorf("enrollment request failed: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return machineConfig{}, fmt.Errorf("enrollment failed with status %d", resp.StatusCode)
	}

	var startResult struct {
		PairCode            string `json:"pairCode"`
		ExpiresIn           int    `json:"expiresIn"`
		PendingEnrollmentID string `json:"pendingEnrollmentId"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&startResult); err != nil {
		return machineConfig{}, fmt.Errorf("failed to decode enrollment response: %w", err)
	}

	fmt.Println("Machine waiting to be paired.")
	fmt.Println()
	fmt.Println("Pairing code:")
	fmt.Println(startResult.PairCode)
	fmt.Println()
	fmt.Println("To pair this machine, run the following from your signed-in shell:")
	fmt.Printf("  neth machine pair %s\n", startResult.PairCode)
	fmt.Println()

	result, err := waitForPairing(backendURL, startResult.PendingEnrollmentID, waitTimeout)
	if err != nil {
		return machineConfig{}, err
	}
	cfg := machineConfig{
		BackendURL:   backendURL,
		MachineID:    result.MachineID,
		MachineToken: result.MachineToken,
		Environment:  enrollmentEnvironment,
		APIURL:       backendURL,
	}
	if err := saveMachineConfig(configPath, cfg); err != nil {
		return machineConfig{}, fmt.Errorf("failed to save machine credentials: %w", err)
	}
	fmt.Printf("Pairing successful. Machine ID: %s\n", cfg.MachineID)
	return cfg, nil
}

func waitForPairing(backendURL, pendingID string, timeout time.Duration) (struct {
	MachineID    string `json:"machineId"`
	MachineToken string `json:"machineToken"`
	Status       string `json:"status"`
}, error) {
	var result struct {
		MachineID    string `json:"machineId"`
		MachineToken string `json:"machineToken"`
		Status       string `json:"status"`
	}
	deadline := time.Now().Add(timeout)
	for {
		req, err := http.NewRequest(http.MethodGet, backendURL+"/api/machines/enroll/wait/"+pendingID, nil)
		if err != nil {
			return result, err
		}
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			return result, err
		}
		body := struct {
			MachineID    string `json:"machineId"`
			MachineToken string `json:"machineToken"`
			Status       string `json:"status"`
		}{}
		if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
			_ = resp.Body.Close()
			return result, err
		}
		_ = resp.Body.Close()
		if resp.StatusCode == http.StatusOK && body.Status == "paired" {
			if strings.TrimSpace(body.MachineID) == "" || strings.TrimSpace(body.MachineToken) == "" {
				return result, fmt.Errorf("pairing completed, but the backend did not return machine credentials; restart pairing")
			}
			result.MachineID = body.MachineID
			result.MachineToken = body.MachineToken
			result.Status = body.Status
			return result, nil
		}
		if resp.StatusCode == http.StatusConflict && body.Status == "paired" {
			return result, fmt.Errorf("pairing completed, but the backend could not return machine credentials; restart pairing")
		}
		if resp.StatusCode == http.StatusGone || resp.StatusCode == http.StatusNotFound {
			return result, fmt.Errorf("pairing expired or was cancelled")
		}
		if time.Now().After(deadline) {
			return result, fmt.Errorf("pairing timed out")
		}
		time.Sleep(2 * time.Second)
	}
}
