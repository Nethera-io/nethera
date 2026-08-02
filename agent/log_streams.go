package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"nhooyr.io/websocket"
)

func pollAgent(backendURL, token string, activeJob *deployJob) (*agentPollResponse, error) {
	snapshot, err := collectMachineSnapshot(activeJob)
	if err != nil {
		return nil, err
	}
	payload, err := json.Marshal(snapshot)
	if err != nil {
		return nil, err
	}
	req, err := http.NewRequest(http.MethodPost, strings.TrimRight(backendURL, "/")+"/api/agent/poll", bytes.NewReader(payload))
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
		return nil, &httpStatusError{Endpoint: "api/agent/poll", Status: resp.StatusCode, Details: summarizeBody(body)}
	}
	var pollResponse agentPollResponse
	if err := json.NewDecoder(resp.Body).Decode(&pollResponse); err != nil {
		return nil, err
	}
	return &pollResponse, nil
}

func streamLogTarget(parentCtx context.Context, backendURL, token, machineID string, target logStreamTargetPayload) error {
	ctx, cancel := context.WithTimeout(parentCtx, time.Duration(agentLogStreamEnvInt("LOG_STREAM_MAX_SESSION_SECONDS", 1800))*time.Second)
	defer cancel()
	startedAt := time.Now()
	fmt.Printf("log stream target %s attach starting: deployment=%s service=%s tail=%d follow=%t\n", target.TargetID, target.DeploymentID, strings.TrimSpace(target.ServiceName), target.TailLines, target.Follow)

	attachURL, err := logStreamAttachWebSocketURL(backendURL, target.TargetID)
	if err != nil {
		return err
	}
	dialTimeout := time.Duration(agentLogStreamEnvInt("LOG_STREAM_ATTACH_DIAL_TIMEOUT_SECONDS", 20)) * time.Second
	dialCtx, dialCancel := context.WithTimeout(ctx, dialTimeout)
	defer dialCancel()
	fmt.Printf("log stream target %s dialing websocket attach: %s\n", target.TargetID, redactLogStreamAttachURL(attachURL))

	conn, resp, err := websocket.Dial(dialCtx, attachURL, &websocket.DialOptions{
		HTTPHeader: http.Header{"Authorization": []string{"Bearer " + token}},
	})
	if err != nil {
		status := 0
		details := ""
		if resp != nil {
			status = resp.StatusCode
			if resp.Body != nil {
				body, _ := io.ReadAll(resp.Body)
				details = summarizeBody(body)
			}
		}
		if errors.Is(dialCtx.Err(), context.DeadlineExceeded) {
			fmt.Printf("log stream target %s attach websocket timed out after %s\n", target.TargetID, dialTimeout)
			return fmt.Errorf("log stream websocket attach timed out after %s", dialTimeout)
		}
		fmt.Printf("log stream target %s attach websocket failed after %s: %v\n", target.TargetID, time.Since(startedAt).Round(time.Millisecond), err)
		if status != 0 {
			return &httpStatusError{Endpoint: "api/agent/log-stream-targets/:targetId/attach-ws", Status: status, Details: details}
		}
		return err
	}
	defer conn.Close(websocket.StatusNormalClosure, "log stream finished")
	fmt.Printf("log stream target %s attach response: websocket connected after %s\n", target.TargetID, time.Since(startedAt).Round(time.Millisecond))

	streamCtx, streamCancel := context.WithCancel(ctx)
	defer streamCancel()
	go func() {
		for {
			_, _, err := conn.Reader(streamCtx)
			if err != nil {
				streamCancel()
				return
			}
		}
	}()

	writer := &websocketNDJSONWriter{ctx: streamCtx, conn: conn}

	composeDone := make(chan error, 1)
	go func() {
		composeDone <- runComposeLogs(streamCtx, writer, machineID, target)
		_ = writer.Close()
	}()

	select {
	case composeErr := <-composeDone:
		if composeErr == nil {
			_ = conn.Close(websocket.StatusNormalClosure, "log stream finished")
		}
		return composeErr
	case <-ctx.Done():
		_ = conn.Close(websocket.StatusGoingAway, ctx.Err().Error())
		composeErr := <-composeDone
		if composeErr != nil {
			return composeErr
		}
		return ctx.Err()
	}
}

func logStreamAttachWebSocketURL(backendURL, targetID string) (string, error) {
	parsed, err := url.Parse(strings.TrimRight(backendURL, "/"))
	if err != nil {
		return "", err
	}
	switch parsed.Scheme {
	case "https":
		parsed.Scheme = "wss"
	case "http":
		parsed.Scheme = "ws"
	default:
		return "", fmt.Errorf("unsupported backend URL scheme %q", parsed.Scheme)
	}
	parsed.Path = strings.TrimRight(parsed.Path, "/") + "/api/agent/log-stream-targets/" + url.PathEscape(targetID) + "/attach-ws"
	parsed.RawQuery = ""
	parsed.Fragment = ""
	return parsed.String(), nil
}

func redactLogStreamAttachURL(raw string) string {
	parsed, err := url.Parse(raw)
	if err != nil {
		return "<invalid attach url>"
	}
	parsed.RawQuery = ""
	parsed.Fragment = ""
	return parsed.String()
}

type websocketNDJSONWriter struct {
	mu     sync.Mutex
	ctx    context.Context
	conn   *websocket.Conn
	buffer []byte
}

func (w *websocketNDJSONWriter) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.buffer = append(w.buffer, p...)
	for {
		idx := bytes.IndexByte(w.buffer, '\n')
		if idx < 0 {
			break
		}
		line := bytes.TrimSpace(w.buffer[:idx])
		w.buffer = append([]byte(nil), w.buffer[idx+1:]...)
		if len(line) == 0 {
			continue
		}
		if err := w.conn.Write(w.ctx, websocket.MessageText, line); err != nil {
			return 0, err
		}
	}
	return len(p), nil
}

func (w *websocketNDJSONWriter) Close() error {
	w.mu.Lock()
	defer w.mu.Unlock()
	line := bytes.TrimSpace(w.buffer)
	w.buffer = nil
	if len(line) == 0 {
		return nil
	}
	return w.conn.Write(w.ctx, websocket.MessageText, line)
}

func agentLogStreamTrace(format string, args ...any) {
	if os.Getenv("LOG_STREAM_TRACE") != "true" {
		return
	}
	fmt.Printf(format+"\n", args...)
}

func agentLogStreamEnvInt(name string, fallback int) int {
	parsed, err := strconv.Atoi(strings.TrimSpace(os.Getenv(name)))
	if err != nil || parsed <= 0 {
		return fallback
	}
	return parsed
}

func runComposeLogs(ctx context.Context, writer io.Writer, machineID string, target logStreamTargetPayload) error {
	encoder := json.NewEncoder(writer)
	var encodeMu sync.Mutex
	firstFrameWritten := false
	writeFrame := func(frame agentLogFrame) error {
		encodeMu.Lock()
		defer encodeMu.Unlock()
		if frame.TargetID == "" {
			frame.TargetID = target.TargetID
		}
		if frame.DeploymentID == "" {
			frame.DeploymentID = target.DeploymentID
		}
		if frame.MachineID == "" {
			frame.MachineID = machineID
		}
		if err := encoder.Encode(frame); err != nil {
			return err
		}
		if !firstFrameWritten {
			firstFrameWritten = true
			agentLogStreamTrace("log stream target %s first frame written: type=%s", target.TargetID, frame.Type)
		}
		return nil
	}
	fail := func(message string) error {
		_ = writeFrame(agentLogFrame{Type: "error", Message: message})
		return fmt.Errorf("%s", message)
	}
	if err := writeFrame(agentLogFrame{Type: "status", Message: "Agent attached to log stream."}); err != nil {
		return err
	}

	metadata, err := loadDeploymentMetadata(metadataPathForDeployment(filepath.Join(deploymentsStateDir(), sanitizeProjectSegment(target.DeploymentID))))
	if err != nil {
		return fail("Deployment files not found on this machine.")
	}
	if strings.TrimSpace(metadata.DeploymentID) == "" || strings.TrimSpace(metadata.ProjectName) == "" || strings.TrimSpace(metadata.GeneratedComposePath) == "" {
		return fail("Deployment files not found on this machine.")
	}
	if _, err := os.Stat(metadata.GeneratedComposePath); err != nil {
		return fail("Deployment files not found on this machine.")
	}
	dockerBin, err := resolveDockerBinary()
	if err != nil {
		return fail("Docker is not available on this machine.")
	}
	tailLines := target.TailLines
	if tailLines < 0 {
		tailLines = 0
	}
	maxTailLines := agentLogStreamEnvInt("LOG_STREAM_MAX_TAIL_LINES", 1000)
	if tailLines > maxTailLines {
		tailLines = maxTailLines
	}
	projectName, useProjectFlag := composeProjectForMetadata(metadata)
	args := composeCommandArgs(projectName, useProjectFlag, metadata.GeneratedComposePath, "logs", "--tail="+strconv.Itoa(tailLines))
	if target.Follow {
		args = append(args, "-f")
	}
	if strings.TrimSpace(target.ServiceName) != "" {
		args = append(args, strings.TrimSpace(target.ServiceName))
	}
	fmt.Printf("log stream target %s starting docker compose logs: deployment=%s service=%s tail=%d follow=%t\n", target.TargetID, target.DeploymentID, strings.TrimSpace(target.ServiceName), tailLines, target.Follow)

	cmd := exec.CommandContext(ctx, dockerBin, args...)
	cmd.Env = os.Environ()
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return fail("Failed to attach to Docker logs.")
	}
	stderr, err := cmd.StderrPipe()
	if err != nil {
		return fail("Failed to attach to Docker logs.")
	}
	if err := cmd.Start(); err != nil {
		return fail(shortComposeLogError(err))
	}

	var scanWG sync.WaitGroup
	scanWG.Add(2)
	go scanComposeLogPipe(stdout, "stdout", target, writeFrame, &scanWG)
	go scanComposeLogPipe(stderr, "stderr", target, writeFrame, &scanWG)
	scanWG.Wait()
	err = cmd.Wait()
	if err != nil {
		message := shortComposeLogError(err)
		_ = writeFrame(agentLogFrame{Type: "error", Message: message})
		return fmt.Errorf("%s", message)
	}
	if err := writeFrame(agentLogFrame{Type: "end", Reason: "completed"}); err != nil {
		return err
	}
	return nil
}

func scanComposeLogPipe(reader io.Reader, streamName string, target logStreamTargetPayload, writeFrame func(agentLogFrame) error, wg *sync.WaitGroup) {
	defer wg.Done()
	maxLineBytes := agentLogStreamEnvInt("LOG_STREAM_MAX_LINE_BYTES", 16384)
	scanner := bufio.NewScanner(reader)
	scanner.Buffer(make([]byte, 1024), maxLineBytes*2)
	for scanner.Scan() {
		service, line := parseComposeLogLine(scanner.Text(), target.ServiceName)
		if len(line) > maxLineBytes {
			line = line[:maxLineBytes] + "... [truncated]"
		}
		if err := writeFrame(agentLogFrame{
			Type:    "log",
			Service: service,
			Stream:  streamName,
			Time:    time.Now().UTC().Format(time.RFC3339Nano),
			Line:    line,
		}); err != nil {
			return
		}
	}
}

func parseComposeLogLine(line, requestedService string) (string, string) {
	service := strings.TrimSpace(requestedService)
	if service != "" {
		return service, line
	}
	if before, after, ok := strings.Cut(line, "|"); ok {
		candidate := strings.TrimSpace(before)
		if candidate != "" && !strings.Contains(candidate, " ") {
			return candidate, strings.TrimSpace(after)
		}
	}
	return "", line
}

func shortComposeLogError(err error) string {
	if err == nil {
		return ""
	}
	if exitErr, ok := err.(*exec.ExitError); ok {
		details := summarizeBody(exitErr.Stderr)
		if details != "" {
			return "docker compose logs failed: " + details
		}
		return fmt.Sprintf("docker compose logs failed with exit code %d", exitErr.ExitCode())
	}
	return "docker compose logs failed: " + err.Error()
}
