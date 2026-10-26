// Package account ports account-profile.ts, account-refresh.ts,
// game-achievements.ts and game-activity.ts.
package account

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"syscall"
	"time"

	"xodus-nexus-wails/internal/proc"
	"xodus-nexus-wails/internal/shared"
)

type Profile struct {
	Gamertag      string                       `json:"gamertag"`
	AvatarDataURL string                       `json:"avatarDataUrl,omitempty"`
	Subscription  *shared.GamePassSubscription `json:"subscription,omitempty"`
}

// ProfileReader runs `profile --json`.
type ProfileReader func(ctx context.Context, binary string) (string, error)

var (
	controlPattern = regexp.MustCompile(`[\x00-\x1f\x7f]`)
	imagePattern   = regexp.MustCompile(`^data:image/(png|jpeg|webp);base64,[A-Za-z0-9+/=]+$`)
)

func validName(raw json.RawMessage) (string, bool) {
	var value string
	trimmed := strings.TrimSpace(string(raw))
	if !strings.HasPrefix(trimmed, `"`) || json.Unmarshal(raw, &value) != nil {
		return "", false
	}
	return value, strings.TrimSpace(value) != "" && shared.UTF16Len(value) <= 128 && !controlPattern.MatchString(value)
}

func validImage(raw json.RawMessage) (string, bool) {
	var value string
	if json.Unmarshal(raw, &value) != nil {
		return "", false
	}
	return value, len(value) < 3_000_000 && imagePattern.MatchString(value)
}

// runBounded runs a read-only CLI command with a deadline and cancellation.
func runBounded(ctx context.Context, binary string, args []string, maxOutput int, timeout time.Duration, closeStdin bool, failure string) (string, error) {
	if err := ctx.Err(); err != nil {
		return "", err
	}
	tracked, err := proc.Spawn(binary, args, proc.RunOptions{MaxOutput: maxOutput, StdoutOnly: true})
	if err != nil {
		return "", err
	}
	if closeStdin {
		tracked.Stdin.Close()
	}
	timer := time.AfterFunc(timeout, func() { tracked.Kill() })
	defer timer.Stop()
	select {
	case <-tracked.Done():
	case <-ctx.Done():
		tracked.Kill()
		<-tracked.Done()
		return "", ctx.Err()
	}
	result := tracked.Wait()
	if result.Code != 0 {
		return "", errors.New(failure)
	}
	return result.Output, nil
}

func ReadProfile(ctx context.Context, binary string) (string, error) {
	return runBounded(ctx, binary, []string{"profile", "--json"}, 4_000_000, 50*time.Second, false, "Xbox profile unavailable.")
}

func lastJSONLine(output string) string {
	lines := strings.Split(output, "\n")
	for i := len(lines) - 1; i >= 0; i-- {
		if strings.HasPrefix(strings.TrimLeft(lines[i], " \t\r"), "{") {
			return lines[i]
		}
	}
	return ""
}

// Profiles keeps the Xbox profile of the stored account. Only the currently
// stored account can publish/cache a profile result.
type Profiles struct {
	file      string
	changed   func()
	normalize func(string) string
	reader    ProfileReader

	mu          sync.Mutex
	value       *Profile
	account     string
	binary      string
	generation  int
	cancel      context.CancelFunc
	refreshedAt time.Time
	job         chan struct{}
	disk        sync.Mutex
}

func NewProfiles(userData string, changed func(), normalize func(string) string, reader ProfileReader) *Profiles {
	if reader == nil {
		reader = ReadProfile
	}
	return &Profiles{file: filepath.Join(userData, "xbox-profile.json"), changed: changed, normalize: normalize, reader: reader}
}

func (p *Profiles) Value() *Profile {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.value == nil {
		return nil
	}
	value := shared.Clone(*p.value)
	return &value
}

func (p *Profiles) cancelLocked() {
	p.generation++
	if p.cancel != nil {
		p.cancel()
	}
	p.cancel = nil
}

func (p *Profiles) Cancel() {
	p.mu.Lock()
	p.cancelLocked()
	p.mu.Unlock()
}

// Settled waits for the current refresh and cache write.
func (p *Profiles) Settled() {
	p.mu.Lock()
	job := p.job
	p.mu.Unlock()
	if job != nil {
		<-job
	}
	p.disk.Lock()
	p.disk.Unlock()
}

func hash(account string) string {
	sum := sha256.Sum256([]byte(account))
	return hex.EncodeToString(sum[:])
}

func (p *Profiles) removeCache() {
	p.disk.Lock()
	os.Remove(p.file)
	p.disk.Unlock()
}

func (p *Profiles) parseSubscription(raw json.RawMessage) *shared.GamePassSubscription {
	return shared.ParseSubscription(raw, time.Now())
}

func (p *Profiles) readCache(account string) (*Profile, error) {
	p.disk.Lock()
	defer p.disk.Unlock()
	info, err := os.Lstat(p.file)
	if err != nil {
		return nil, err
	}
	stat, _ := info.Sys().(*syscall.Stat_t)
	if !info.Mode().IsRegular() || stat == nil || int(stat.Uid) != os.Getuid() || info.Size() > 1_000_000 || info.Mode().Perm()&0o077 != 0 {
		return nil, errors.New("Unsafe cache")
	}
	data, err := os.ReadFile(p.file)
	if err != nil {
		return nil, err
	}
	var cached map[string]json.RawMessage
	if err := json.Unmarshal(data, &cached); err != nil {
		return nil, err
	}
	var accountHash string
	json.Unmarshal(cached["accountHash"], &accountHash)
	gamertag, ok := validName(cached["gamertag"])
	if strings.TrimSpace(string(cached["schema"])) != "1" || accountHash != hash(account) || !ok {
		return nil, errors.New("Different account")
	}
	profile := &Profile{Gamertag: gamertag, Subscription: p.parseSubscription(cached["subscription"])}
	if avatar, ok := validImage(cached["avatarDataUrl"]); ok {
		profile.AvatarDataURL = p.normalize(avatar)
	}
	return profile, nil
}

// Select binds the profile to account. Repeated refreshes share an existing
// request: aborting it can interrupt device renewal and disguises a UI
// cancellation as a backend crash.
func (p *Profiles) Select(account, binary string, force bool) {
	p.mu.Lock()
	switched := account != p.account
	backendChanged := binary != p.binary
	if switched || backendChanged || account == "" {
		p.cancelLocked()
	}
	p.account, p.binary = account, binary
	if backendChanged {
		p.refreshedAt = time.Time{}
	}
	generation := p.generation
	if account == "" {
		p.value, p.refreshedAt = nil, time.Time{}
		p.mu.Unlock()
		p.changed()
		p.removeCache()
		return
	}
	if switched {
		p.value, p.refreshedAt = nil, time.Time{}
		p.mu.Unlock()
		p.changed()
		cached, err := p.readCache(account)
		p.mu.Lock()
		if generation != p.generation {
			p.mu.Unlock()
			return
		}
		if err == nil {
			p.value = cached
			p.mu.Unlock()
			p.changed()
			p.mu.Lock()
		} else {
			p.mu.Unlock()
			p.removeCache()
			p.mu.Lock()
		}
	}
	if generation != p.generation || binary == "" || p.cancel != nil || (!force && time.Since(p.refreshedAt) < 10*time.Minute) {
		p.mu.Unlock()
		return
	}
	ctx, cancel := context.WithCancel(context.Background())
	p.cancel = cancel
	job := make(chan struct{})
	p.job = job
	p.mu.Unlock()
	go p.refresh(ctx, account, binary, generation, job)
}

func (p *Profiles) refresh(ctx context.Context, account, binary string, generation int, job chan struct{}) {
	defer func() {
		p.mu.Lock()
		if generation == p.generation {
			p.cancel = nil
			p.refreshedAt = time.Now()
		}
		p.mu.Unlock()
		close(job)
	}()
	// Failures keep the same-account cached profile, or email/default avatar.
	output, err := p.reader(ctx, binary)
	if err != nil || len(output) > 4_000_000 {
		return
	}
	var result map[string]json.RawMessage
	if json.Unmarshal([]byte(lastJSONLine(output)), &result) != nil {
		return
	}
	p.mu.Lock()
	if generation != p.generation || ctx.Err() != nil {
		p.mu.Unlock()
		return
	}
	p.mu.Unlock()
	var resultAccount string
	json.Unmarshal(result["account"], &resultAccount)
	gamertag, ok := validName(result["gamertag"])
	if strings.TrimSpace(string(result["schema"])) != "1" || resultAccount != account || !ok {
		return
	}
	value := &Profile{Gamertag: strings.TrimSpace(gamertag), Subscription: p.parseSubscription(result["subscription"])}
	if avatar, ok := validImage(result["gamerpicDataUrl"]); ok {
		value.AvatarDataURL = p.normalize(avatar)
	}
	p.mu.Lock()
	p.value = value
	p.mu.Unlock()
	p.changed()
	p.writeCache(account, generation, *value)
}

func (p *Profiles) writeCache(account string, generation int, value Profile) {
	p.disk.Lock()
	defer p.disk.Unlock()
	current := func() bool { p.mu.Lock(); defer p.mu.Unlock(); return generation == p.generation }
	if !current() {
		return
	}
	if err := os.MkdirAll(filepath.Dir(p.file), 0o700); err != nil {
		return
	}
	record := map[string]any{"schema": 1, "accountHash": hash(account), "gamertag": value.Gamertag}
	if value.AvatarDataURL != "" {
		record["avatarDataUrl"] = value.AvatarDataURL
	}
	if value.Subscription != nil {
		record["subscription"] = value.Subscription
	}
	data, _ := json.Marshal(record)
	temporary := p.file + "." + randomID()
	defer os.Remove(temporary)
	handle, err := os.OpenFile(temporary, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return
	}
	_, err = handle.Write(data)
	handle.Close()
	if err == nil && current() {
		os.Rename(temporary, p.file)
	}
}

func randomID() string {
	var b [16]byte
	if f, err := os.Open("/dev/urandom"); err == nil {
		f.Read(b[:])
		f.Close()
	}
	return hex.EncodeToString(b[:])
}
