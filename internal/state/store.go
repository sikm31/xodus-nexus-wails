// Package state ports store.ts: the persisted launcher state and inspection of
// installed game directories.
package state

import (
	"encoding/json"
	"errors"
	"io/fs"
	"log"
	"os"
	"path/filepath"
	"slices"
	"sync"

	"xodus-nexus-wails/internal/shared"
)

type persisted struct {
	Games     []shared.Game         `json:"games"`
	Downloads []shared.DownloadTask `json:"downloads"`
	Settings  shared.Settings       `json:"settings"`
}

func DefaultSettings() shared.Settings {
	home, _ := os.UserHomeDir()
	zero := 0.0
	return shared.Settings{
		InstallRoot: filepath.Join(home, "Games", "Xodus Nexus Wails"), Market: "neutral", ParallelDownloads: 4, DownloadLimitMiB: &zero,
		RuntimeRoot: filepath.Join(home, ".local", "share", "xodus-nexus", "runtimes"),
	}
}

// Patch is a partial JSON object. A nil value removes the field, like
// assigning undefined in TypeScript.
type Patch map[string]any

// apply overlays patch onto target through its JSON form.
func apply[T any](target *T, patch Patch) error {
	encoded, err := json.Marshal(target)
	if err != nil {
		return err
	}
	fields := map[string]any{}
	if err := json.Unmarshal(encoded, &fields); err != nil {
		return err
	}
	for key, value := range patch {
		if value == nil {
			delete(fields, key)
		} else {
			fields[key] = value
		}
	}
	merged, err := json.Marshal(fields)
	if err != nil {
		return err
	}
	var out T
	if err := json.Unmarshal(merged, &out); err != nil {
		return err
	}
	*target = out
	return nil
}

// ToPatch converts a typed value into a patch of its non-empty fields.
func ToPatch(value any) Patch {
	encoded, _ := json.Marshal(value)
	patch := Patch{}
	json.Unmarshal(encoded, &patch)
	return patch
}

func active(status string) bool {
	return status == "running" || status == "queued" || status == "paused"
}

type Store struct {
	file   string
	mu     sync.Mutex
	writes sync.Mutex
	state  persisted
}

func New(userData string) *Store {
	return &Store{file: filepath.Join(userData, "launcher-state.json"),
		state: persisted{Games: []shared.Game{}, Downloads: []shared.DownloadTask{}, Settings: DefaultSettings()}}
}

func (s *Store) Load() {
	data, err := os.ReadFile(s.file)
	if err != nil {
		if !errors.Is(err, fs.ErrNotExist) {
			log.Printf("State could not be loaded: %v", err)
		}
		return
	}
	var raw struct {
		Games     []shared.Game         `json:"games"`
		Downloads []shared.DownloadTask `json:"downloads"`
		Settings  map[string]any        `json:"settings"`
	}
	if err := json.Unmarshal(data, &raw); err != nil {
		log.Printf("State could not be loaded: %v", err)
		return
	}
	settings := DefaultSettings()
	limit := 0.0
	if value, ok := raw.Settings["downloadLimitMiB"].(float64); ok {
		// Ignore an invalid saved preference; show the default in Settings.
		if _, err := shared.DownloadLimitKiB(value); err == nil {
			limit = value
		}
	}
	delete(raw.Settings, "downloadLimitMiB")
	delete(raw.Settings, "separateGameProcess")
	patch := Patch{}
	for k, v := range raw.Settings {
		if v != nil {
			patch[k] = v
		}
	}
	if err := apply(&settings, patch); err != nil {
		settings = DefaultSettings()
	}
	settings.DownloadLimitMiB = &limit
	downloads := []shared.DownloadTask{}
	for _, task := range raw.Downloads {
		if active(task.Status) {
			task.Status, task.CanPause, task.ControlPending, task.TransferRate = "paused", true, false, ""
			task.Phase = "Paused — resume to reconnect and restore saved download checkpoints"
		}
		downloads = append(downloads, task)
	}
	games := raw.Games
	if games == nil {
		games = []shared.Game{}
	}
	s.mu.Lock()
	s.state = persisted{Games: games, Downloads: downloads, Settings: settings}
	s.mu.Unlock()
}

func (s *Store) Games() []shared.Game {
	s.mu.Lock()
	defer s.mu.Unlock()
	return shared.Clone(s.state.Games)
}

func (s *Store) Downloads() []shared.DownloadTask {
	s.mu.Lock()
	defer s.mu.Unlock()
	return shared.Clone(s.state.Downloads)
}

func (s *Store) Settings() shared.Settings {
	s.mu.Lock()
	defer s.mu.Unlock()
	return shared.Clone(s.state.Settings)
}

func (s *Store) Game(id string) (shared.Game, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, game := range s.state.Games {
		if game.ID == id {
			return shared.Clone(game), true
		}
	}
	return shared.Game{}, false
}

func (s *Store) Download(id string) (shared.DownloadTask, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, task := range s.state.Downloads {
		if task.ID == id {
			return shared.Clone(task), true
		}
	}
	return shared.DownloadTask{}, false
}

// Save writes the state atomically with private permissions. Writes are serialized.
func (s *Store) Save() error {
	// Snapshot under the write lock so a later state is never overwritten by an earlier one.
	s.writes.Lock()
	defer s.writes.Unlock()
	s.mu.Lock()
	data, err := json.MarshalIndent(s.state, "", "  ")
	s.mu.Unlock()
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(s.file), 0o755); err != nil {
		return err
	}
	temporary := s.file + ".tmp"
	if err := os.WriteFile(temporary, data, 0o600); err != nil {
		return err
	}
	return os.Rename(temporary, s.file)
}

func (s *Store) SetSettings(patch Patch) (shared.Settings, error) {
	if value, ok := patch["downloadLimitMiB"]; ok {
		if _, err := shared.DownloadLimitKiB(value); err != nil {
			return shared.Settings{}, err
		}
	}
	s.mu.Lock()
	err := apply(&s.state.Settings, patch)
	s.mu.Unlock()
	if err != nil {
		return shared.Settings{}, err
	}
	if err := s.Save(); err != nil {
		return shared.Settings{}, err
	}
	return s.Settings(), nil
}

// UpsertGame merges an inspected game into an existing entry with the same id
// or path, keeping fields the inspection does not provide (such as launch tools).
func (s *Store) UpsertGame(game shared.Game) (shared.Game, error) {
	s.mu.Lock()
	index := slices.IndexFunc(s.state.Games, func(entry shared.Game) bool { return entry.ID == game.ID || entry.Path == game.Path })
	if index >= 0 {
		if err := apply(&s.state.Games[index], ToPatch(game)); err != nil {
			s.mu.Unlock()
			return shared.Game{}, err
		}
	} else {
		s.state.Games = append([]shared.Game{game}, s.state.Games...)
	}
	s.mu.Unlock()
	return game, s.Save()
}

func (s *Store) PatchGame(id string, patch Patch) (shared.Game, error) {
	s.mu.Lock()
	index := slices.IndexFunc(s.state.Games, func(entry shared.Game) bool { return entry.ID == id })
	if index < 0 {
		s.mu.Unlock()
		return shared.Game{}, errors.New("Game not found.")
	}
	err := apply(&s.state.Games[index], patch)
	game := shared.Clone(s.state.Games[index])
	s.mu.Unlock()
	if err != nil {
		return shared.Game{}, err
	}
	return game, s.Save()
}

func (s *Store) RemoveGame(id string) error {
	s.mu.Lock()
	s.state.Games = slices.DeleteFunc(s.state.Games, func(entry shared.Game) bool { return entry.ID == id })
	s.mu.Unlock()
	return s.Save()
}

// AddDownload prepends a task and keeps at most 30 finished entries.
func (s *Store) AddDownload(task shared.DownloadTask) error {
	s.mu.Lock()
	all := append([]shared.DownloadTask{task}, s.state.Downloads...)
	finished := 0
	s.state.Downloads = slices.DeleteFunc(all, func(entry shared.DownloadTask) bool {
		if active(entry.Status) {
			return false
		}
		finished++
		return finished > 30
	})
	s.mu.Unlock()
	return s.Save()
}

func (s *Store) UpdateDownload(id string, patch Patch) error {
	s.mu.Lock()
	var err error
	if index := slices.IndexFunc(s.state.Downloads, func(entry shared.DownloadTask) bool { return entry.ID == id }); index >= 0 {
		err = apply(&s.state.Downloads[index], patch)
	}
	s.mu.Unlock()
	if err != nil {
		return err
	}
	return s.Save()
}

func (s *Store) RemoveDownload(id string) error {
	s.mu.Lock()
	index := slices.IndexFunc(s.state.Downloads, func(entry shared.DownloadTask) bool { return entry.ID == id })
	if index < 0 {
		s.mu.Unlock()
		return errors.New("Queue entry not found.")
	}
	if active(s.state.Downloads[index].Status) {
		s.mu.Unlock()
		return errors.New("Stop the installation before removing it from the queue.")
	}
	s.state.Downloads = slices.Delete(s.state.Downloads, index, index+1)
	s.mu.Unlock()
	return s.Save()
}

func (s *Store) ClearFinishedDownloads() error {
	s.mu.Lock()
	s.state.Downloads = slices.DeleteFunc(s.state.Downloads, func(entry shared.DownloadTask) bool { return !active(entry.Status) })
	s.mu.Unlock()
	return s.Save()
}

// MutateDownload changes a live task in memory without saving, like the
// original's shared task objects. Call Save (or UpdateDownload) to persist.
func (s *Store) MutateDownload(id string, change func(*shared.DownloadTask)) (shared.DownloadTask, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	index := slices.IndexFunc(s.state.Downloads, func(entry shared.DownloadTask) bool { return entry.ID == id })
	if index < 0 {
		return shared.DownloadTask{}, false
	}
	change(&s.state.Downloads[index])
	return shared.Clone(s.state.Downloads[index]), true
}

// Fields returns a patch of the named JSON fields of value; absent (empty)
// fields become nil so they are cleared, matching assignments of undefined.
func Fields(value any, keys ...string) Patch {
	full, patch := ToPatch(value), Patch{}
	for _, key := range keys {
		patch[key] = full[key]
	}
	return patch
}
