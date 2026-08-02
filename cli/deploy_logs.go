package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"regexp"
	"strings"
	"sync"
	"unicode"

	"golang.org/x/term"
)

type deployJobEvent struct {
	Type    string `json:"type"`
	Status  string `json:"status"`
	Stream  string `json:"stream"`
	Line    string `json:"line"`
	Message string `json:"message"`
	Reason  string `json:"reason"`
}

type deployJobEventResult struct {
	Event *deployJobEvent
	Err   error
}

func startDeployJobEventStream(ctx context.Context, backendURL, token, jobID string) <-chan deployJobEventResult {
	results := make(chan deployJobEventResult, 256)
	go func() {
		defer close(results)
		eventsURL := strings.TrimRight(backendURL, "/") + "/deploy/" + url.PathEscape(jobID) + "/events"
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, eventsURL, nil)
		if err != nil {
			results <- deployJobEventResult{Err: err}
			return
		}
		req.Header.Set("Authorization", "Bearer "+token)
		req.Header.Set("Accept", "text/event-stream")
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			if ctx.Err() == nil {
				results <- deployJobEventResult{Err: err}
			}
			return
		}
		defer resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			results <- deployJobEventResult{Err: fmt.Errorf("%s", formatHTTPError(resp, "deploy log stream request failed"))}
			return
		}
		err = readSSE(resp.Body, func(eventName string, data string) error {
			var event deployJobEvent
			if json.Unmarshal([]byte(data), &event) != nil {
				return nil
			}
			if event.Type == "" {
				event.Type = eventName
			}
			select {
			case results <- deployJobEventResult{Event: &event}:
				return nil
			case <-ctx.Done():
				return io.EOF
			}
		})
		if err != nil && ctx.Err() == nil {
			results <- deployJobEventResult{Err: err}
		}
	}()
	return results
}

var terminalEscapePattern = regexp.MustCompile(`\x1b\[[0-?]*[ -/]*[@-~]`)

func sanitizeDeployLogLine(line string) string {
	line = terminalEscapePattern.ReplaceAllString(line, "")
	return strings.Map(func(value rune) rune {
		if value == '\t' || !unicode.IsControl(value) {
			return value
		}
		return -1
	}, line)
}

type deployLogPane struct {
	mu      sync.Mutex
	output  *os.File
	label   string
	height  int
	lines   []string
	started bool
	closed  bool
}

func newDeployLogPane(output *os.File, label string, height int) *deployLogPane {
	if height <= 0 {
		height = 10
	}
	return &deployLogPane{output: output, label: label, height: height}
}

func deployOutputIsTerminal() bool {
	return term.IsTerminal(int(os.Stdout.Fd()))
}

func (pane *deployLogPane) Add(line string) {
	line = strings.TrimSpace(sanitizeDeployLogLine(line))
	if line == "" {
		return
	}
	pane.mu.Lock()
	defer pane.mu.Unlock()
	if pane.closed {
		return
	}
	pane.lines = append(pane.lines, line)
	if len(pane.lines) > pane.height {
		pane.lines = pane.lines[len(pane.lines)-pane.height:]
	}
	pane.render()
}

func (pane *deployLogPane) render() {
	if !pane.started {
		fmt.Fprintf(pane.output, "%s — live deploy logs (last %d lines)\n", pane.label, pane.height)
		for index := 0; index < pane.height; index++ {
			fmt.Fprintln(pane.output)
		}
		pane.started = true
	}
	fmt.Fprintf(pane.output, "\x1b[%dA", pane.height)
	width := 0
	if columns, _, err := term.GetSize(int(pane.output.Fd())); err == nil {
		width = columns
	}
	for index := 0; index < pane.height; index++ {
		line := ""
		if index < len(pane.lines) {
			line = pane.lines[index]
		}
		if width > 1 && len([]rune(line)) >= width {
			line = string([]rune(line)[:width-1])
		}
		fmt.Fprintf(pane.output, "\x1b[2K%s\n", line)
	}
}

func (pane *deployLogPane) Close() {
	pane.mu.Lock()
	defer pane.mu.Unlock()
	if pane.closed {
		return
	}
	pane.closed = true
}
