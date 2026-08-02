package main

import (
	"bufio"
	"bytes"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"
)

func runLogs(args []string) {
	fs := flag.NewFlagSet("logs", flag.ExitOnError)
	backendURL := fs.String("backend", defaultBackendURL(), "backend base URL")
	appOverride := fs.String("app", "", "app id or name")
	serviceName := fs.String("service", "", "compose service name")
	maxTailLines := logStreamEnvInt("LOG_STREAM_MAX_TAIL_LINES", 1000)
	tailLines := fs.Int("tail", logStreamEnvInt("LOG_STREAM_DEFAULT_TAIL_LINES", 1000), "number of log lines to include")
	follow := fs.Bool("follow", true, "follow logs")
	noFollow := fs.Bool("no-follow", false, "print existing logs and stop")
	machineID := fs.String("machine", "", "machine id or name")
	deploymentID := fs.String("deployment", "", "deployment id")
	fs.Parse(args)

	if *tailLines < 0 || *tailLines > maxTailLines {
		fmt.Fprintf(os.Stderr, "--tail must be %d lines or fewer.\n", maxTailLines)
		os.Exit(1)
	}
	effectiveFollow := *follow
	if *noFollow {
		effectiveFollow = false
	}

	token, err := loadAuthToken(*backendURL)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	app, err := resolveSecretAppContext(*backendURL, token, *appOverride)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}

	session, err := createLogStream(*backendURL, token, app.ID, logStreamCreateRequest{
		ServiceName:  strings.TrimSpace(*serviceName),
		TailLines:    *tailLines,
		Follow:       effectiveFollow,
		MachineID:    strings.TrimSpace(*machineID),
		DeploymentID: strings.TrimSpace(*deploymentID),
	})
	if err != nil {
		fmt.Fprintf(os.Stderr, "failed to start log stream: %v\n", err)
		os.Exit(1)
	}
	if err := consumeLogStream(*backendURL, token, session, strings.TrimSpace(*serviceName)); err != nil {
		fmt.Fprintf(os.Stderr, "log stream failed: %v\n", err)
		os.Exit(1)
	}
}

func logStreamEnvInt(name string, fallback int) int {
	parsed, err := strconv.Atoi(strings.TrimSpace(os.Getenv(name)))
	if err != nil || parsed <= 0 {
		return fallback
	}
	return parsed
}

type logStreamCreateRequest struct {
	ServiceName  string `json:"serviceName,omitempty"`
	TailLines    int    `json:"tailLines"`
	Follow       bool   `json:"follow"`
	MachineID    string `json:"machineId,omitempty"`
	DeploymentID string `json:"deploymentId,omitempty"`
}

func createLogStream(backendURL, token, appID string, input logStreamCreateRequest) (*logStreamCreateResponse, error) {
	payload, err := json.Marshal(input)
	if err != nil {
		return nil, err
	}
	req, err := http.NewRequest(http.MethodPost, strings.TrimRight(backendURL, "/")+"/api/apps/"+url.PathEscape(appID)+"/log-streams", bytes.NewReader(payload))
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
	if resp.StatusCode != http.StatusCreated {
		return nil, fmt.Errorf("%s", formatHTTPError(resp, "log stream request failed"))
	}
	var result logStreamCreateResponse
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return nil, err
	}
	if strings.TrimSpace(result.EventsURL) == "" {
		return nil, fmt.Errorf("log stream response did not include an events URL")
	}
	return &result, nil
}

func consumeLogStream(backendURL, token string, session *logStreamCreateResponse, requestedService string) error {
	eventsURL := session.EventsURL
	if strings.HasPrefix(eventsURL, "/") {
		eventsURL = strings.TrimRight(backendURL, "/") + eventsURL
	}
	req, err := http.NewRequest(http.MethodGet, eventsURL, nil)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Accept", "text/event-stream")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("%s", formatHTTPError(resp, "log stream events request failed"))
	}

	targetCount := session.TargetCount
	if targetCount == 0 {
		targetCount = len(session.Targets)
	}
	seenStatus := map[string]bool{}
	return readSSE(resp.Body, func(eventName string, data string) error {
		var event logStreamEvent
		if err := json.Unmarshal([]byte(data), &event); err != nil {
			return nil
		}
		switch eventName {
		case "log":
			printLogLine(event, targetCount, requestedService)
		case "status":
			message := strings.TrimSpace(event.Message)
			if message != "" && !seenStatus[message] {
				fmt.Fprintln(os.Stderr, message)
				seenStatus[message] = true
			}
		case "error":
			message := strings.TrimSpace(event.Message)
			if message == "" {
				message = "log stream target failed"
			}
			fmt.Fprintln(os.Stderr, message)
		case "end":
			return io.EOF
		}
		return nil
	})
}

func readSSE(reader io.Reader, handle func(eventName string, data string) error) error {
	scanner := bufio.NewScanner(reader)
	scanner.Buffer(make([]byte, 1024), 1024*1024)
	eventName := "message"
	dataLines := []string{}
	flush := func() error {
		if len(dataLines) == 0 {
			eventName = "message"
			return nil
		}
		data := strings.Join(dataLines, "\n")
		dataLines = nil
		name := eventName
		eventName = "message"
		err := handle(name, data)
		if err == io.EOF {
			return io.EOF
		}
		return err
	}
	for scanner.Scan() {
		line := scanner.Text()
		if line == "" {
			if err := flush(); err != nil {
				if err == io.EOF {
					return nil
				}
				return err
			}
			continue
		}
		if strings.HasPrefix(line, "event:") {
			eventName = strings.TrimSpace(strings.TrimPrefix(line, "event:"))
		} else if strings.HasPrefix(line, "data:") {
			dataLines = append(dataLines, strings.TrimSpace(strings.TrimPrefix(line, "data:")))
		}
	}
	if err := scanner.Err(); err != nil {
		return err
	}
	if err := flush(); err != nil && err != io.EOF {
		return err
	}
	return nil
}

func printLogLine(event logStreamEvent, targetCount int, requestedService string) {
	line := event.Line
	if line == "" {
		return
	}
	service := strings.TrimSpace(event.Service)
	machine := strings.TrimSpace(event.MachineName)
	if machine == "" {
		machine = strings.TrimSpace(event.MachineID)
	}
	prefixes := []string{}
	if targetCount > 1 {
		if machine != "" {
			prefixes = append(prefixes, machine)
		}
		if service != "" {
			prefixes = append(prefixes, service)
		}
	} else if strings.TrimSpace(requestedService) == "" && service != "" {
		prefixes = append(prefixes, service)
	}
	if len(prefixes) == 0 {
		fmt.Println(line)
		return
	}
	fmt.Printf("%-12s %s\n", strings.Join(prefixes, " "), line)
}
