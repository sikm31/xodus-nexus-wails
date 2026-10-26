package diagnostics

import (
	"encoding/json"
	"errors"
	"io/fs"
	"math"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"xodus-nexus/internal/shared"
)

var operations = setOf("account", "profile", "achievements", "library", "login", "logout", "streaming", "download", "license", "run", "resolve-package", "service", "probe")
var credentialStages = setOf("keyring-unavailable", "keyring-invalid-data", "keyring-missing-data", "keyring-write-verification", "credential-io", "device-authentication")
var stages = func() map[string]struct{} {
	all := setOf(
		"keyring-unavailable", "keyring-invalid-data", "keyring-missing-data", "keyring-write-verification", "credential-io", "device-authentication",
		"stored-account", "account-changed", "stored-device", "device-token", "stored-user", "user-token", "http-client",
		"store-ticket", "store-ticket-refused", "store-ticket-type", "store-ticket-format", "market", "account-check",
		"entitlement-shape", "entitlement-count", "entitlement-product", "entitlement-status", "entitlement-trial", "entitlement-sharing",
		"entitlement-expiry", "entitlement-parent", "entitlements-request", "entitlements-http", "entitlements-cursor", "entitlements-pagination",
		"entitlements-size", "entitlements-read", "entitlements-json", "entitlements-inconsistent", "complete", "partial",
		"access-unresolved", "access-conditional", "access-special", "access-parent-unavailable", "access-cycle", "access-depth",
		"timeout", "invalid-response", "loader-library", "panic", "ready", "protocol-error", "cancelled", "window-error", "spawn-not-found", "spawn-permission",
	)
	for _, set := range []map[string]struct{}{SessionStages, shared.ContentLicenseStages, shared.TransferStages} {
		for stage := range set {
			all[stage] = struct{}{}
		}
	}
	return all
}()

var events = setOf("start", "exit", "spawn-error", "diagnostic", "validation")
var signals = setOf("SIGINT", "SIGTERM", "SIGKILL", "SIGSEGV", "SIGABRT", "SIGPIPE")
var versionPattern = regexp.MustCompile(`^\d{1,4}\.\d{1,4}\.\d{1,4}$`)
var serviceCodePattern = regexp.MustCompile(`^[A-F0-9]{8}$`)

type Context struct{ Launcher, Backend, Source string }

// Record is allowlisted metadata. Zero/nil fields are omitted.
type Record struct {
	Operation   string
	Event       string // start | exit | spawn-error | diagnostic | validation
	Stage       string
	Status      int
	Skipped     *int
	Code        *int
	ServiceCode string
	Signal      string
	ElapsedMs   *float64
}

type entry struct {
	At          string `json:"at"`
	Operation   string `json:"operation"`
	Event       string `json:"event"`
	Launcher    string `json:"launcher"`
	Backend     string `json:"backend"`
	Source      string `json:"source"`
	Stage       string `json:"stage,omitempty"`
	Status      int    `json:"status,omitempty"`
	Skipped     *int   `json:"skipped,omitempty"`
	Code        *int   `json:"code,omitempty"`
	ServiceCode string `json:"serviceCode,omitempty"`
	Signal      string `json:"signal,omitempty"`
	ElapsedMs   *int64 `json:"elapsedMs,omitempty"`
}

func version(value string) string {
	if versionPattern.MatchString(value) {
		return value
	}
	return "unknown"
}

// Backend writes allowlisted metadata only. It never persists child output,
// argv, auth frames or identifiers.
type Backend struct {
	root, file string
	context    func() Context
	limit      int64
	queue      chan string
	writes     sync.WaitGroup
	pending    atomic.Int32
	failed     atomic.Bool
}

func NewBackend(userData string, context func() Context, limit int64) *Backend {
	if context == nil {
		context = func() Context { return Context{} }
	}
	if limit <= 0 {
		limit = 256 * 1024
	}
	root := filepath.Join(userData, "backend-diagnostics")
	b := &Backend{root: root, file: filepath.Join(root, "backend.log"), context: context, limit: limit, queue: make(chan string, 256)}
	go func() {
		for line := range b.queue {
			if err := b.append(line); err != nil {
				b.failed.Store(true)
			}
			b.pending.Add(-1)
			b.writes.Done()
		}
	}()
	return b
}

func ownedPrivate(info fs.FileInfo) bool {
	stat, ok := info.Sys().(*syscall.Stat_t)
	return ok && int(stat.Uid) == os.Getuid() && info.Mode().Perm()&0o077 == 0
}

func singleLink(info fs.FileInfo) bool {
	stat, ok := info.Sys().(*syscall.Stat_t)
	return ok && stat.Nlink == 1
}

var errUnsafe = errors.New("Unsafe log")

func (b *Backend) safeFile(file string) error {
	info, err := os.Lstat(file)
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	if !info.Mode().IsRegular() || !singleLink(info) || !ownedPrivate(info) {
		return errUnsafe
	}
	return nil
}

func (b *Backend) safeRoot() error {
	info, err := os.Lstat(b.root)
	if err != nil {
		return err
	}
	if !info.IsDir() || info.Mode()&fs.ModeSymlink != 0 || !ownedPrivate(info) {
		return errors.New("Unsafe log directory")
	}
	return nil
}

func (b *Backend) append(line string) error {
	if err := os.MkdirAll(b.root, 0o700); err != nil {
		return err
	}
	if err := b.safeRoot(); err != nil {
		return err
	}
	if err := b.safeFile(b.file); err != nil {
		return err
	}
	var size int64
	if info, err := os.Stat(b.file); err == nil {
		size = info.Size()
	} else if !errors.Is(err, fs.ErrNotExist) {
		return err
	}
	if size > 0 && size+int64(len(line)) > b.limit {
		if err := b.safeFile(b.file + ".1"); err != nil {
			return err
		}
		if err := os.Rename(b.file, b.file+".1"); err != nil {
			return err
		}
	}
	handle, err := os.OpenFile(b.file, os.O_WRONLY|os.O_CREATE|os.O_APPEND|syscall.O_NOFOLLOW, 0o600)
	if err != nil {
		return err
	}
	defer handle.Close()
	info, err := handle.Stat()
	if err != nil {
		return err
	}
	if !info.Mode().IsRegular() || !singleLink(info) || !ownedPrivate(info) {
		return errUnsafe
	}
	_, err = handle.WriteString(line)
	return err
}

func (b *Backend) Record(data Record) {
	if !has(operations, data.Operation) || !has(events, data.Event) {
		return
	}
	context := func() (c Context) {
		defer func() {
			if recover() != nil {
				c = Context{}
			}
		}()
		return b.context()
	}()
	e := entry{
		At: time.Now().UTC().Format("2006-01-02T15:04:05.000Z"), Operation: data.Operation, Event: data.Event,
		Launcher: version(context.Launcher), Backend: version(context.Backend), Source: "unknown",
	}
	if context.Source == "managed" || context.Source == "external" {
		e.Source = context.Source
	}
	if has(stages, data.Stage) {
		e.Stage = data.Stage
	}
	if data.Status >= 100 && data.Status <= 599 {
		e.Status = data.Status
	}
	if data.Skipped != nil && *data.Skipped >= 0 && *data.Skipped <= 200_000 {
		e.Skipped = data.Skipped
	}
	if data.Code != nil && *data.Code >= -1 && *data.Code <= 255 {
		e.Code = data.Code
	}
	if serviceCodePattern.MatchString(data.ServiceCode) && (data.Operation == "library" || data.Operation == "profile") &&
		(data.Stage == "ticket-refused" || data.Stage == "ticket-interaction-required") {
		e.ServiceCode = data.ServiceCode
	}
	if has(signals, data.Signal) {
		e.Signal = data.Signal
	}
	if data.ElapsedMs != nil && !math.IsNaN(*data.ElapsedMs) && !math.IsInf(*data.ElapsedMs, 0) {
		elapsed := int64(math.Max(0, math.Min(86_400_000, math.Floor(*data.ElapsedMs+0.5))))
		e.ElapsedMs = &elapsed
	}
	encoded, err := json.Marshal(e)
	if err != nil {
		return
	}
	if b.pending.Add(1) > 256 {
		b.pending.Add(-1)
		return
	}
	b.writes.Add(1)
	select {
	case b.queue <- string(encoded) + "\n":
	default:
		b.pending.Add(-1)
		b.writes.Done()
	}
}

func (b *Backend) Flush() { b.writes.Wait() }

func (b *Backend) PendingWrites() int { return int(b.pending.Load()) }

func (b *Backend) Read() (shared.DiagnosticLog, error) {
	b.Flush()
	if b.failed.Load() {
		return shared.DiagnosticLog{}, errors.New("Backend diagnostics could not be saved. Check available disk space and private profile permissions.")
	}
	log, err := func() (shared.DiagnosticLog, error) {
		if err := b.safeRoot(); err != nil {
			return shared.DiagnosticLog{}, err
		}
		if err := b.safeFile(b.file); err != nil {
			return shared.DiagnosticLog{}, err
		}
		return ReadDiagnosticTail(b.file, b.root, 0)
	}()
	if errors.Is(err, fs.ErrNotExist) {
		return shared.DiagnosticLog{Name: "backend.log", Content: "No backend operations have been recorded in this profile yet.", Truncated: false}, nil
	}
	if err != nil {
		return shared.DiagnosticLog{}, errors.New("Backend diagnostics could not be opened safely.")
	}
	return log, nil
}

var (
	credentialStrict = regexp.MustCompile(`^NEXUS_CREDENTIAL_ERROR_V1 ([a-z-]+)$`)
	sessionStrict    = regexp.MustCompile(`^NEXUS_SESSION_DIAGNOSTIC_V1 ([a-z-]+)$`)
	numericLine      = regexp.MustCompile(`^NEXUS_LIBRARY_DIAGNOSTIC_V1 ([a-z-]+) (\d{1,3}) (\d{1,6})$`)
	legacyLine       = regexp.MustCompile(`^Account library unavailable \(([a-z-]+)\); credentials and installed games are unchanged\.$`)
	panicLine        = regexp.MustCompile(`^thread '[^']{1,80}' panicked at `)
	loaderLine       = regexp.MustCompile(`^[^\r\n]{1,200}: error while loading shared libraries: `)
)

// BackendReader is bounded, ASCII-only framing. Unknown or oversized lines are
// discarded, not redacted.
type BackendReader struct {
	operation string
	emit      func(Record)
	framer    LineFramer
	emitted   int
}

func NewBackendReader(operation string, emit func(Record)) *BackendReader {
	return &BackendReader{operation: operation, emit: emit, framer: LineFramer{maxLine: 512, maxTotal: 2 * 1024 * 1024}}
}

func (r *BackendReader) Push(bytes []byte) {
	if r.emitted >= 32 {
		return
	}
	r.framer.Push(bytes, func(line string) bool {
		r.consume(trimCR(line))
		return r.emitted < 32
	})
}

func (r *BackendReader) consume(line string) {
	op := r.operation
	in := func(values ...string) bool { return slices.Contains(values, op) }
	record := Record{Operation: op, Event: "diagnostic"}
	credential := credentialStrict.FindStringSubmatch(line)
	session := sessionStrict.FindStringSubmatch(line)
	content, isContent := shared.ContentLicenseDiagnostic(line)
	transfer, isTransfer := shared.TransferDiagnostic(line)
	ticket, isTicket := TicketFailureDiagnostic(line)
	numeric := numericLine.FindStringSubmatch(line)
	legacy := legacyLine.FindStringSubmatch(line)
	switch {
	case credential != nil && has(credentialStages, credential[1]):
		record.Stage = credential[1]
	case isTicket && in("library", "profile"):
		record.Stage, record.ServiceCode = ticket.Stage, ticket.ServiceCode
	case isTransfer && in("streaming", "download"):
		record.Stage, record.Status = transfer.Stage, transfer.Status
	case isContent && in("streaming", "download", "license", "run"):
		record.Stage, record.Status = content.Stage, content.Status
	case session != nil && in("library", "profile") && has(SessionStages, session[1]):
		record.Stage = session[1]
	case numeric != nil && op == "library":
		status, _ := strconv.Atoi(numeric[2])
		skipped, _ := strconv.Atoi(numeric[3])
		if (status != 0 && (status < 100 || status > 599)) || skipped > 200_000 {
			return
		}
		record.Stage, record.Status, record.Skipped = numeric[1], status, &skipped
	case legacy != nil && op == "library":
		record.Stage = legacy[1]
	case line == "Unable to read the stored account: Stored Xodus credentials are empty or invalid. Check the system keyring.":
		record.Stage = "keyring-invalid-data"
	case line == "Unable to read the stored account: The system keyring could not be accessed. Check that it is running and unlocked.":
		record.Stage = "keyring-unavailable"
	case line == "Account library timed out; installed games are unchanged.":
		record.Stage = "timeout"
	case panicLine.MatchString(line):
		record.Stage = "panic"
	case loaderLine.MatchString(line):
		record.Stage = "loader-library"
	}
	if !has(stages, record.Stage) {
		return
	}
	r.emitted++
	r.emit(record)
}

var (
	activeMu sync.RWMutex
	active   *Backend
)

func SetBackend(value *Backend) {
	activeMu.Lock()
	active = value
	activeMu.Unlock()
}

func current() *Backend {
	activeMu.RLock()
	defer activeMu.RUnlock()
	return active
}

// Emit records into the active diagnostics log, if any.
func Emit(data Record) {
	if log := current(); log != nil {
		log.Record(data)
	}
}

// Observation tracks one backend child process. Methods are no-ops on nil.
type Observation struct {
	log       *Backend
	operation string
	start     time.Time
	reader    *BackendReader
	mu        sync.Mutex
}

func elapsed(start time.Time) *float64 {
	ms := float64(time.Since(start).Nanoseconds()) / 1e6
	return &ms
}

// ObserveChild starts observing a child for operation. Returns nil when disabled.
func ObserveChild(operation string) *Observation {
	log := current()
	if log == nil || !has(operations, operation) {
		return nil
	}
	o := &Observation{log: log, operation: operation, start: time.Now()}
	log.Record(Record{Operation: operation, Event: "start"})
	o.reader = NewBackendReader(operation, log.Record)
	return o
}

// ObserveCommand observes only xodus-cli invocations.
func ObserveCommand(binary string, args []string) *Observation {
	if filepath.Base(binary) != "xodus-cli" {
		return nil
	}
	operation := ""
	if slices.Contains(args, "--help") || slices.Contains(args, "--version") {
		operation = "probe"
	} else if len(args) > 0 {
		operation = args[0]
	}
	return ObserveChild(operation)
}

// Stderr feeds child stderr. No stdout listener: login URLs, cookies, profile
// and entitlement data are private.
func (o *Observation) Stderr(bytes []byte) {
	if o == nil {
		return
	}
	o.mu.Lock()
	o.reader.Push(bytes)
	o.mu.Unlock()
}

func (o *Observation) SpawnError(err error) {
	if o == nil {
		return
	}
	record := Record{Operation: o.operation, Event: "spawn-error", ElapsedMs: elapsed(o.start)}
	if errors.Is(err, fs.ErrNotExist) {
		record.Stage = "spawn-not-found"
	} else if errors.Is(err, fs.ErrPermission) {
		record.Stage = "spawn-permission"
	}
	o.log.Record(record)
}

// Exit records the exit code (-1 when killed) and the terminating signal name, if any.
func (o *Observation) Exit(code int, signal string) {
	if o == nil {
		return
	}
	o.log.Record(Record{Operation: o.operation, Event: "exit", Code: &code, Signal: signal, ElapsedMs: elapsed(o.start)})
}
