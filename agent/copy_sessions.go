package main

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/binary"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"io"
	"math/big"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/quic-go/quic-go"
)

const (
	copyALPN          = "nethera-copy/1"
	copyAllowedRoot   = "/mnt/nethera"
	copyMaxFrameBytes = 1024 * 1024
	copyMaxList       = 2000
)

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

func handleCopySession(ctx context.Context, backendURL, machineToken string, session copySessionPayload) {
	if err := runCopySession(ctx, backendURL, machineToken, session); err != nil {
		fmt.Printf("copy session %s failed: %v\n", session.ID, err)
		_ = reportCopySessionComplete(backendURL, machineToken, session.ID, "failed", err.Error())
	}
}

func runCopySession(ctx context.Context, backendURL, machineToken string, session copySessionPayload) error {
	if strings.TrimSpace(session.ID) == "" || strings.TrimSpace(session.SessionToken) == "" {
		return fmt.Errorf("copy session missing id or token")
	}
	tlsConfig, err := copyTLSConfig()
	if err != nil {
		return err
	}
	listener, err := quic.ListenAddr("0.0.0.0:0", tlsConfig, &quic.Config{MaxIdleTimeout: 2 * time.Minute})
	if err != nil {
		return fmt.Errorf("start QUIC copy listener: %w", err)
	}
	defer listener.Close()

	udpAddr, _ := listener.Addr().(*net.UDPAddr)
	port := 0
	if udpAddr != nil {
		port = udpAddr.Port
	}
	host := "127.0.0.1"
	if addresses := collectLANAddresses(); len(addresses) > 0 {
		host = addresses[0]
	}
	if err := reportCopySessionReady(backendURL, machineToken, session.ID, host, port); err != nil {
		return err
	}
	fmt.Printf("copy session %s ready on %s:%d\n", session.ID, host, port)

	acceptCtx, cancel := context.WithDeadline(ctx, copySessionDeadline(session.ExpiresAt))
	defer cancel()
	conn, err := listener.Accept(acceptCtx)
	if err != nil {
		return fmt.Errorf("copy client did not connect: %w", err)
	}
	defer conn.CloseWithError(0, "copy session finished")
	stream, err := conn.AcceptStream(acceptCtx)
	if err != nil {
		return fmt.Errorf("copy stream failed: %w", err)
	}
	defer stream.Close()

	hello, err := readCopyFrame(stream)
	if err != nil {
		return err
	}
	if hello.Type != "hello" || hello.SessionID != session.ID || hello.Token != session.SessionToken || hello.Operation != session.Operation {
		_ = writeCopyFrame(stream, copyFrame{Type: "error", Error: "copy session authentication failed"})
		return fmt.Errorf("copy session authentication failed")
	}
	if hello.RemotePath != session.RemotePath {
		_ = writeCopyFrame(stream, copyFrame{Type: "error", Error: "remote path does not match approved session"})
		return fmt.Errorf("remote path does not match approved session")
	}
	remotePath, err := validateCopyPath(session.RemotePath)
	if err != nil {
		_ = writeCopyFrame(stream, copyFrame{Type: "error", Error: err.Error()})
		return err
	}
	if err := writeCopyFrame(stream, copyFrame{Type: "ready"}); err != nil {
		return err
	}

	switch session.Operation {
	case "upload":
		err = receiveUpload(stream, remotePath)
	case "download":
		err = sendDownload(stream, remotePath)
	case "list":
		err = sendList(stream, remotePath)
	case "sync-list":
		err = sendSyncList(stream, remotePath)
	case "sync-download":
		err = sendDownloadSelected(stream, remotePath, session.Manifest)
	default:
		err = fmt.Errorf("unsupported copy operation %s", session.Operation)
	}
	if err != nil {
		_ = writeCopyFrame(stream, copyFrame{Type: "error", Error: err.Error()})
		return err
	}
	_ = writeCopyFrame(stream, copyFrame{Type: "done"})
	return reportCopySessionComplete(backendURL, machineToken, session.ID, "succeeded", "")
}

func copySessionDeadline(value string) time.Time {
	if parsed, err := time.Parse(time.RFC3339, strings.TrimSpace(value)); err == nil {
		return parsed
	}
	return time.Now().Add(10 * time.Minute)
}

func copyTLSConfig() (*tls.Config, error) {
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		return nil, err
	}
	template := x509.Certificate{
		SerialNumber: big.NewInt(time.Now().UnixNano()),
		Subject:      pkix.Name{CommonName: "nethera-copy"},
		NotBefore:    time.Now().Add(-time.Minute),
		NotAfter:     time.Now().Add(time.Hour),
		KeyUsage:     x509.KeyUsageKeyEncipherment | x509.KeyUsageDigitalSignature,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
	}
	certDER, err := x509.CreateCertificate(rand.Reader, &template, &template, &key.PublicKey, key)
	if err != nil {
		return nil, err
	}
	certPEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: certDER})
	keyPEM := pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(key)})
	cert, err := tls.X509KeyPair(certPEM, keyPEM)
	if err != nil {
		return nil, err
	}
	return &tls.Config{Certificates: []tls.Certificate{cert}, NextProtos: []string{copyALPN}, MinVersion: tls.VersionTLS13}, nil
}

func validateCopyPath(path string) (string, error) {
	clean := filepath.Clean(strings.TrimSpace(path))
	if !strings.HasPrefix(clean, "/") || strings.Contains(clean, "\x00") {
		return "", fmt.Errorf("remote path must be absolute")
	}
	if clean != copyAllowedRoot && !strings.HasPrefix(clean, copyAllowedRoot+"/") {
		return "", fmt.Errorf("remote file operations are restricted to %s", copyAllowedRoot)
	}
	return clean, nil
}

func safeJoinRemote(base, relative string) (string, error) {
	relative = filepath.Clean(strings.TrimPrefix(relative, "/"))
	if relative == "." || strings.HasPrefix(relative, "..") || strings.Contains(relative, "\x00") {
		return "", fmt.Errorf("unsafe relative path %q", relative)
	}
	target := filepath.Clean(filepath.Join(base, relative))
	if target != copyAllowedRoot && !strings.HasPrefix(target, copyAllowedRoot+"/") {
		return "", fmt.Errorf("path escapes %s", copyAllowedRoot)
	}
	return target, nil
}

func receiveUpload(stream io.ReadWriter, remotePath string) error {
	if err := os.MkdirAll(remotePath, 0o755); err != nil {
		return err
	}
	for {
		frame, err := readCopyFrame(stream)
		if err != nil {
			return err
		}
		switch frame.Type {
		case "entry":
			target, err := safeJoinRemote(remotePath, frame.Path)
			if err != nil {
				return err
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
			applyCopyModTime(target, frame.ModTime)
		case "end":
			return nil
		default:
			return fmt.Errorf("unexpected upload frame %s", frame.Type)
		}
	}
}

func sendDownload(stream io.ReadWriter, remotePath string) error {
	info, err := os.Stat(remotePath)
	if err != nil {
		return err
	}
	base := filepath.Dir(remotePath)
	if info.IsDir() {
		base = remotePath
	}
	return filepath.WalkDir(remotePath, func(path string, entry os.DirEntry, walkErr error) error {
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
		frame := copyFrame{Type: "entry", Path: rel, IsDir: entry.IsDir(), Size: info.Size(), ModTime: info.ModTime().UTC().Format(time.RFC3339Nano)}
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
		_, copyErr := io.Copy(stream, file)
		closeErr := file.Close()
		if copyErr != nil {
			return copyErr
		}
		return closeErr
	})
}

func sendDownloadSelected(stream io.ReadWriter, remotePath string, manifest map[string]any) error {
	paths := manifestPathSet(manifest)
	if len(paths) == 0 {
		return nil
	}
	for rel := range paths {
		target, err := safeJoinRemote(remotePath, rel)
		if err != nil {
			return err
		}
		info, err := os.Stat(target)
		if err != nil {
			return err
		}
		frame := copyFrame{Type: "entry", Path: filepath.ToSlash(filepath.Clean(rel)), IsDir: info.IsDir(), Size: info.Size(), ModTime: info.ModTime().UTC().Format(time.RFC3339Nano)}
		if err := writeCopyFrame(stream, frame); err != nil {
			return err
		}
		if info.IsDir() {
			continue
		}
		file, err := os.Open(target)
		if err != nil {
			return err
		}
		_, copyErr := io.Copy(stream, file)
		closeErr := file.Close()
		if copyErr != nil {
			return copyErr
		}
		if closeErr != nil {
			return closeErr
		}
	}
	return nil
}

func manifestPathSet(manifest map[string]any) map[string]bool {
	out := map[string]bool{}
	raw, ok := manifest["paths"]
	if !ok {
		return out
	}
	items, ok := raw.([]any)
	if !ok {
		return out
	}
	for _, item := range items {
		path := filepath.ToSlash(filepath.Clean(strings.TrimSpace(fmt.Sprint(item))))
		if path == "." || path == "" || strings.HasPrefix(path, "../") || path == ".." || strings.Contains(path, "\x00") {
			continue
		}
		out[path] = true
	}
	return out
}

func applyCopyModTime(path string, value string) {
	if strings.TrimSpace(value) == "" {
		return
	}
	parsed, err := time.Parse(time.RFC3339Nano, strings.TrimSpace(value))
	if err != nil {
		return
	}
	_ = os.Chtimes(path, parsed, parsed)
}

func sendList(stream io.ReadWriter, remotePath string) error {
	entries, truncated, more, err := buildCopyList(remotePath)
	if err != nil {
		return err
	}
	return writeCopyFrame(stream, copyFrame{Type: "list", RemotePath: remotePath, Entries: entries, Truncated: truncated, MoreEntries: more})
}

func sendSyncList(stream io.ReadWriter, remotePath string) error {
	info, err := os.Stat(remotePath)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return err
	}
	if !info.IsDir() {
		return fmt.Errorf("sync path must be a directory")
	}
	return filepath.WalkDir(remotePath, func(path string, entry os.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if path == remotePath {
			return nil
		}
		info, err := entry.Info()
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(remotePath, path)
		if err != nil {
			return err
		}
		frame := copyFrame{
			Type:    "entry",
			Path:    filepath.ToSlash(rel),
			IsDir:   entry.IsDir(),
			Size:    info.Size(),
			ModTime: info.ModTime().UTC().Format(time.RFC3339Nano),
		}
		return writeCopyFrame(stream, frame)
	})
}

func buildCopyList(remotePath string) ([]copyListEntry, bool, int, error) {
	entries, err := os.ReadDir(remotePath)
	if err != nil {
		return nil, false, 0, err
	}
	truncated := len(entries) > copyMaxList
	more := 0
	if truncated {
		more = len(entries) - copyMaxList
		entries = entries[:copyMaxList]
	}
	result := make([]copyListEntry, 0, len(entries))
	expand := len(entries) <= 10
	for _, entry := range entries {
		path := filepath.Join(remotePath, entry.Name())
		item := listEntry(path, entry)
		if expand && entry.IsDir() {
			children, childTruncated, _, childErr := buildCopyListDepth(path, 1, 200)
			if childErr != nil {
				item.Error = childErr.Error()
			} else {
				item.Children = children
				item.Truncated = childTruncated
			}
		}
		result = append(result, item)
	}
	return result, truncated, more, nil
}

func buildCopyListDepth(remotePath string, depth int, maxEntries int) ([]copyListEntry, bool, int, error) {
	if depth <= 0 {
		return nil, false, 0, nil
	}
	entries, err := os.ReadDir(remotePath)
	if err != nil {
		return nil, false, 0, err
	}
	truncated := len(entries) > maxEntries
	more := 0
	if truncated {
		more = len(entries) - maxEntries
		entries = entries[:maxEntries]
	}
	result := make([]copyListEntry, 0, len(entries))
	for _, entry := range entries {
		item := listEntry(filepath.Join(remotePath, entry.Name()), entry)
		result = append(result, item)
	}
	return result, truncated, more, nil
}

func listEntry(path string, entry os.DirEntry) copyListEntry {
	info, err := entry.Info()
	itemType := "file"
	if entry.IsDir() {
		itemType = "directory"
	}
	item := copyListEntry{Name: entry.Name(), Path: path, Type: itemType}
	if err == nil {
		item.Size = info.Size()
		item.ModTime = info.ModTime().UTC().Format(time.RFC3339)
	}
	return item
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

func reportCopySessionReady(backendURL, machineToken, sessionID, host string, port int) error {
	payload, _ := json.Marshal(map[string]any{"listenerHost": host, "listenerPort": port})
	return postAgentCopySession(backendURL, machineToken, sessionID, "ready", payload)
}

func reportCopySessionComplete(backendURL, machineToken, sessionID, status, message string) error {
	payload, _ := json.Marshal(map[string]any{"status": status, "errorMessage": message})
	return postAgentCopySession(backendURL, machineToken, sessionID, "complete", payload)
}

func postAgentCopySession(backendURL, machineToken, sessionID, action string, payload []byte) error {
	req, err := http.NewRequest(http.MethodPost, strings.TrimRight(backendURL, "/")+"/api/agent/copy-sessions/"+sessionID+"/"+action, bytes.NewReader(payload))
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+machineToken)
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		body, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("copy session %s failed with status %d: %s", action, resp.StatusCode, summarizeBody(body))
	}
	return nil
}
