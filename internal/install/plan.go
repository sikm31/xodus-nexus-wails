package install

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os/exec"
	"regexp"
	"slices"
	"strings"
	"sync"
	"syscall"
	"time"

	"xodus-nexus-wails/internal/diagnostics"
	"xodus-nexus-wails/internal/launcherenv"
	"xodus-nexus-wails/internal/shared"
)

var (
	storeID       = regexp.MustCompile(`^[A-Z0-9]{12}$`)
	familyPattern = regexp.MustCompile(`^[A-Za-z0-9.-]+_[A-Za-z0-9]+$`)
	controlChars  = regexp.MustCompile(`[\x00-\x1f\x7f]`)
	marketCode    = regexp.MustCompile(`^[A-Z]{2}$`)
)

func label(value string) bool {
	return shared.UTF16Len(value) <= 500 && !controlChars.MatchString(value)
}
func family(value string) bool { return familyPattern.MatchString(value) && len(value) <= 200 }

func ParseInstallPlan(output, productID, market string) (shared.InstallPlan, error) {
	if len(output) > 256_000 {
		return shared.InstallPlan{}, errors.New("Store package response is too large.")
	}
	var raw struct {
		Schema        json.RawMessage              `json:"schema"`
		RootProductID string                       `json:"rootProductId"`
		Market        string                       `json:"market"`
		Status        string                       `json:"status"`
		Choices       []map[string]json.RawMessage `json:"choices"`
	}
	invalid := errors.New("Invalid Store package response.")
	if err := json.Unmarshal([]byte(output), &raw); err != nil {
		return shared.InstallPlan{}, err
	}
	if strings.TrimSpace(string(raw.Schema)) != "1" || raw.RootProductID != productID || raw.Market != market ||
		!slices.Contains([]string{"ready", "choice", "unavailable"}, raw.Status) || raw.Choices == nil || len(raw.Choices) > 128 {
		return shared.InstallPlan{}, invalid
	}
	plan := shared.InstallPlan{Schema: 1, RootProductID: productID, Market: market, Status: raw.Status, Choices: []shared.InstallChoice{}}
	seen := map[string]bool{}
	invalidChoice := errors.New("Invalid Store package choice.")
	for _, item := range raw.Choices {
		if item == nil {
			return shared.InstallPlan{}, invalidChoice
		}
		str := func(key string) (string, bool) {
			var value string
			raw := bytes.TrimSpace(item[key])
			return value, len(raw) > 0 && raw[0] == '"' && json.Unmarshal(raw, &value) == nil
		}
		optional := func(key string, valid func(string) bool) (string, bool) {
			raw := bytes.TrimSpace(item[key])
			if len(raw) == 0 || string(raw) == "null" {
				return "", true
			}
			value, ok := str(key)
			return value, ok && valid(value)
		}
		var choice shared.InstallChoice
		var oks [9]bool
		choice.ProductID, oks[0] = str("productId")
		choice.ContentID, oks[1] = str("contentId")
		choice.Title, oks[2] = str("title")
		choice.Platform, oks[3] = str("platform")
		choice.PackageFormat, oks[4] = str("packageFormat")
		choice.Kind, oks[5] = str("kind")
		switch strings.TrimSpace(string(item["supported"])) {
		case "true":
			choice.Supported, oks[6] = true, true
		case "false":
			oks[6] = true
		}
		choice.ParentProductID, oks[7] = optional("parentProductId", storeID.MatchString)
		choice.MainPackageFamilyName, oks[8] = optional("mainPackageFamilyName", family)
		if slices.Contains(oks[:], false) || !storeID.MatchString(choice.ProductID) || !uuidPattern.MatchString(choice.ContentID) ||
			!label(choice.Title) || choice.Title == "" || !label(choice.Platform) || !label(choice.PackageFormat) || !label(choice.Kind) ||
			(choice.Supported && (choice.Platform != "Windows.Desktop" || choice.PackageFormat != "MSIXVC" || !slices.Contains([]string{"Game", "GameAddOn", "Durable"}, choice.Kind))) ||
			(choice.Supported && choice.Kind == "Durable" && (!storeID.MatchString(choice.ParentProductID) || choice.ParentProductID == choice.ProductID || !family(choice.MainPackageFamilyName))) {
			return shared.InstallPlan{}, invalidChoice
		}
		if seen[choice.ContentID] {
			return shared.InstallPlan{}, errors.New("Duplicate Store package choice.")
		}
		seen[choice.ContentID] = true
		plan.Choices = append(plan.Choices, choice)
	}
	var supported []shared.InstallChoice
	for _, choice := range plan.Choices {
		if choice.Supported {
			supported = append(supported, choice)
		}
	}
	status := "unavailable"
	if len(supported) == 1 && supported[0].Kind == "Game" {
		status = "ready"
	} else if len(supported) > 0 {
		status = "choice"
	}
	if status != plan.Status {
		return shared.InstallPlan{}, errors.New("Inconsistent Store package response.")
	}
	return plan, nil
}

type limitedBuffer struct {
	bytes.Buffer
	total *int
	over  func()
}

func (b *limitedBuffer) Write(p []byte) (int, error) {
	*b.total += len(p)
	if *b.total > 256_000 {
		b.over()
		return len(p), nil
	}
	return b.Buffer.Write(p)
}

// ReadInstallPlan runs a public metadata subprocess, no account/sign-in
// commands and no stdin dialogue.
func ReadInstallPlan(ctx context.Context, binary, productID, market string, includeDLC bool) (shared.InstallPlan, error) {
	if !storeID.MatchString(productID) || !marketCode.MatchString(market) {
		return shared.InstallPlan{}, errors.New("Enter a valid 12-character Store Product ID.")
	}
	args := []string{"resolve-package", productID, "--json", "--market", market}
	if includeDLC {
		args = append(args, "--include-dlc")
	}
	cmd := exec.Command(binary, args...)
	cmd.Env = launcherenv.External(nil).List()
	var (
		mu      sync.Mutex
		total   int
		failure error
	)
	kill := func(reason error) {
		mu.Lock()
		if failure == nil {
			failure = reason
		}
		mu.Unlock()
		if cmd.Process != nil {
			cmd.Process.Signal(syscall.SIGKILL)
		}
	}
	observation := diagnostics.ObserveChild("resolve-package")
	stdout := &limitedBuffer{total: &total, over: func() { kill(errors.New("Store package response is too large.")) }}
	stderr := &limitedBuffer{total: &total, over: func() { kill(errors.New("Store package response is too large.")) }}
	cmd.Stdout = stdout
	cmd.Stderr = writerFunc(func(p []byte) (int, error) {
		observation.Stderr(p)
		return stderr.Write(p)
	})
	if err := cmd.Start(); err != nil {
		observation.SpawnError(err)
		return shared.InstallPlan{}, err
	}
	timer := time.AfterFunc(40*time.Second, func() { kill(errors.New("Store package lookup timed out. Try again later.")) })
	stop := context.AfterFunc(ctx, func() { kill(ctx.Err()) })
	err := cmd.Wait()
	timer.Stop()
	stop()
	code, signal := -1, ""
	if cmd.ProcessState != nil {
		code = cmd.ProcessState.ExitCode()
		if status, ok := cmd.ProcessState.Sys().(syscall.WaitStatus); ok && status.Signaled() && status.Signal() == syscall.SIGKILL {
			signal = "SIGKILL"
		}
	}
	observation.Exit(code, signal)
	mu.Lock()
	defer mu.Unlock()
	if failure != nil {
		return shared.InstallPlan{}, failure
	}
	if err != nil || code != 0 {
		detail := controlChars.ReplaceAllString(stderr.String(), " ")
		if len(detail) > 300 {
			detail = strings.ToValidUTF8(detail[len(detail)-300:], "")
		}
		return shared.InstallPlan{}, errors.New("Store packages could not be resolved. " + detail)
	}
	return ParseInstallPlan(strings.ToValidUTF8(stdout.String(), "�"), productID, market)
}

type writerFunc func([]byte) (int, error)

func (f writerFunc) Write(p []byte) (int, error) { return f(p) }

func InstallSelection(plan shared.InstallPlan, selected *string) (string, error) {
	if selected == nil && plan.Status == "ready" {
		for _, choice := range plan.Choices {
			if choice.Supported {
				return choice.ContentID, nil
			}
		}
	}
	count := 0
	if selected != nil {
		for _, choice := range plan.Choices {
			if choice.Supported && choice.ContentID == *selected {
				count++
			}
		}
	}
	if selected == nil || !uuidPattern.MatchString(*selected) || count != 1 {
		return "", errors.New("Choose a supported package from this Store offer before installing.")
	}
	return *selected, nil
}

type PlanReader func(ctx context.Context, binary, productID, market string, includeDLC bool) (shared.InstallPlan, error)

type planEntry struct {
	until time.Time
	done  chan struct{}
	plan  shared.InstallPlan
	err   error
}

// Plans reuses the main-process public preflight, never renderer-supplied package metadata.
type Plans struct {
	reader  PlanReader
	now     func() time.Time
	mu      sync.Mutex
	entries map[string]*planEntry
	order   []string
}

func NewPlans(reader PlanReader, now func() time.Time) *Plans {
	if reader == nil {
		reader = ReadInstallPlan
	}
	if now == nil {
		now = time.Now
	}
	return &Plans{reader: reader, now: now, entries: map[string]*planEntry{}}
}

func (p *Plans) Get(ctx context.Context, binary, productID, market string, includeDLC bool) (shared.InstallPlan, error) {
	if !storeID.MatchString(productID) || !marketCode.MatchString(market) {
		return shared.InstallPlan{}, errors.New("Invalid Store package scope.")
	}
	key, _ := json.Marshal([]any{binary, productID, market, includeDLC})
	p.mu.Lock()
	entry := p.entries[string(key)]
	if entry == nil || !entry.until.After(p.now()) {
		entry = &planEntry{until: p.now().Add(time.Minute), done: make(chan struct{})}
		p.order = slices.DeleteFunc(p.order, func(k string) bool { return k == string(key) })
		p.entries[string(key)] = entry
		p.order = append(p.order, string(key))
		for len(p.order) > 64 {
			delete(p.entries, p.order[0])
			p.order = p.order[1:]
		}
		current := entry
		go func() {
			plan, err := p.reader(context.WithoutCancel(ctx), binary, productID, market, includeDLC)
			p.mu.Lock()
			current.plan, current.err = plan, err
			if err == nil {
				current.until = p.now().Add(time.Minute)
			} else if p.entries[string(key)] == current {
				delete(p.entries, string(key))
				p.order = slices.DeleteFunc(p.order, func(k string) bool { return k == string(key) })
			}
			p.mu.Unlock()
			close(current.done)
		}()
	}
	p.mu.Unlock()
	select {
	case <-entry.done:
	case <-ctx.Done():
		return shared.InstallPlan{}, ctx.Err()
	}
	if entry.err != nil {
		return shared.InstallPlan{}, entry.err
	}
	return shared.Clone(entry.plan), nil
}

// ContentParentForChoice returns the installed game a Durable belongs to. A
// Durable is content for one installed game, not a new runnable base game.
// It returns "" for other kinds.
func ContentParentForChoice(choice shared.InstallChoice, games []shared.Game) (string, error) {
	if choice.Kind != "Durable" {
		return "", nil
	}
	if !choice.Supported || !storeID.MatchString(choice.ParentProductID) || !family(choice.MainPackageFamilyName) {
		return "", errors.New("The add-on has no verified parent package.")
	}
	var parents []shared.Game
	for _, game := range games {
		if strings.ToUpper(game.StoreID) == choice.ParentProductID && game.InstallState == "ready" &&
			(game.PackageName == "" || strings.HasPrefix(choice.MainPackageFamilyName, game.PackageName+"_")) {
			parents = append(parents, game)
		}
	}
	if len(parents) != 1 {
		return "", errors.New("Install the matching base game first, or use Manage content on the intended installation.")
	}
	return parents[0].ID, nil
}
