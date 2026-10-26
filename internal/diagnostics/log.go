package diagnostics

import (
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"syscall"

	"xodus-nexus-wails/internal/shared"
)

func openDiagnosticFile(file, root string) (*os.File, string, error) {
	canonicalRoot, err := filepath.EvalSymlinks(root)
	if err != nil {
		return nil, "", err
	}
	canonical, err := filepath.EvalSymlinks(file)
	if err != nil {
		return nil, "", err
	}
	if !strings.HasPrefix(canonical, canonicalRoot+string(filepath.Separator)) {
		return nil, "", errors.New("Not a launcher diagnostic log.")
	}
	handle, err := os.OpenFile(file, os.O_RDONLY|syscall.O_NOFOLLOW|syscall.O_NONBLOCK, 0)
	if err != nil {
		return nil, "", err
	}
	if stat, err := handle.Stat(); err != nil || !stat.Mode().IsRegular() {
		handle.Close()
		if err == nil {
			err = errors.New("Diagnostic log is not a regular file.")
		}
		return nil, "", err
	}
	return handle, canonical, nil
}

// DiagnosticLogFile returns the canonical path of a log inside root.
func DiagnosticLogFile(file, root string) (string, error) {
	handle, canonical, err := openDiagnosticFile(file, root)
	if err != nil {
		return "", err
	}
	handle.Close()
	return canonical, nil
}

// CopyDiagnosticFile copies the full file at the moment of the click, never the
// renderer's preview. Keep a memory ceiling and fail explicitly instead of
// silently truncating. A log still being written may append more bytes after this snapshot.
func CopyDiagnosticFile(file, root string, writeText func(string) error, limit int64) error {
	if limit <= 0 {
		limit = 128 * 1024 * 1024
	}
	handle, _, err := openDiagnosticFile(file, root)
	if err != nil {
		return err
	}
	defer handle.Close()
	stat, err := handle.Stat()
	if err != nil {
		return err
	}
	size := stat.Size()
	if size > limit {
		return errors.New("This log is too large for the clipboard. Use Show log file to attach the complete file.")
	}
	buffer := make([]byte, size)
	var offset int64
	for offset < size {
		n, err := handle.ReadAt(buffer[offset:min(size, offset+1024*1024)], offset)
		if n == 0 {
			if err != nil && !errors.Is(err, io.EOF) {
				return err
			}
			return errors.New("The log changed while copying. Try again.")
		}
		offset += int64(n)
	}
	return writeText(strings.ToValidUTF8(string(buffer), "�"))
}

// ReadDiagnosticTail reads only a bounded tail from the known private
// diagnostic directory. The renderer gets text, never a file:// page or a
// file-association launch.
func ReadDiagnosticTail(file, root string, limit int64) (shared.DiagnosticLog, error) {
	if limit <= 0 {
		limit = 512 * 1024
	}
	handle, _, err := openDiagnosticFile(file, root)
	if err != nil {
		return shared.DiagnosticLog{}, err
	}
	defer handle.Close()
	stat, err := handle.Stat()
	if err != nil {
		return shared.DiagnosticLog{}, err
	}
	length := min(stat.Size(), limit)
	buffer := make([]byte, length)
	n, err := handle.ReadAt(buffer, max(0, stat.Size()-length))
	if err != nil && !errors.Is(err, io.EOF) {
		return shared.DiagnosticLog{}, err
	}
	content := strings.ToValidUTF8(string(buffer[:n]), "�")
	if stat.Size() > limit {
		if index := strings.IndexByte(content, '\n'); index >= 0 {
			content = content[index+1:]
		}
	}
	return shared.DiagnosticLog{Name: filepath.Base(file), Content: content, Truncated: stat.Size() > limit}, nil
}
