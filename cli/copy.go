package main

import (
	"bytes"
	"context"
	"crypto/tls"
	"encoding/binary"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/quic-go/quic-go"
)

const (
	copyALPN          = "nethera-copy/1"
	copyMaxFrameBytes = 1024 * 1024
)

type remoteSpec struct {
	Machine string
	Path    string
}

type copyFrame struct {
	Type        string          `json:"type"`
	SessionID   string          `json:"sessionId,omitempty"`
	Token       string          `json:"token,omitempty"`
	Operation   string          `json:"operation,omitempty"`
	RemotePath  string          `json:"remotePath,omitempty"`
	Path        string          `json:"path,omitempty"`
	Size        int64           `json:"size,omitempty"`
	IsDir       bool            `json:"isDir,omitempty"`
	ModTime     string          `json:"modifiedAt,omitempty"`
	Entries     []copyListEntry `json:"entries,omitempty"`
	Error       string          `json:"error,omitempty"`
	Truncated   bool            `json:"truncated,omitempty"`
	MoreEntries int             `json:"moreEntries,omitempty"`
}

type copyListEntry struct {
	Name      string          `json:"name"`
	Path      string          `json:"path"`
	Type      string          `json:"type"`
	Size      int64           `json:"size,omitempty"`
	ModTime   string          `json:"modifiedAt,omitempty"`
	Children  []copyListEntry `json:"children,omitempty"`
	Truncated bool            `json:"truncated,omitempty"`
	Error     string          `json:"error,omitempty"`
}

type syncEntry struct {
	Path    string
	Size    int64
	IsDir   bool
	ModTime time.Time
}

func runCopy(args []string) {
	fs := flag.NewFlagSet("copy", flag.ExitOnError)
	backendURL := fs.String("backend", defaultBackendURL(), "backend base URL")
	force := fs.Bool("force", false, "overwrite existing local files without prompting")
	fs.Parse(args)
	if fs.NArg() != 2 {
		fmt.Println("usage: neth copy <local-path> <machine>:/mnt/nethera/...")
		fmt.Println("       neth copy <machine>:/mnt/nethera/... <local-path>")
		os.Exit(1)
	}
	leftRemote, leftSpec := parseRemoteSpec(fs.Arg(0))
	rightRemote, rightSpec := parseRemoteSpec(fs.Arg(1))
	if leftRemote == rightRemote {
		fmt.Println("copy must be local-to-machine or machine-to-local")
		os.Exit(1)
	}
	token, err := loadAuthToken(*backendURL)
	if err != nil {
		fmt.Println(err)
		os.Exit(1)
	}
	if rightRemote {
		if err := runCopyUpload(*backendURL, token, fs.Arg(0), rightSpec); err != nil {
			fmt.Printf("copy failed: %v\n", err)
			os.Exit(1)
		}
		return
	}
	if leftRemote {
		if err := runCopyDownload(*backendURL, token, leftSpec, fs.Arg(1), *force); err != nil {
			fmt.Printf("copy failed: %v\n", err)
			os.Exit(1)
		}
		return
	}
}

func runSync(args []string) {
	fs := flag.NewFlagSet("sync", flag.ExitOnError)
	backendURL := fs.String("backend", defaultBackendURL(), "backend base URL")
	dryRun := fs.Bool("dry-run", false, "show what would change without copying files")
	fs.Parse(args)
	if fs.NArg() != 2 {
		fmt.Println("usage: neth sync <local-dir> <machine>:/mnt/nethera/...")
		fmt.Println("       neth sync <machine>:/mnt/nethera/... <local-dir>")
		os.Exit(1)
	}
	leftRemote, leftSpec := parseRemoteSpec(fs.Arg(0))
	rightRemote, rightSpec := parseRemoteSpec(fs.Arg(1))
	if leftRemote == rightRemote {
		fmt.Println("sync must be local-to-machine or machine-to-local")
		os.Exit(1)
	}
	token, err := loadAuthToken(*backendURL)
	if err != nil {
		fmt.Println(err)
		os.Exit(1)
	}
	if rightRemote {
		if err := runSyncUpload(*backendURL, token, fs.Arg(0), rightSpec, *dryRun); err != nil {
			fmt.Printf("sync failed: %v\n", err)
			os.Exit(1)
		}
		return
	}
	if leftRemote {
		if err := runSyncDownload(*backendURL, token, leftSpec, fs.Arg(1), *dryRun); err != nil {
			fmt.Printf("sync failed: %v\n", err)
			os.Exit(1)
		}
		return
	}
}

func runRemoteLS(args []string) {
	fs := flag.NewFlagSet("ls", flag.ExitOnError)
	backendURL := fs.String("backend", defaultBackendURL(), "backend base URL")
	fs.Parse(args)
	if fs.NArg() != 1 {
		fmt.Println("usage: neth ls <machine>:/mnt/nethera/...")
		os.Exit(1)
	}
	isRemote, spec := parseRemoteSpec(fs.Arg(0))
	if !isRemote {
		fmt.Println("path must look like <machine>:/mnt/nethera/...")
		os.Exit(1)
	}
	token, err := loadAuthToken(*backendURL)
	if err != nil {
		fmt.Println(err)
		os.Exit(1)
	}
	session, sessionToken, err := createCopySession(*backendURL, token, spec.Machine, "list", spec.Path, nil)
	if err != nil {
		fmt.Printf("failed to create listing session: %v\n", err)
		os.Exit(1)
	}
	session, err = waitForCopySessionReady(*backendURL, token, session.ID)
	if err != nil {
		fmt.Printf("failed waiting for machine: %v\n", err)
		os.Exit(1)
	}
	stream, closeFn, err := openCopyStream(session, sessionToken)
	if err != nil {
		fmt.Printf("failed to connect to machine: %v\n", err)
		os.Exit(1)
	}
	defer closeFn()
	if err := copyHandshake(stream, session, sessionToken); err != nil {
		fmt.Printf("copy handshake failed: %v\n", err)
		os.Exit(1)
	}
	frame, err := readCopyFrame(stream)
	if err != nil {
		fmt.Printf("listing failed: %v\n", err)
		os.Exit(1)
	}
	if frame.Type == "error" {
		fmt.Println(frame.Error)
		os.Exit(1)
	}
	if frame.Type != "list" {
		fmt.Printf("unexpected response: %s\n", frame.Type)
		os.Exit(1)
	}
	fmt.Printf("%s:%s\n\n", spec.Machine, spec.Path)
	printListEntries(frame.Entries, 0)
	if frame.Truncated {
		fmt.Printf("... %d more entries\n", frame.MoreEntries)
	}
}

func parseRemoteSpec(value string) (bool, remoteSpec) {
	left, right, ok := strings.Cut(value, ":")
	if !ok || strings.TrimSpace(left) == "" || !strings.HasPrefix(right, "/") {
		return false, remoteSpec{}
	}
	return true, remoteSpec{Machine: strings.TrimSpace(left), Path: strings.TrimSpace(right)}
}

func runCopyUpload(backendURL, token, localPath string, remote remoteSpec) error {
	info, err := os.Stat(localPath)
	if err != nil {
		return err
	}
	manifest, err := buildLocalManifest(localPath, info)
	if err != nil {
		return err
	}
	session, sessionToken, err := createCopySession(backendURL, token, remote.Machine, "upload", remote.Path, map[string]any{"entries": manifest})
	if err != nil {
		return err
	}
	fmt.Printf("Preparing copy to %s...\n", remote.Machine)
	session, err = waitForCopySessionReady(backendURL, token, session.ID)
	if err != nil {
		return err
	}
	stream, closeFn, err := openCopyStream(session, sessionToken)
	if err != nil {
		return err
	}
	defer closeFn()
	if err := copyHandshake(stream, session, sessionToken); err != nil {
		return err
	}
	fmt.Printf("Copying to %s:%s\n", remote.Machine, remote.Path)
	return sendUpload(stream, localPath, info)
}

func runCopyDownload(backendURL, token string, remote remoteSpec, localPath string, force bool) error {
	session, sessionToken, err := createCopySession(backendURL, token, remote.Machine, "download", remote.Path, nil)
	if err != nil {
		return err
	}
	fmt.Printf("Preparing download from %s...\n", remote.Machine)
	session, err = waitForCopySessionReady(backendURL, token, session.ID)
	if err != nil {
		return err
	}
	stream, closeFn, err := openCopyStream(session, sessionToken)
	if err != nil {
		return err
	}
	defer closeFn()
	if err := copyHandshake(stream, session, sessionToken); err != nil {
		return err
	}
	fmt.Printf("Copying from %s:%s\n", remote.Machine, remote.Path)
	return receiveDownload(stream, localPath, force)
}

func runSyncUpload(backendURL, token, localPath string, remote remoteSpec, dryRun bool) error {
	localPath = filepath.Clean(localPath)
	info, err := os.Stat(localPath)
	if err != nil {
		return err
	}
	if !info.IsDir() {
		return fmt.Errorf("sync source must be a directory")
	}
	fmt.Printf("Preparing sync to %s...\n", remote.Machine)
	localEntries, err := buildSyncLocalManifest(localPath)
	if err != nil {
		return err
	}
	remoteEntries, err := fetchRemoteSyncManifest(backendURL, token, remote)
	if err != nil {
		return err
	}
	plan := syncPlan(localEntries, remoteEntries)
	printSyncPlan(plan, "upload", dryRun)
	if dryRun || len(plan) == 0 {
		return nil
	}
	session, sessionToken, err := createCopySession(backendURL, token, remote.Machine, "upload", remote.Path, nil)
	if err != nil {
		return err
	}
	session, err = waitForCopySessionReady(backendURL, token, session.ID)
	if err != nil {
		return err
	}
	stream, closeFn, err := openCopyStream(session, sessionToken)
	if err != nil {
		return err
	}
	defer closeFn()
	if err := copyHandshake(stream, session, sessionToken); err != nil {
		return err
	}
	fmt.Printf("Syncing to %s:%s\n", remote.Machine, remote.Path)
	return sendUploadPlan(stream, localPath, plan)
}

func runSyncDownload(backendURL, token string, remote remoteSpec, localPath string, dryRun bool) error {
	localPath = filepath.Clean(localPath)
	fmt.Printf("Preparing sync from %s...\n", remote.Machine)
	remoteEntries, err := fetchRemoteSyncManifest(backendURL, token, remote)
	if err != nil {
		return err
	}
	localEntries := map[string]syncEntry{}
	if info, err := os.Stat(localPath); err == nil {
		if !info.IsDir() {
			return fmt.Errorf("sync destination must be a directory")
		}
		localEntries, err = buildSyncLocalManifest(localPath)
		if err != nil {
			return err
		}
	} else if !os.IsNotExist(err) {
		return err
	}
	plan := syncPlan(remoteEntries, localEntries)
	printSyncPlan(plan, "download", dryRun)
	if dryRun || len(plan) == 0 {
		return nil
	}
	paths := make([]any, 0, len(plan))
	for _, entry := range plan {
		paths = append(paths, entry.Path)
	}
	session, sessionToken, err := createCopySession(backendURL, token, remote.Machine, "sync-download", remote.Path, map[string]any{"paths": paths})
	if err != nil {
		return err
	}
	session, err = waitForCopySessionReady(backendURL, token, session.ID)
	if err != nil {
		return err
	}
	stream, closeFn, err := openCopyStream(session, sessionToken)
	if err != nil {
		return err
	}
	defer closeFn()
	if err := copyHandshake(stream, session, sessionToken); err != nil {
		return err
	}
	fmt.Printf("Syncing from %s:%s\n", remote.Machine, remote.Path)
	return receiveDownloadPlan(stream, localPath)
}

func fetchRemoteSyncManifest(backendURL, token string, remote remoteSpec) (map[string]syncEntry, error) {
	session, sessionToken, err := createCopySession(backendURL, token, remote.Machine, "sync-list", remote.Path, nil)
	if err != nil {
		return nil, err
	}
	session, err = waitForCopySessionReady(backendURL, token, session.ID)
	if err != nil {
		return nil, err
	}
	stream, closeFn, err := openCopyStream(session, sessionToken)
	if err != nil {
		return nil, err
	}
	defer closeFn()
	if err := copyHandshake(stream, session, sessionToken); err != nil {
		return nil, err
	}
	entries := map[string]syncEntry{}
	for {
		frame, err := readCopyFrame(stream)
		if err != nil {
			return nil, err
		}
		switch frame.Type {
		case "entry":
			entry := syncEntryFromFrame(frame)
			if entry.Path != "" {
				entries[entry.Path] = entry
			}
		case "done":
			return entries, nil
		case "error":
			return nil, fmt.Errorf(frame.Error)
		default:
			return nil, fmt.Errorf("unexpected sync-list frame %s", frame.Type)
		}
	}
}

func createCopySession(backendURL, token, machine, operation, remotePath string, manifest map[string]any) (copySessionInfo, string, error) {
	body, _ := json.Marshal(map[string]any{
		"machineId":  machine,
		"operation":  operation,
		"remotePath": remotePath,
		"manifest":   manifest,
	})
	req, err := http.NewRequest(http.MethodPost, strings.TrimRight(backendURL, "/")+"/api/copy-sessions", bytes.NewReader(body))
	if err != nil {
		return copySessionInfo{}, "", err
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return copySessionInfo{}, "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return copySessionInfo{}, "", formatJSONHTTPError(resp, "copy session request failed")
	}
	var envelope copySessionEnvelope
	if err := json.NewDecoder(resp.Body).Decode(&envelope); err != nil {
		return copySessionInfo{}, "", err
	}
	return envelope.Session, envelope.SessionToken, nil
}

func waitForCopySessionReady(backendURL, token, sessionID string) (copySessionInfo, error) {
	deadline := time.Now().Add(2 * time.Minute)
	for {
		req, err := http.NewRequest(http.MethodGet, strings.TrimRight(backendURL, "/")+"/api/copy-sessions/"+url.PathEscape(sessionID), nil)
		if err != nil {
			return copySessionInfo{}, err
		}
		req.Header.Set("Authorization", "Bearer "+token)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			return copySessionInfo{}, err
		}
		if resp.StatusCode != http.StatusOK {
			err := formatJSONHTTPError(resp, "copy session status failed")
			resp.Body.Close()
			return copySessionInfo{}, err
		}
		var envelope copySessionStatusEnvelope
		err = json.NewDecoder(resp.Body).Decode(&envelope)
		resp.Body.Close()
		if err != nil {
			return copySessionInfo{}, err
		}
		switch envelope.Session.Status {
		case "agent_ready":
			return envelope.Session, nil
		case "failed":
			return copySessionInfo{}, fmt.Errorf(envelope.Session.ErrorMessage)
		}
		if time.Now().After(deadline) {
			return copySessionInfo{}, fmt.Errorf("timed out waiting for machine agent")
		}
		time.Sleep(2 * time.Second)
	}
}

func openCopyStream(session copySessionInfo, token string) (io.ReadWriteCloser, func(), error) {
	addr := fmt.Sprintf("%s:%d", session.ListenerHost, session.ListenerPort)
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	tlsConfig := &tls.Config{InsecureSkipVerify: true, NextProtos: []string{copyALPN}, MinVersion: tls.VersionTLS13}
	conn, err := quic.DialAddr(ctx, addr, tlsConfig, &quic.Config{MaxIdleTimeout: 2 * time.Minute})
	cancel()
	if err != nil {
		return nil, nil, err
	}
	stream, err := conn.OpenStreamSync(context.Background())
	if err != nil {
		conn.CloseWithError(1, "stream failed")
		return nil, nil, err
	}
	closeFn := func() {
		stream.Close()
		conn.CloseWithError(0, "done")
	}
	return stream, closeFn, nil
}

func copyHandshake(stream io.ReadWriter, session copySessionInfo, token string) error {
	if err := writeCopyFrame(stream, copyFrame{Type: "hello", SessionID: session.ID, Token: token, Operation: session.Operation, RemotePath: session.RemotePath}); err != nil {
		return err
	}
	frame, err := readCopyFrame(stream)
	if err != nil {
		return err
	}
	if frame.Type == "error" {
		return fmt.Errorf(frame.Error)
	}
	if frame.Type != "ready" {
		return fmt.Errorf("unexpected copy response %s", frame.Type)
	}
	return nil
}

func buildLocalManifest(localPath string, info os.FileInfo) ([]map[string]any, error) {
	base := filepath.Dir(localPath)
	if info.IsDir() {
		base = localPath
	}
	entries := []map[string]any{}
	err := filepath.WalkDir(localPath, func(path string, entry os.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		rel, _ := filepath.Rel(base, path)
		if rel == "." {
			rel = entry.Name()
		}
		info, err := entry.Info()
		if err != nil {
			return err
		}
		entries = append(entries, map[string]any{"path": filepath.ToSlash(rel), "size": info.Size(), "isDir": entry.IsDir()})
		return nil
	})
	return entries, err
}

func buildSyncLocalManifest(localPath string) (map[string]syncEntry, error) {
	entries := map[string]syncEntry{}
	err := filepath.WalkDir(localPath, func(path string, entry os.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if path == localPath {
			return nil
		}
		info, err := entry.Info()
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(localPath, path)
		if err != nil {
			return err
		}
		rel = filepath.ToSlash(rel)
		entries[rel] = syncEntry{Path: rel, Size: info.Size(), IsDir: entry.IsDir(), ModTime: info.ModTime().UTC()}
		return nil
	})
	return entries, err
}

func syncEntryFromFrame(frame copyFrame) syncEntry {
	modTime := time.Time{}
	if strings.TrimSpace(frame.ModTime) != "" {
		modTime, _ = time.Parse(time.RFC3339Nano, strings.TrimSpace(frame.ModTime))
	}
	path := filepath.ToSlash(filepath.Clean(strings.TrimSpace(frame.Path)))
	if path == "." || strings.HasPrefix(path, "../") || path == ".." {
		path = ""
	}
	return syncEntry{Path: path, Size: frame.Size, IsDir: frame.IsDir, ModTime: modTime.UTC()}
}

func syncPlan(source, dest map[string]syncEntry) []syncEntry {
	planned := []syncEntry{}
	for path, sourceEntry := range source {
		destEntry, ok := dest[path]
		if !ok || syncEntryChanged(sourceEntry, destEntry) {
			planned = append(planned, sourceEntry)
		}
	}
	sortSyncEntries(planned)
	return planned
}

func syncEntryChanged(source, dest syncEntry) bool {
	if source.IsDir || dest.IsDir {
		return source.IsDir != dest.IsDir
	}
	if source.Size != dest.Size {
		return true
	}
	if source.ModTime.IsZero() || dest.ModTime.IsZero() {
		return false
	}
	return source.ModTime.After(dest.ModTime.Add(time.Second))
}

func sortSyncEntries(entries []syncEntry) {
	sort.Slice(entries, func(i, j int) bool {
		if entries[i].IsDir != entries[j].IsDir {
			return entries[i].IsDir
		}
		return entries[i].Path < entries[j].Path
	})
}

func printSyncPlan(plan []syncEntry, direction string, dryRun bool) {
	dirCount := 0
	fileCount := 0
	var bytes int64
	for _, entry := range plan {
		if entry.IsDir {
			dirCount++
		} else {
			fileCount++
			bytes += entry.Size
		}
	}
	fmt.Println("Sync plan:")
	fmt.Printf("  %d director%s to create/update\n", dirCount, pluralY(dirCount))
	fmt.Printf("  %d file%s to %s (%s)\n", fileCount, pluralS(fileCount), direction, copyFormatBytes(bytes))
	if dryRun {
		fmt.Println("Dry run; no files copied.")
	}
}

func sendUpload(stream io.ReadWriter, localPath string, info os.FileInfo) error {
	base := filepath.Dir(localPath)
	if info.IsDir() {
		base = localPath
	}
	if err := filepath.WalkDir(localPath, func(path string, entry os.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		rel, _ := filepath.Rel(base, path)
		if rel == "." {
			rel = entry.Name()
		}
		info, err := entry.Info()
		if err != nil {
			return err
		}
		frame := copyFrame{Type: "entry", Path: filepath.ToSlash(rel), Size: info.Size(), IsDir: entry.IsDir(), ModTime: info.ModTime().UTC().Format(time.RFC3339Nano)}
		if err := writeCopyFrame(stream, frame); err != nil {
			return err
		}
		if entry.IsDir() {
			return nil
		}
		file, err := os.Open(path)
		if err != nil {
			return err
		}
		fmt.Printf("  %s (%s)\n", filepath.ToSlash(rel), copyFormatBytes(info.Size()))
		_, copyErr := io.Copy(stream, file)
		closeErr := file.Close()
		if copyErr != nil {
			return copyErr
		}
		return closeErr
	}); err != nil {
		return err
	}
	if err := writeCopyFrame(stream, copyFrame{Type: "end"}); err != nil {
		return err
	}
	return expectCopyDone(stream)
}

func sendUploadPlan(stream io.ReadWriter, localRoot string, plan []syncEntry) error {
	for _, entry := range plan {
		frame := copyFrame{Type: "entry", Path: entry.Path, Size: entry.Size, IsDir: entry.IsDir, ModTime: entry.ModTime.UTC().Format(time.RFC3339Nano)}
		if err := writeCopyFrame(stream, frame); err != nil {
			return err
		}
		if entry.IsDir {
			continue
		}
		path := filepath.Join(localRoot, filepath.FromSlash(entry.Path))
		file, err := os.Open(path)
		if err != nil {
			return err
		}
		fmt.Printf("  %s (%s)\n", entry.Path, copyFormatBytes(entry.Size))
		_, copyErr := io.Copy(stream, file)
		closeErr := file.Close()
		if copyErr != nil {
			return copyErr
		}
		if closeErr != nil {
			return closeErr
		}
	}
	if err := writeCopyFrame(stream, copyFrame{Type: "end"}); err != nil {
		return err
	}
	return expectCopyDone(stream)
}

func receiveDownload(stream io.ReadWriter, localPath string, force bool) error {
	localPath = filepath.Clean(localPath)
	localInfo, localStatErr := os.Stat(localPath)
	localIsDir := localStatErr == nil && localInfo.IsDir()
	entryCount := 0
	for {
		frame, err := readCopyFrame(stream)
		if err != nil {
			return err
		}
		switch frame.Type {
		case "entry":
			entryCount += 1
			target := filepath.Clean(filepath.Join(localPath, frame.Path))
			if !localIsDir && !frame.IsDir && entryCount == 1 {
				target = localPath
			}
			if frame.IsDir {
				localIsDir = true
				if err := os.MkdirAll(target, 0o755); err != nil {
					return err
				}
				continue
			}
			if !force {
				if _, err := os.Stat(target); err == nil {
					overwrite, promptErr := promptYesNoDefaultNo(fmt.Sprintf("%s exists. Overwrite?", target))
					if promptErr != nil {
						return promptErr
					}
					if !overwrite {
						return fmt.Errorf("download cancelled")
					}
				}
			}
			if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
				return err
			}
			tmp := target + ".nethera-tmp"
			file, err := os.OpenFile(tmp, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o644)
			if err != nil {
				return err
			}
			fmt.Printf("  %s (%s)\n", frame.Path, copyFormatBytes(frame.Size))
			_, copyErr := io.CopyN(file, stream, frame.Size)
			closeErr := file.Close()
			if copyErr != nil {
				_ = os.Remove(tmp)
				return copyErr
			}
			if closeErr != nil {
				_ = os.Remove(tmp)
				return closeErr
			}
			if err := os.Rename(tmp, target); err != nil {
				_ = os.Remove(tmp)
				return err
			}
			applyLocalCopyModTime(target, frame.ModTime)
		case "done":
			return nil
		case "error":
			return fmt.Errorf(frame.Error)
		default:
			return fmt.Errorf("unexpected copy frame %s", frame.Type)
		}
	}
}

func receiveDownloadPlan(stream io.ReadWriter, localRoot string) error {
	if err := os.MkdirAll(localRoot, 0o755); err != nil {
		return err
	}
	for {
		frame, err := readCopyFrame(stream)
		if err != nil {
			return err
		}
		switch frame.Type {
		case "entry":
			target := filepath.Clean(filepath.Join(localRoot, filepath.FromSlash(frame.Path)))
			if !strings.HasPrefix(target, filepath.Clean(localRoot)+string(os.PathSeparator)) && target != filepath.Clean(localRoot) {
				return fmt.Errorf("download path escapes destination: %s", frame.Path)
			}
			if frame.IsDir {
				if err := os.MkdirAll(target, 0o755); err != nil {
					return err
				}
				continue
			}
			if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
				return err
			}
			tmp := target + ".nethera-tmp"
			file, err := os.OpenFile(tmp, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o644)
			if err != nil {
				return err
			}
			fmt.Printf("  %s (%s)\n", frame.Path, copyFormatBytes(frame.Size))
			_, copyErr := io.CopyN(file, stream, frame.Size)
			closeErr := file.Close()
			if copyErr != nil {
				_ = os.Remove(tmp)
				return copyErr
			}
			if closeErr != nil {
				_ = os.Remove(tmp)
				return closeErr
			}
			if err := os.Rename(tmp, target); err != nil {
				_ = os.Remove(tmp)
				return err
			}
			applyLocalCopyModTime(target, frame.ModTime)
		case "done":
			return nil
		case "error":
			return fmt.Errorf(frame.Error)
		default:
			return fmt.Errorf("unexpected sync-download frame %s", frame.Type)
		}
	}
}

func applyLocalCopyModTime(path string, value string) {
	if strings.TrimSpace(value) == "" {
		return
	}
	parsed, err := time.Parse(time.RFC3339Nano, strings.TrimSpace(value))
	if err != nil {
		return
	}
	_ = os.Chtimes(path, parsed, parsed)
}

func pluralS(count int) string {
	if count == 1 {
		return ""
	}
	return "s"
}

func pluralY(count int) string {
	if count == 1 {
		return "y"
	}
	return "ies"
}

func expectCopyDone(stream io.Reader) error {
	frame, err := readCopyFrame(stream)
	if err != nil {
		return err
	}
	if frame.Type == "error" {
		return fmt.Errorf(frame.Error)
	}
	if frame.Type != "done" {
		return fmt.Errorf("unexpected copy response %s", frame.Type)
	}
	return nil
}

func printListEntries(entries []copyListEntry, indent int) {
	prefix := strings.Repeat("  ", indent)
	for _, entry := range entries {
		name := entry.Name
		if entry.Type == "directory" {
			name += "/"
		}
		if entry.Type == "file" {
			fmt.Printf("%s%-48s %s\n", prefix, name, copyFormatBytes(entry.Size))
		} else {
			fmt.Printf("%s%s\n", prefix, name)
		}
		if entry.Error != "" {
			fmt.Printf("%s  [%s]\n", prefix, entry.Error)
		}
		if len(entry.Children) > 0 {
			printListEntries(entry.Children, indent+1)
			if entry.Truncated {
				fmt.Printf("%s  ...\n", prefix)
			}
		}
	}
}

func writeCopyFrame(w io.Writer, frame copyFrame) error {
	body, err := json.Marshal(frame)
	if err != nil {
		return err
	}
	if len(body) > copyMaxFrameBytes {
		return fmt.Errorf("copy frame is too large")
	}
	var header [4]byte
	binary.BigEndian.PutUint32(header[:], uint32(len(body)))
	if _, err := w.Write(header[:]); err != nil {
		return err
	}
	_, err = w.Write(body)
	return err
}

func readCopyFrame(r io.Reader) (copyFrame, error) {
	var header [4]byte
	if _, err := io.ReadFull(r, header[:]); err != nil {
		return copyFrame{}, err
	}
	size := binary.BigEndian.Uint32(header[:])
	if size == 0 || size > copyMaxFrameBytes {
		return copyFrame{}, fmt.Errorf("invalid copy frame size")
	}
	body := make([]byte, size)
	if _, err := io.ReadFull(r, body); err != nil {
		return copyFrame{}, err
	}
	var frame copyFrame
	if err := json.Unmarshal(body, &frame); err != nil {
		return copyFrame{}, err
	}
	return frame, nil
}

func formatJSONHTTPError(resp *http.Response, fallback string) error {
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	var errorBody apiErrorResponse
	if json.Unmarshal(body, &errorBody) == nil && strings.TrimSpace(errorBody.Error) != "" {
		return fmt.Errorf("%s", errorBody.Error)
	}
	return fmt.Errorf("%s with status %d", fallback, resp.StatusCode)
}

func copyFormatBytes(size int64) string {
	const unit = 1024
	if size < unit {
		return fmt.Sprintf("%d B", size)
	}
	value := float64(size)
	for _, suffix := range []string{"KiB", "MiB", "GiB", "TiB"} {
		value /= unit
		if value < unit {
			return fmt.Sprintf("%.1f %s", value, suffix)
		}
	}
	return fmt.Sprintf("%.1f PiB", value/unit)
}
