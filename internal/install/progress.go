package install

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"math"
	"os"
	"strings"
	"sync"

	"xodus-nexus-wails/internal/shared"
)

const ProgressPrefix = "NEXUS_PROGRESS "

var phases = map[string]string{
	"initializing": "Preparing download", "metadata": "Reading package metadata",
	"downloading": "Downloading and extracting game files", "finalizing": "Finalizing installation",
	"complete": "Installation complete", "failed": "Installation failed",
}

// ProgressPatch is a partial DownloadTask update.
type ProgressPatch struct {
	Progress        *float64
	DownloadedBytes float64
	TotalBytes      *float64
	TransferRate    string
	Phase           string
	Status          string
	CanPause        bool
}

// Patch returns the update as a state patch; nil values clear fields.
func (p ProgressPatch) Patch() map[string]any {
	patch := map[string]any{"downloadedBytes": p.DownloadedBytes, "phase": p.Phase, "status": p.Status, "canPause": p.CanPause, "controlPending": false,
		"progress": nil, "totalBytes": nil, "transferRate": nil}
	if p.Progress != nil {
		patch["progress"] = *p.Progress
	}
	if p.TotalBytes != nil {
		patch["totalBytes"] = *p.TotalBytes
	}
	if p.TransferRate != "" {
		patch["transferRate"] = p.TransferRate
	}
	return patch
}

const maxSafeInteger = 1<<53 - 1

func counter(raw json.RawMessage) (float64, bool) {
	var value float64
	text := strings.TrimSpace(string(raw))
	if text == "" || text[0] == '"' || json.Unmarshal(raw, &value) != nil {
		return 0, false
	}
	return value, value >= 0 && value <= maxSafeInteger && value == math.Trunc(value)
}

// Progress accepts machine-readable v1 records only and never infers percentages from log text.
type Progress struct {
	sequence float64
	Terminal string // complete | failed
}

func NewProgress() *Progress { return &Progress{sequence: -1} }

func (p *Progress) Update(line string) (ProgressPatch, bool) {
	if !strings.HasPrefix(line, ProgressPrefix) || shared.UTF16Len(line) > 4096 || p.Terminal != "" {
		return ProgressPatch{}, false
	}
	var value map[string]json.RawMessage
	if json.Unmarshal([]byte(line[len(ProgressPrefix):]), &value) != nil || value == nil {
		return ProgressPatch{}, false
	}
	str := func(key string) string {
		var s string
		if raw := strings.TrimSpace(string(value[key])); strings.HasPrefix(raw, `"`) {
			json.Unmarshal(value[key], &s)
		}
		return s
	}
	boolean := func(key string) (bool, bool) {
		switch strings.TrimSpace(string(value[key])) {
		case "true":
			return true, true
		case "false":
			return false, true
		}
		return false, false
	}
	sequence, sequenceOK := counter(value["sequence"])
	completed, completedOK := counter(value["completedBytes"])
	rate, rateOK := counter(value["bytesPerSecond"])
	paused, pausedOK := boolean("paused")
	canPause, canPauseOK := boolean("canPause")
	var total *float64
	totalOK := strings.TrimSpace(string(value["totalBytes"])) == "null"
	if !totalOK {
		if t, ok := counter(value["totalBytes"]); ok {
			total, totalOK = &t, true
		}
	}
	phase := str("phase")
	if strings.TrimSpace(string(value["schema"])) != "1" || str("event") != "progress" || !sequenceOK || sequence <= p.sequence ||
		phases[phase] == "" || !completedOK || !totalOK || (total != nil && completed > *total) || !rateOK || !pausedOK || !canPauseOK {
		return ProgressPatch{}, false
	}
	p.sequence = sequence
	if phase == "complete" || phase == "failed" {
		p.Terminal = phase
	}
	patch := ProgressPatch{DownloadedBytes: completed, TotalBytes: total, Phase: phases[phase], Status: "running", CanPause: canPause && phase == "downloading"}
	if total != nil && *total > 0 {
		progress := math.Min(99.9, completed / *total * 100)
		patch.Progress = &progress
	}
	if !paused && rate != 0 {
		patch.TransferRate = fmt.Sprintf("%.2f MiB/s", rate/1048576)
	}
	if paused {
		patch.Phase, patch.Status = "Paused", "paused"
	}
	return patch, true
}

// BandwidthControls tracks active CLI stdin channels only. Removing a channel
// never changes its process.
type BandwidthControls struct {
	mu       sync.Mutex
	channels map[string]io.Writer
}

func NewBandwidthControls() *BandwidthControls {
	return &BandwidthControls{channels: map[string]io.Writer{}}
}

func (b *BandwidthControls) Attach(id string, stdin io.Writer) {
	b.mu.Lock()
	b.channels[id] = stdin
	b.mu.Unlock()
}

func (b *BandwidthControls) Detach(id string) {
	b.mu.Lock()
	delete(b.channels, id)
	b.mu.Unlock()
}

func (b *BandwidthControls) Apply(limitMiB any) error {
	command, err := shared.DownloadLimitCommand(limitMiB)
	if err != nil {
		return err
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	var failures []error
	for _, stdin := range b.channels {
		if _, err := io.WriteString(stdin, command); err != nil {
			if errors.Is(err, os.ErrClosed) || errors.Is(err, io.ErrClosedPipe) {
				err = errors.New("The active download is no longer accepting controls.")
			}
			failures = append(failures, err)
		}
	}
	return errors.Join(failures...)
}

// IsBandwidthOnlyPatch reports a settings patch that only changes the download limit.
func IsBandwidthOnlyPatch(patch map[string]any) bool {
	_, ok := patch["downloadLimitMiB"]
	return len(patch) == 1 && ok
}

type NotificationPort interface {
	Supported() bool
	Show(title, body string) error
}

// CompletionNotifications notify only newly completed jobs, never restored
// history or failed/cancelled tasks. Desktop notification failures must not
// change installation results.
type CompletionNotifications struct {
	port     NotificationPort
	mu       sync.Mutex
	notified []string
	seen     map[string]bool
}

func NewCompletionNotifications(port NotificationPort) *CompletionNotifications {
	return &CompletionNotifications{port: port, seen: map[string]bool{}}
}

func (n *CompletionNotifications) Complete(task shared.DownloadTask) bool {
	n.mu.Lock()
	if task.Status != "complete" || n.seen[task.ID] {
		n.mu.Unlock()
		return false
	}
	n.seen[task.ID] = true
	n.notified = append(n.notified, task.ID)
	if len(n.notified) > 256 {
		delete(n.seen, n.notified[0])
		n.notified = n.notified[1:]
	}
	n.mu.Unlock()
	if !n.port.Supported() {
		return false
	}
	title := "Download complete"
	switch task.Kind {
	case "update":
		title = "Game update complete"
	case "content":
		title = "Content download complete"
	}
	if err := n.port.Show(title, task.Title+" has finished downloading."); err != nil {
		log.Printf("Could not show download completion notification: %v", err)
		return false
	}
	return true
}
