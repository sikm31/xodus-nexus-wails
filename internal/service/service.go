// Package service ports xodus-service.ts: the per-game Xodus account service
// and its authenticated loopback bridge.
package service

import (
	"context"
	"crypto/rand"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"syscall"
	"time"

	"xodus-nexus-wails/internal/diagnostics"
	"xodus-nexus-wails/internal/launcherenv"
)

type Bridge struct {
	Port int
	Key  string
	stop func()
}

func (b *Bridge) Stop() { b.stop() }

// StartBridge exposes socketPath on a loopback TCP port. Each client must send
// the random key first; a mismatch closes the connection.
func StartBridge(socketPath string) (*Bridge, error) {
	keyBytes := make([]byte, 32)
	if _, err := rand.Read(keyBytes); err != nil {
		return nil, err
	}
	key := hex.EncodeToString(keyBytes)
	expected := []byte(key)
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return nil, errors.New("Could not bind the Xodus loopback bridge.")
	}
	var mu sync.Mutex
	connections := map[net.Conn]bool{}
	track := func(conn net.Conn, add bool) {
		mu.Lock()
		if add {
			connections[conn] = true
		} else {
			delete(connections, conn)
		}
		mu.Unlock()
	}
	go func() {
		for {
			client, err := listener.Accept()
			if err != nil {
				return
			}
			track(client, true)
			go func() {
				defer func() { client.Close(); track(client, false) }()
				client.SetDeadline(time.Now().Add(10 * time.Second))
				supplied := make([]byte, len(expected))
				if _, err := io.ReadFull(client, supplied); err != nil || subtle.ConstantTimeCompare(supplied, expected) != 1 {
					return
				}
				client.SetDeadline(time.Time{})
				upstream, err := net.Dial("unix", socketPath)
				if err != nil {
					return
				}
				track(upstream, true)
				defer func() { upstream.Close(); track(upstream, false) }()
				done := make(chan struct{}, 2)
				go func() { io.Copy(upstream, client); done <- struct{}{} }()
				go func() { io.Copy(client, upstream); done <- struct{}{} }()
				<-done
			}()
		}
	}()
	return &Bridge{Port: listener.Addr().(*net.TCPAddr).Port, Key: key, stop: func() {
		listener.Close()
		mu.Lock()
		for conn := range connections {
			conn.Close()
		}
		mu.Unlock()
	}}, nil
}

func socketReady(socketPath string) bool {
	conn, err := net.DialTimeout("unix", socketPath, time.Second)
	if err != nil {
		return false
	}
	conn.Close()
	return true
}

func runtimeFeatures(winePath string) []string {
	root := filepath.Clean(filepath.Join(filepath.Dir(winePath), "..", ".."))
	data, err := os.ReadFile(filepath.Join(root, "XODUS_NEXUS_PATCH.json"))
	if err != nil {
		return nil
	}
	var metadata struct {
		Features []any `json:"features"`
	}
	json.Unmarshal(data, &metadata)
	var features []string
	for _, feature := range metadata.Features {
		if text, ok := feature.(string); ok {
			features = append(features, text)
		}
	}
	return features
}

func hasAll(features []string, required ...string) bool {
	for _, feature := range required {
		if !slices.Contains(features, feature) {
			return false
		}
	}
	return true
}

func CloudSaveRuntime(winePath string) bool {
	return hasAll(runtimeFeatures(winePath), "cloud-saves-pr28", "xodus-tcp-bridge-v1")
}

func StoreContentRuntime(winePath string) bool {
	return hasAll(runtimeFeatures(winePath), "xstore-entitlements-v1", "xpackage-content-v1", "xstore-package-license-v1", "xodus-tcp-bridge-v1", "cloud-saves-pr28")
}

func StoreDiscoveryRuntime(winePath string) bool {
	return hasAll(runtimeFeatures(winePath), "xstore-related-discovery-v1", "xstore-durable-license-v1", "xstore-entitlements-v1", "xodus-tcp-bridge-v1")
}

func UnbridgedCloudSaveRuntime(winePath string) bool {
	features := runtimeFeatures(winePath)
	return slices.Contains(features, "cloud-saves-pr28") && !slices.Contains(features, "xodus-tcp-bridge-v1")
}

// SelectedWinePath prefers the active managed runtime over a stale managed path.
func SelectedWinePath(configured, runtimeRoot, active string) string {
	if configured == "" {
		return active
	}
	relative, err := filepath.Rel(filepath.Join(runtimeRoot, "releases"), configured)
	managed := err == nil && relative != ".." && !strings.HasPrefix(relative, ".."+string(filepath.Separator)) && !filepath.IsAbs(relative)
	if managed && active != "" {
		return active
	}
	return configured
}

type Session struct {
	SocketPath string
	BridgePort int
	BridgeKey  string
	stop       func()
	once       sync.Once
}

func (s *Session) Stop() { s.once.Do(s.stop) }

// Start runs xodus-service next to backendBinary in a private runtime directory.
func Start(ctx context.Context, backendBinary, userData string, onStatus func(string)) (*Session, error) {
	binary := filepath.Join(filepath.Dir(backendBinary), "xodus-service")
	if info, err := os.Stat(binary); err != nil || info.Mode()&0o111 == 0 {
		return nil, errors.New("This Xodus backend has no xodus-service. Rebuild the managed backend to use Xbox cloud saves.")
	}
	sessionRoot := filepath.Join(userData, "service-sessions")
	if err := os.MkdirAll(sessionRoot, 0o700); err != nil {
		return nil, err
	}
	runtimeDir, err := os.MkdirTemp(sessionRoot, "game-")
	if err != nil {
		return nil, err
	}
	os.Chmod(runtimeDir, 0o700)
	socketPath := filepath.Join(runtimeDir, "xodus.sock")
	env := launcherenv.External(nil)
	env["XDG_RUNTIME_DIR"], env["XODUS_LOG"] = runtimeDir, "error"
	cmd := exec.Command(binary)
	cmd.Env = env.List()
	observation := diagnostics.ObserveChild("service")
	credentials := diagnostics.NewCredentialErrorReader()
	var stderrMu sync.Mutex
	cmd.Stdout = io.Discard
	cmd.Stderr = writer(func(b []byte) (int, error) {
		stderrMu.Lock()
		credentials.Push(b)
		stderrMu.Unlock()
		observation.Stderr(b)
		return len(b), nil
	})
	stdin, _ := cmd.StdinPipe()
	if err := cmd.Start(); err != nil {
		observation.SpawnError(err)
		os.RemoveAll(runtimeDir)
		return nil, fmt.Errorf("Could not start xodus-service: %s", err.Error())
	}
	exited := make(chan struct{})
	exitCode := -1
	go func() {
		cmd.Wait()
		if cmd.ProcessState != nil {
			exitCode = cmd.ProcessState.ExitCode()
		}
		observation.Exit(exitCode, "")
		close(exited)
	}()
	var bridge *Bridge
	session := &Session{SocketPath: socketPath}
	session.stop = func() {
		if bridge != nil {
			bridge.Stop()
		}
		select {
		case <-exited:
		default:
			cmd.Process.Signal(syscall.SIGINT)
			select {
			case <-exited:
			case <-time.After(4 * time.Second):
				cmd.Process.Signal(syscall.SIGTERM)
			}
		}
		stdin.Close()
		os.RemoveAll(runtimeDir)
	}
	stopOnCancel := context.AfterFunc(ctx, func() { cmd.Process.Signal(syscall.SIGINT) })
	defer stopOnCancel()
	fail := func(err error) (*Session, error) { session.Stop(); return nil, err }

	onStatus("Connecting the Xodus account service…")
	deadline := time.Now().Add(90 * time.Second)
	for time.Now().Before(deadline) {
		if ctx.Err() != nil {
			return fail(errors.New("Account service operation cancelled."))
		}
		if socketReady(socketPath) {
			if bridge, err = StartBridge(socketPath); err != nil {
				return fail(err)
			}
			diagnostics.Emit(diagnostics.Record{Operation: "service", Event: "validation", Stage: "ready"})
			onStatus("Xodus account service and private loopback bridge are ready.")
			session.BridgePort, session.BridgeKey = bridge.Port, bridge.Key
			return session, nil
		}
		select {
		case <-exited:
			stderrMu.Lock()
			message := credentials.Message
			stderrMu.Unlock()
			if message == "" {
				message = fmt.Sprintf("xodus-service exited with code %d. Check the stored Xodus account and network connection.", exitCode)
			}
			return fail(errors.New(message))
		case <-time.After(250 * time.Millisecond):
		}
	}
	diagnostics.Emit(diagnostics.Record{Operation: "service", Event: "validation", Stage: "timeout"})
	return fail(errors.New("xodus-service did not become ready within 90 seconds. Check the stored Xodus account and network connection."))
}

type writer func([]byte) (int, error)

func (w writer) Write(b []byte) (int, error) { return w(b) }
