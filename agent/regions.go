package main

import (
	"bufio"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"strings"
)

type regionSummary struct {
	Code                  string `json:"code"`
	Name                  string `json:"name"`
	EdgeHostname          string `json:"edgeHostname"`
	WireGuardEndpointHost string `json:"wireguardEndpointHost"`
}

type probedRegion struct {
	region regionSummary
}

func selectEnrollmentRegion(backendURL, requestedRegion string) string {
	if strings.TrimSpace(requestedRegion) != "" {
		return strings.TrimSpace(requestedRegion)
	}

	regions, err := fetchActiveRegions(backendURL)
	if err != nil || len(regions) == 0 {
		return defaultRegionCode()
	}

	probed := probeRegions(regions)
	defaultIndex := defaultRegionIndex(probed, defaultRegionCode())
	if defaultIndex < 0 {
		defaultIndex = 0
	}

	fmt.Println("Select an edge region for this machine:")
	for i, region := range probed {
		suffix := ""
		if i == defaultIndex {
			suffix = " [recommended]"
		}
		fmt.Printf("  %d. %s (%s)%s\n", i+1, region.region.Name, region.region.Code, suffix)
	}

	input, cleanup, ok := interactiveInput()
	if !ok {
		selected := probed[defaultIndex].region.Code
		fmt.Printf("Using %s (%s).\n", probed[defaultIndex].region.Name, selected)
		return selected
	}
	defer cleanup()

	fmt.Printf("Region [%d]: ", defaultIndex+1)
	reader := bufio.NewReader(input)
	value, err := reader.ReadString('\n')
	if err != nil {
		return probed[defaultIndex].region.Code
	}
	value = strings.TrimSpace(value)
	if value == "" {
		return probed[defaultIndex].region.Code
	}
	var selected int
	if _, err := fmt.Sscanf(value, "%d", &selected); err != nil || selected < 1 || selected > len(probed) {
		fmt.Println("Invalid selection; using recommended region.")
		return probed[defaultIndex].region.Code
	}
	return probed[selected-1].region.Code
}

func fetchActiveRegions(backendURL string) ([]regionSummary, error) {
	req, err := http.NewRequest(http.MethodGet, strings.TrimRight(backendURL, "/")+"/api/regions", nil)
	if err != nil {
		return nil, err
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("regions request failed with status %d", resp.StatusCode)
	}
	var body struct {
		Regions []regionSummary `json:"regions"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		return nil, err
	}
	return body.Regions, nil
}

func probeRegions(regions []regionSummary) []probedRegion {
	probed := make([]probedRegion, 0, len(regions))
	for _, region := range regions {
		probed = append(probed, probedRegion{region: region})
	}
	return probed
}

func defaultRegionIndex(regions []probedRegion, defaultCode string) int {
	defaultCode = strings.TrimSpace(defaultCode)
	for i, region := range regions {
		if region.region.Code == defaultCode {
			return i
		}
	}
	return -1
}

func interactiveInput() (*os.File, func(), bool) {
	if tty, err := os.OpenFile("/dev/tty", os.O_RDONLY, 0); err == nil {
		return tty, func() { _ = tty.Close() }, true
	}
	info, err := os.Stdin.Stat()
	if err != nil {
		return nil, func() {}, false
	}
	if (info.Mode() & os.ModeCharDevice) == 0 {
		return nil, func() {}, false
	}
	return os.Stdin, func() {}, true
}
