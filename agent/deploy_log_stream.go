package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"nhooyr.io/websocket"
)

type deployLogFrame struct {
	Type   string `json:"type"`
	Stream string `json:"stream,omitempty"`
	Time   string `json:"time,omitempty"`
	Line   string `json:"line,omitempty"`
}

type deployLogStreamer struct {
	mu         sync.Mutex
	frames     chan deployLogFrame
	done       chan struct{}
	closed     bool
	backendURL string
	token      string
	jobID      string
	parent     context.Context
}

func startDeployLogStreamer(parent context.Context, backendURL, token, jobID string) *deployLogStreamer {
	streamer := &deployLogStreamer{
		frames:     make(chan deployLogFrame, 256),
		done:       make(chan struct{}),
		backendURL: strings.TrimRight(backendURL, "/"),
		token:      token,
		jobID:      jobID,
		parent:     parent,
	}
	go func() {
		defer close(streamer.done)
		streamer.flushLoop()
	}()
	return streamer
}

func (s *deployLogStreamer) flushLoop() {
	attachURL, err := deployLogAttachWebSocketURL(s.backendURL, s.jobID)
	if err != nil {
		fmt.Printf("deploy log stream websocket URL failed: %v\n", err)
		drainDeployLogFrames(s.frames)
		return
	}
	dialTimeout := time.Duration(agentLogStreamEnvInt("DEPLOY_LOG_STREAM_ATTACH_DIAL_TIMEOUT_SECONDS", 20)) * time.Second
	dialCtx, dialCancel := context.WithTimeout(s.parent, dialTimeout)
	defer dialCancel()
	conn, resp, err := websocket.Dial(dialCtx, attachURL, &websocket.DialOptions{
		HTTPHeader: http.Header{"Authorization": []string{"Bearer " + s.token}},
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
			fmt.Printf("deploy log stream websocket timed out after %s\n", dialTimeout)
		} else if status != 0 {
			fmt.Printf("deploy log stream websocket rejected with status %d: %s\n", status, details)
		} else {
			fmt.Printf("deploy log stream websocket failed: %v\n", err)
		}
		drainDeployLogFrames(s.frames)
		return
	}
	defer conn.Close(websocket.StatusNormalClosure, "deploy log stream finished")
	streamCtx, streamCancel := context.WithCancel(s.parent)
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
	for {
		select {
		case frame, ok := <-s.frames:
			if !ok {
				_ = writeDeployLogWebSocketFrame(streamCtx, conn, deployLogFrame{Type: "end", Time: time.Now().UTC().Format(time.RFC3339Nano)})
				return
			}
			if err := writeDeployLogWebSocketFrame(streamCtx, conn, frame); err != nil {
				fmt.Printf("deploy log stream websocket write failed: %v\n", err)
				drainDeployLogFrames(s.frames)
				return
			}
		case <-s.parent.Done():
			return
		}
	}
}

func writeDeployLogWebSocketFrame(ctx context.Context, conn *websocket.Conn, frame deployLogFrame) error {
	payload, err := json.Marshal(frame)
	if err != nil {
		return err
	}
	return conn.Write(ctx, websocket.MessageText, payload)
}

func deployLogAttachWebSocketURL(backendURL, jobID string) (string, error) {
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
	parsed.Path = strings.TrimRight(parsed.Path, "/") + "/api/agent/deploy-jobs/" + url.PathEscape(jobID) + "/events-ws"
	parsed.RawQuery = ""
	return parsed.String(), nil
}

func drainDeployLogFrames(frames <-chan deployLogFrame) {
	for range frames {
	}
}

func (s *deployLogStreamer) Emit(stream, line string) {
	line = strings.TrimSpace(line)
	if line == "" {
		return
	}
	if len(line) > 16*1024 {
		line = line[:16*1024] + "... [truncated]"
	}
	frame := deployLogFrame{
		Type:   "log",
		Stream: strings.TrimSpace(stream),
		Time:   time.Now().UTC().Format(time.RFC3339Nano),
		Line:   line,
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return
	}
	select {
	case s.frames <- frame:
	default:
	}
}

func (s *deployLogStreamer) Close() {
	s.mu.Lock()
	if !s.closed {
		s.closed = true
		close(s.frames)
	}
	s.mu.Unlock()
	select {
	case <-s.done:
	case <-time.After(5 * time.Second):
	}
}
