package service

import (
	"context"
	"encoding/json"
	"io"
	"net"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func TestServiceErrors(t *testing.T) {
	for _, mode := range []string{"credential", "unknown", "stdout"} {
		root := t.TempDir()
		marker := "NEXUS_CREDENTIAL_ERROR_V1 keyring-unavailable"
		if mode == "unknown" {
			marker = "NEXUS_CREDENTIAL_ERROR_V1 private-secret"
		}
		stream := ">&2"
		if mode == "stdout" {
			stream = ""
		}
		script := "#!/bin/sh\necho private-account-token >&2\necho '" + marker + "' " + stream + "\nexit 1\n"
		os.WriteFile(filepath.Join(root, "xodus-service"), []byte(script), 0o700)
		_, err := Start(context.Background(), filepath.Join(root, "xodus-cli"), filepath.Join(root, "profile"), func(string) {})
		want := "xodus-service exited with code 1"
		if mode == "credential" {
			want = "system keyring could not be accessed"
		}
		if err == nil || !strings.Contains(err.Error(), want) || regexp.MustCompile(`private-account|private-secret`).MatchString(err.Error()) {
			t.Errorf("%s: %v", mode, err)
		}
		if entries, _ := os.ReadDir(filepath.Join(root, "profile/service-sessions")); len(entries) != 0 {
			t.Errorf("%s: session directory left behind", mode)
		}
	}
}

func TestRuntimeCapabilities(t *testing.T) {
	root := t.TempDir()
	wine := filepath.Join(root, "files/bin/wine")
	write := func(features []string) {
		data, _ := json.Marshal(map[string]any{"features": features})
		os.WriteFile(filepath.Join(root, "XODUS_NEXUS_PATCH.json"), data, 0o644)
	}
	if StoreContentRuntime(wine) || StoreDiscoveryRuntime(wine) {
		t.Error("no metadata, no capability")
	}
	content := []string{"xstore-entitlements-v1", "xpackage-content-v1", "xstore-package-license-v1", "xodus-tcp-bridge-v1", "cloud-saves-pr28"}
	for _, missing := range content {
		write(slices.DeleteFunc(slices.Clone(content), func(f string) bool { return f == missing }))
		if StoreContentRuntime(wine) {
			t.Errorf("content without %s", missing)
		}
	}
	write(content)
	if !StoreContentRuntime(wine) || !CloudSaveRuntime(wine) || UnbridgedCloudSaveRuntime(wine) {
		t.Error("full content runtime")
	}
	discovery := []string{"xstore-related-discovery-v1", "xstore-durable-license-v1", "xstore-entitlements-v1", "xodus-tcp-bridge-v1"}
	for _, missing := range discovery {
		write(slices.DeleteFunc(slices.Clone(discovery), func(f string) bool { return f == missing }))
		if StoreDiscoveryRuntime(wine) {
			t.Errorf("discovery without %s", missing)
		}
	}
	write(discovery)
	if !StoreDiscoveryRuntime(wine) {
		t.Error("full discovery runtime")
	}
}

func TestSelectedWinePath(t *testing.T) {
	root, active := "/data/runtimes", "/data/runtimes/releases/new/files/bin/wine"
	if SelectedWinePath("/data/runtimes/releases/old/files/bin/wine", root, active) != active ||
		SelectedWinePath("/custom/global/wine", root, active) != "/custom/global/wine" || SelectedWinePath("", root, active) != active {
		t.Error("wine selection")
	}
}

func TestBridge(t *testing.T) {
	socket := filepath.Join(t.TempDir(), "backend.sock")
	listener, err := net.Listen("unix", socket)
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	var connections atomic.Int32
	go func() {
		for {
			conn, err := listener.Accept()
			if err != nil {
				return
			}
			connections.Add(1)
			go io.Copy(conn, conn)
		}
	}()
	bridge, err := StartBridge(socket)
	if err != nil {
		t.Fatal(err)
	}
	defer bridge.Stop()
	client, _ := net.Dial("tcp", net.JoinHostPort("127.0.0.1", strconv.Itoa(bridge.Port)))
	client.Write([]byte(bridge.Key + "ping"))
	reply := make([]byte, 4)
	client.SetDeadline(time.Now().Add(2 * time.Second))
	if _, err := io.ReadFull(client, reply); err != nil || string(reply) != "ping" {
		t.Errorf("reply = %q, %v", reply, err)
	}
	client.Close()
	rejected, _ := net.Dial("tcp", net.JoinHostPort("127.0.0.1", strconv.Itoa(bridge.Port)))
	rejected.Write([]byte(strings.Repeat("0", 64) + "ping"))
	rejected.SetDeadline(time.Now().Add(2 * time.Second))
	if n, _ := rejected.Read(make([]byte, 4)); n != 0 {
		t.Error("unauthenticated client was forwarded")
	}
	if connections.Load() != 1 {
		t.Errorf("backend connections = %d", connections.Load())
	}
}
