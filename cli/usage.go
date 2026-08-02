package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"
)

func runUsage(args []string) {
	fs := flag.NewFlagSet("usage", flag.ExitOnError)
	backendURL := fs.String("backend", defaultBackendURL(), "backend base URL")
	month := fs.String("month", "", "usage month in YYYY-MM format")
	fs.Parse(args)

	token, err := loadAuthToken(*backendURL)
	if err != nil {
		fmt.Println(err)
		os.Exit(1)
	}

	usage, err := fetchMonthlyUsage(*backendURL, token, *month)
	if err != nil {
		fmt.Printf("failed to fetch usage: %v\n", err)
		os.Exit(1)
	}
	printMonthlyUsage(usage)
}

func fetchMonthlyUsage(backendURL, token, month string) (*monthlyUsageResponse, error) {
	queryURL := strings.TrimRight(backendURL, "/") + "/api/usage/monthly"
	if strings.TrimSpace(month) != "" {
		queryURL += "?month=" + url.QueryEscape(strings.TrimSpace(month))
	}
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
		return nil, fmt.Errorf("%s", formatHTTPError(resp, "usage request failed"))
	}

	var result monthlyUsageResponse
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return nil, err
	}
	return &result, nil
}

func printMonthlyUsage(usage *monthlyUsageResponse) {
	fmt.Printf("Usage for %s\n", usage.Month)
	if strings.TrimSpace(usage.Organization.Name) != "" {
		fmt.Printf("Organization: %s\n", usage.Organization.Name)
	}
	if strings.TrimSpace(usage.Organization.Plan.Name) != "" {
		if usage.Organization.Plan.MonthlyBandwidthGB == nil {
			fmt.Printf("Plan: %s (unmetered)\n", usage.Organization.Plan.Name)
		} else {
			fmt.Printf("Plan: %s (%d GB/month)\n", usage.Organization.Plan.Name, *usage.Organization.Plan.MonthlyBandwidthGB)
		}
	}
	fmt.Printf("Bandwidth in: %s\n", formatHumanBytesString(usage.Usage.BytesIn))
	fmt.Printf("Bandwidth out: %s\n", formatHumanBytesString(usage.Usage.BytesOut))
	fmt.Printf("Total bandwidth: %s\n", formatHumanBytesString(usage.Usage.TotalBytes))
	fmt.Printf("Requests: %s\n", usage.Usage.Requests)
	if strings.TrimSpace(usage.Organization.BandwidthState) != "" {
		fmt.Printf("Bandwidth state: %s\n", usage.Organization.BandwidthState)
	}
}

func formatHumanBytesString(raw string) string {
	trimmed := strings.TrimSpace(raw)
	if trimmed == "" {
		return "0 B"
	}
	value, err := strconv.ParseFloat(trimmed, 64)
	if err != nil || value < 0 {
		return trimmed
	}
	return formatHumanBytes(value)
}

func formatHumanBytes(value float64) string {
	units := []string{"B", "KB", "MB", "GB", "TB", "PB"}
	unitIndex := 0
	for value >= 1000 && unitIndex < len(units)-1 {
		value = value / 1000
		unitIndex++
	}
	if unitIndex == 0 {
		return fmt.Sprintf("%.0f %s", value, units[unitIndex])
	}
	if value >= 100 {
		return fmt.Sprintf("%.0f %s", value, units[unitIndex])
	}
	if value >= 10 {
		return fmt.Sprintf("%.1f %s", value, units[unitIndex])
	}
	return fmt.Sprintf("%.2f %s", value, units[unitIndex])
}
