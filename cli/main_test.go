package main

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestFormatHTTPError(t *testing.T) {
	tests := []struct {
		name       string
		statusCode int
		body       []byte
		want       string
	}{
		{
			name:       "uses backend error message",
			statusCode: http.StatusConflict,
			body:       []byte(`{"error":"machine name already exists"}`),
			want:       "machine name already exists",
		},
		{
			name:       "falls back to status code",
			statusCode: http.StatusUnauthorized,
			body:       []byte(`{"message":"nope"}`),
			want:       "request failed with status: unexpected status 401",
		},
		{
			name:       "machine not found message",
			statusCode: http.StatusNotFound,
			body:       []byte(`{"error":"machine test-machine not found"}`),
			want:       "machine test-machine not found",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			resp := &http.Response{StatusCode: tt.statusCode, Body: ioNopCloser(bytes.NewReader(tt.body))}
			got := formatHTTPError(resp, "request failed with status")
			if got != tt.want {
				t.Fatalf("formatHTTPError() = %q, want %q", got, tt.want)
			}
		})
	}
}

func ioNopCloser(body *bytes.Reader) io.ReadCloser {
	return io.NopCloser(body)
}

func TestFormatEndpointInspectionErrorMissingMachine(t *testing.T) {
	resp := &http.Response{StatusCode: http.StatusNotFound, Body: ioNopCloser(bytes.NewReader([]byte(`{"error":"machine test-machine not found"}`)))}
	got := formatEndpointInspectionError(resp, "test-machine")
	want := "nethera.yml references machine \"test-machine\", but that machine is not registered to your account. Run `neth machine list` to see registered machines."
	if got != want {
		t.Fatalf("formatEndpointInspectionError() = %q, want %q", got, want)
	}
}

func TestPrintMachines(t *testing.T) {
	var output bytes.Buffer
	originalStdout := os.Stdout
	reader, writer, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	os.Stdout = writer
	defer func() {
		os.Stdout = originalStdout
	}()

	printMachines([]machineSummary{
		{Name: "test1", RegionCode: "sin", IsAvailable: true, RunningApps: []string{"api", "web"}},
		{Name: "test2", RegionCode: "iad", IsAvailable: false},
		{Name: "test3", RegionCode: "sg", IsAvailable: true, RunningApps: []string{"api", "web", "worker", "db", "cache", "admin"}},
	})

	_ = writer.Close()
	if _, err := io.Copy(&output, reader); err != nil {
		t.Fatal(err)
	}

	got := output.String()
	for _, want := range []string{
		"Machines registered to your account:",
		" - test1 [sin] (available) - apps: api, web",
		" - test2 [iad] (offline) - apps: no running apps",
		" - test3 [sg] (available) - apps: api, web, worker, db, ...",
	} {
		if !strings.Contains(got, want) {
			t.Fatalf("printMachines() output missing %q in:\n%s", want, got)
		}
	}
}

func TestMachineAppsLabelTruncatesLongLists(t *testing.T) {
	got := machineAppsLabel(machineSummary{RunningApps: []string{"api", "web", "worker", "db", "cache"}})
	want := "api, web, worker, db, ..."
	if got != want {
		t.Fatalf("machineAppsLabel() = %q, want %q", got, want)
	}
}

func TestPrintMonthlyUsage(t *testing.T) {
	var output bytes.Buffer
	originalStdout := os.Stdout
	reader, writer, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	os.Stdout = writer
	defer func() {
		os.Stdout = originalStdout
	}()

	limit := 100
	usage := &monthlyUsageResponse{Month: "2026-07"}
	usage.Organization.Name = "Test workspace"
	usage.Organization.BandwidthState = "ok"
	usage.Organization.Plan.Name = "Free"
	usage.Organization.Plan.MonthlyBandwidthGB = &limit
	usage.Usage.BytesIn = "1234"
	usage.Usage.BytesOut = "3456789"
	usage.Usage.TotalBytes = "90000000000"
	usage.Usage.Requests = "2"

	printMonthlyUsage(usage)

	_ = writer.Close()
	if _, err := io.Copy(&output, reader); err != nil {
		t.Fatal(err)
	}

	got := output.String()
	for _, want := range []string{
		"Usage for 2026-07",
		"Organization: Test workspace",
		"Plan: Free (100 GB/month)",
		"Bandwidth in: 1.23 KB",
		"Bandwidth out: 3.46 MB",
		"Total bandwidth: 90.0 GB",
		"Requests: 2",
		"Bandwidth state: ok",
	} {
		if !strings.Contains(got, want) {
			t.Fatalf("printMonthlyUsage() output missing %q in:\n%s", want, got)
		}
	}
}

func TestFindLongestCommandHelp(t *testing.T) {
	tests := []struct {
		args []string
		want string
	}{
		{args: []string{"deploy", "--help"}, want: "deploy"},
		{args: []string{"machine", "select", "--help"}, want: "machine select"},
		{args: []string{"endpoint", "token", "create", "-h"}, want: "endpoint token create"},
		{args: []string{"secrets", "set", "API_KEY", "--help"}, want: "secrets set"},
		{args: []string{"version", "--help"}, want: "version"},
	}

	for _, tt := range tests {
		entry := findLongestCommandHelp(tt.args)
		if entry == nil {
			t.Fatalf("findLongestCommandHelp(%v) returned nil", tt.args)
		}
		if entry.Command != tt.want {
			t.Fatalf("findLongestCommandHelp(%v) = %q, want %q", tt.args, entry.Command, tt.want)
		}
	}
}

func TestRenderCLIReferenceMDXUsesHelpCatalog(t *testing.T) {
	mdx := renderCLIReferenceMDX()
	for _, want := range []string{
		"title: CLI",
		"### `neth deploy`",
		"neth deploy [path/to/nethera.yml] [--no-token] [--wait|--replace] [--yes] [--verbose]",
		"### `neth endpoint token create`",
		"### `neth machine select`",
		"### `neth usage`",
	} {
		if !strings.Contains(mdx, want) {
			t.Fatalf("renderCLIReferenceMDX() missing %q", want)
		}
	}
}

func TestGroupedDeployEndpointsCollapsesSharedHostnames(t *testing.T) {
	groups := groupedDeployEndpoints(
		[]string{"homebox2", "laptop"},
		map[string][]deployEndpointSummary{
			"homebox2": {
				{Hostname: "n8n-web-t2k5.sg.nethera.io", AuthMode: "login"},
			},
			"laptop": {
				{Hostname: "n8n-web-t2k5.sg.nethera.io", AuthMode: "login"},
			},
		},
	)
	if len(groups) != 1 {
		t.Fatalf("expected one load-balanced group, got %#v", groups)
	}
	if got := strings.Join(groups[0].Machines, ", "); got != "homebox2, laptop" {
		t.Fatalf("unexpected grouped machines: %s", got)
	}
	if len(groups[0].Endpoints) != 1 || groups[0].Endpoints[0].Hostname != "n8n-web-t2k5.sg.nethera.io" {
		t.Fatalf("unexpected grouped endpoints: %#v", groups[0].Endpoints)
	}
}

func TestDeployEndpointLANURL(t *testing.T) {
	got := deployEndpointLANURL(deployEndpointSummary{
		PreferLAN: true,
		LANHost:   "192.168.1.50",
		LANPort:   8080,
	})
	if got != "http://192.168.1.50:8080" {
		t.Fatalf("deployEndpointLANURL() = %q, want http://192.168.1.50:8080", got)
	}

	if got := deployEndpointLANURL(deployEndpointSummary{PreferLAN: false, LANHost: "192.168.1.50", LANPort: 8080}); got != "" {
		t.Fatalf("expected no LAN URL when preferLan is false, got %q", got)
	}
}

func TestRequestMachinePairingOmitsRegionWhenNotExplicit(t *testing.T) {
	var body map[string]string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Fatalf("decode request body: %v", err)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"message":"Machine paired successfully.","regionCode":"sg"}`))
	}))
	defer server.Close()

	_, statusCode, errMsg, err := requestMachinePairing(server.URL, "token", "abc123", "home-gpu", "")
	if err != nil {
		t.Fatalf("requestMachinePairing returned error: %v", err)
	}
	if statusCode != http.StatusOK || errMsg != "" {
		t.Fatalf("unexpected status/error: status=%d errMsg=%q", statusCode, errMsg)
	}
	if _, ok := body["regionCode"]; ok {
		t.Fatalf("regionCode should be omitted when CLI did not explicitly override region: %#v", body)
	}
}

func TestRequestMachinePairingSendsExplicitRegionOverride(t *testing.T) {
	var body map[string]string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Fatalf("decode request body: %v", err)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"message":"Machine paired successfully.","regionCode":"sg"}`))
	}))
	defer server.Close()

	_, statusCode, errMsg, err := requestMachinePairing(server.URL, "token", "abc123", "home-gpu", "iad")
	if err != nil {
		t.Fatalf("requestMachinePairing returned error: %v", err)
	}
	if statusCode != http.StatusOK || errMsg != "" {
		t.Fatalf("unexpected status/error: status=%d errMsg=%q", statusCode, errMsg)
	}
	if got := body["regionCode"]; got != "iad" {
		t.Fatalf("regionCode = %q, want explicit override iad; body=%#v", got, body)
	}
}

func TestCollectManagedFilesReadsTextFile(t *testing.T) {
	dir := t.TempDir()
	manifestPath := filepath.Join(dir, "nethera.yml")
	if err := os.WriteFile(filepath.Join(dir, "nginx.conf"), []byte("server { listen 80; }\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	compose := `services:
  web:
    image: nginx:alpine
    nethera:
      files:
        nginx.conf:
          source: ./nginx.conf
          target: /etc/nginx/conf.d/default.conf
          mode: "0644"
`
	files, err := collectManagedFiles(compose, manifestPath)
	if err != nil {
		t.Fatalf("collectManagedFiles: %v", err)
	}
	if len(files) != 1 {
		t.Fatalf("expected one managed file, got %#v", files)
	}
	file := files[0]
	if file.ServiceName != "web" || file.Name != "nginx.conf" || file.Target != "/etc/nginx/conf.d/default.conf" || file.Mode != "0644" {
		t.Fatalf("unexpected managed file metadata: %#v", file)
	}
	if file.Content != "server { listen 80; }\n" {
		t.Fatalf("unexpected managed file content: %q", file.Content)
	}
}

func TestCollectManagedFilesRejectsUnsafeSource(t *testing.T) {
	dir := t.TempDir()
	manifestPath := filepath.Join(dir, "nethera.yml")
	compose := `services:
  web:
    image: nginx:alpine
    nethera:
      files:
        nginx.conf:
          source: ../nginx.conf
          target: /etc/nginx/conf.d/default.conf
`
	_, err := collectManagedFiles(compose, manifestPath)
	if err == nil || !strings.Contains(err.Error(), "must not contain ..") {
		t.Fatalf("expected unsafe source error, got %v", err)
	}
}

func TestCollectManagedFilesRejectsBinaryFile(t *testing.T) {
	dir := t.TempDir()
	manifestPath := filepath.Join(dir, "nethera.yml")
	if err := os.WriteFile(filepath.Join(dir, "config.bin"), []byte{0xff, 0xfe, 0xfd}, 0o644); err != nil {
		t.Fatal(err)
	}
	compose := `services:
  web:
    image: nginx:alpine
    nethera:
      files:
        config.bin:
          source: ./config.bin
          target: /etc/app/config.bin
`
	_, err := collectManagedFiles(compose, manifestPath)
	if err == nil || !strings.Contains(err.Error(), "not a UTF-8 text file") {
		t.Fatalf("expected binary file error, got %v", err)
	}
}

func TestSanitizeDeployLogLine(t *testing.T) {
	got := sanitizeDeployLogLine("\x1b[31mfailed\x1b[0m\r\nnext")
	if got != "failednext" {
		t.Fatalf("sanitize deploy log = %q", got)
	}
}

func TestStartDeployJobEventStream(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/deploy/job-1/events" {
			t.Fatalf("path = %s", r.URL.Path)
		}
		if r.Header.Get("Authorization") != "Bearer test-token" {
			t.Fatalf("authorization header = %q", r.Header.Get("Authorization"))
		}
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, "event: log\ndata: {\"type\":\"log\",\"line\":\"pulling image\"}\n\n")
		_, _ = io.WriteString(w, "event: end\ndata: {\"type\":\"end\",\"status\":\"succeeded\"}\n\n")
	}))
	defer server.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	results := startDeployJobEventStream(ctx, server.URL, "test-token", "job-1")
	first := <-results
	if first.Err != nil || first.Event == nil || first.Event.Line != "pulling image" {
		t.Fatalf("first stream result = %#v", first)
	}
	second := <-results
	if second.Err != nil || second.Event == nil || second.Event.Type != "end" || second.Event.Status != "succeeded" {
		t.Fatalf("second stream result = %#v", second)
	}
}

func TestFetchActiveDeployJob(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/deploy/active" || r.URL.Query().Get("appId") != "app-1" || r.URL.Query().Get("machineName") != "prod one" {
			t.Fatalf("unexpected active deploy request: %s", r.URL.String())
		}
		if r.Header.Get("Authorization") != "Bearer token" {
			t.Fatalf("authorization = %q", r.Header.Get("Authorization"))
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"job":{"id":"job-1","status":"deploying","machineName":"prod one","heartbeatAt":"2026-07-14T00:00:00Z"}}`)
	}))
	defer server.Close()

	job, err := fetchActiveDeployJob(server.URL, "token", "app-1", "prod one")
	if err != nil {
		t.Fatalf("fetch active deploy: %v", err)
	}
	if job == nil || job.ID != "job-1" || !deployJobIsActive(job.Status) {
		t.Fatalf("active job = %#v", job)
	}
	if deployJobIsActive("cancelled") {
		t.Fatal("cancelled job should not be active")
	}
}
