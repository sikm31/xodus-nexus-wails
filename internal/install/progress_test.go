package install

import (
	"encoding/json"
	"errors"
	"io"
	"os"
	"strings"
	"testing"

	"xodus-nexus-wails/internal/shared"
)

func record(patch map[string]any) string {
	value := map[string]any{"schema": 1, "event": "progress", "sequence": 1, "phase": "downloading", "completedBytes": 25,
		"totalBytes": 100, "bytesPerSecond": 1048576, "paused": false, "canPause": true}
	for k, v := range patch {
		value[k] = v
	}
	encoded, _ := json.Marshal(value)
	return ProgressPrefix + string(encoded)
}

func TestProgressRecords(t *testing.T) {
	progress := NewProgress()
	patch, ok := progress.Update(record(nil))
	if !ok || *patch.Progress != 25 || patch.TransferRate != "1.00 MiB/s" || !patch.CanPause {
		t.Errorf("patch = %+v", patch)
	}
	if _, ok := progress.Update("game.bin 1 MiB/1 MiB 100%"); ok {
		t.Error("log text accepted")
	}
	if _, ok := progress.Update(record(nil)); ok {
		t.Error("stale sequence accepted")
	}
	if patch, _ := progress.Update(record(map[string]any{"sequence": 2, "completedBytes": 50})); *patch.Progress != 50 {
		t.Error("progress update")
	}

	unknown := NewProgress()
	if patch, _ := unknown.Update(record(map[string]any{"phase": "initializing", "totalBytes": nil, "completedBytes": 0, "canPause": false})); patch.Progress != nil {
		t.Error("unknown total must not invent progress")
	}
	if patch, _ := unknown.Update(record(map[string]any{"sequence": 2, "completedBytes": 100, "phase": "finalizing", "canPause": false})); *patch.Progress != 99.9 || unknown.Terminal != "" {
		t.Error("finalizing caps at 99.9")
	}
	unknown.Update(record(map[string]any{"sequence": 3, "phase": "complete", "completedBytes": 100, "canPause": false}))
	if _, ok := unknown.Update(record(map[string]any{"sequence": 4})); unknown.Terminal != "complete" || ok {
		t.Error("terminal records end the stream")
	}
	for _, patch := range []map[string]any{{"schema": 2}, {"sequence": -1}, {"completedBytes": -1}, {"totalBytes": 5}, {"totalBytes": "100"},
		{"completedBytes": float64(1<<53) + 2}, {"bytesPerSecond": "NaN"}, {"phase": "invented"}} {
		if _, ok := NewProgress().Update(record(patch)); ok {
			t.Errorf("accepted %v", patch)
		}
	}
	paused := NewProgress()
	if patch, _ := paused.Update(record(map[string]any{"paused": true})); patch.Status != "paused" || *patch.Progress != 25 || patch.TransferRate != "" {
		t.Errorf("paused = %+v", patch)
	}
	if patch, _ := paused.Update(record(map[string]any{"sequence": 2})); patch.Status != "running" {
		t.Error("resume")
	}
	paused.Update(record(map[string]any{"sequence": 3, "phase": "failed", "canPause": false}))
	if paused.Terminal != "failed" {
		t.Error("failure terminal")
	}
}

type port struct {
	supported bool
	fail      error
	shown     []string
}

func (p *port) Supported() bool { return p.supported }
func (p *port) Show(title, body string) error {
	p.shown = append(p.shown, title+"|"+body)
	return p.fail
}

func TestCompletionNotifications(t *testing.T) {
	task := shared.DownloadTask{ID: "synthetic", Title: "Sample game", Status: "complete"}
	for _, status := range []string{"queued", "running", "paused", "failed", "cancelled"} {
		p := &port{supported: true}
		other := task
		other.Status = status
		if NewCompletionNotifications(p).Complete(other) || len(p.shown) != 0 {
			t.Errorf("%s notified", status)
		}
	}
	for kind, title := range map[string]string{"install": "Download complete", "update": "Game update complete", "content": "Content download complete"} {
		p := &port{supported: true}
		notifications := NewCompletionNotifications(p)
		value := task
		value.Kind = kind
		if !notifications.Complete(value) || notifications.Complete(value) || len(p.shown) != 1 || p.shown[0] != title+"|Sample game has finished downloading." {
			t.Errorf("%s: %v", kind, p.shown)
		}
	}
	if NewCompletionNotifications(&port{}).Complete(task) {
		t.Error("unsupported desktop")
	}
	if NewCompletionNotifications(&port{supported: true, fail: errors.New("Notification service failed")}).Complete(task) {
		t.Error("failed notification reported as shown")
	}
}

func TestBandwidthControls(t *testing.T) {
	controls := NewBandwidthControls()
	readers := make([]*os.File, 2)
	writers := make([]*os.File, 2)
	for i := range 2 {
		readers[i], writers[i], _ = os.Pipe()
		controls.Attach(string(rune('0'+i)), writers[i])
	}
	if err := controls.Apply(0.5); err != nil {
		t.Fatal(err)
	}
	controls.Detach("0")
	if err := controls.Apply(0.0); err != nil {
		t.Fatal(err)
	}
	for _, w := range writers {
		w.Close()
	}
	first, _ := io.ReadAll(readers[0])
	second, _ := io.ReadAll(readers[1])
	if strings.Count(string(first), "\n") != 1 || !strings.Contains(string(first), `"kibPerSecond":512`) ||
		strings.Count(string(second), "\n") != 2 || !strings.HasSuffix(string(second), `"kibPerSecond":0}`+"\n") {
		t.Errorf("frames %q / %q", first, second)
	}
	closedR, closedW, _ := os.Pipe()
	closedR.Close()
	closedW.Close()
	controls = NewBandwidthControls()
	controls.Attach("closed", closedW)
	if err := controls.Apply(1.0); err == nil || !strings.Contains(err.Error(), "no longer accepting") {
		t.Errorf("closed: %v", err)
	}
	controls.Detach("closed")
	if err := controls.Apply(0.0); err != nil {
		t.Error(err)
	}
	if !IsBandwidthOnlyPatch(map[string]any{"downloadLimitMiB": 0.0}) || IsBandwidthOnlyPatch(map[string]any{}) ||
		IsBandwidthOnlyPatch(map[string]any{"market": "US"}) || IsBandwidthOnlyPatch(map[string]any{"downloadLimitMiB": 2.0, "market": "GB"}) {
		t.Error("bandwidth-only patch detection")
	}
}
