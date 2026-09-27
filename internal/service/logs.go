package service

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
)

// Log rotation bounds. The supervisor runs unattended, so its log is capped and
// rotated rather than allowed to grow without limit.
const (
	MaxLogBytes = 1 << 20
	LogHistory  = 1
)

// Logger writes the supervisor's bounded, rotating log.
//
// Only paths, counts, timings and error text are recorded: there is no API that
// accepts file content, so source text and configuration values cannot reach the
// log even by mistake. Field values are sanitized to a single line, so an error
// string carrying embedded newlines cannot forge log records either.
type Logger struct {
	path string

	mutex sync.Mutex
	file  *os.File
	size  int64
	// clock is replaced in tests so records are comparable.
	clock func() time.Time
}

// LogPath returns the supervisor log file inside the Grafo state directory.
func LogPath(stateDir string) string {
	return filepath.Join(stateDir, "logs", "service.log")
}

// OpenLogger opens (creating if needed) the supervisor log at path.
func OpenLogger(path string) (*Logger, error) {
	if directory := filepath.Dir(path); directory != "" {
		if err := os.MkdirAll(directory, 0o700); err != nil {
			return nil, fmt.Errorf("create log directory %s: %w", directory, err)
		}
	}
	file, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		return nil, fmt.Errorf("open log %s: %w", path, err)
	}
	size := int64(0)
	if info, statErr := file.Stat(); statErr == nil {
		size = info.Size()
	}
	return &Logger{path: path, file: file, size: size, clock: func() time.Time { return time.Now().UTC() }}, nil
}

// Path reports the log file location.
func (l *Logger) Path() string { return l.path }

// Close closes the underlying file.
func (l *Logger) Close() error {
	if l == nil {
		return nil
	}
	l.mutex.Lock()
	defer l.mutex.Unlock()
	if l.file == nil {
		return nil
	}
	err := l.file.Close()
	l.file = nil
	return err
}

// Record appends one structured record. Fields are emitted in sorted key order
// so the log is deterministic.
func (l *Logger) Record(level, message string, fields map[string]string) {
	if l == nil {
		return
	}
	keys := make([]string, 0, len(fields))
	for key := range fields {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	line := &strings.Builder{}
	line.WriteString(l.now().Format(time.RFC3339))
	line.WriteString(" " + sanitizeField(level))
	line.WriteString(" " + sanitizeField(message))
	for _, key := range keys {
		line.WriteString(" " + sanitizeField(key) + "=" + sanitizeField(fields[key]))
	}
	line.WriteString("\n")
	l.write(line.String())
}

func (l *Logger) now() time.Time {
	if l.clock == nil {
		return time.Now().UTC()
	}
	return l.clock()
}

func (l *Logger) write(line string) {
	l.mutex.Lock()
	defer l.mutex.Unlock()
	if l.file == nil {
		return
	}
	if l.size+int64(len(line)) > MaxLogBytes {
		l.rotateLocked()
	}
	written, err := l.file.WriteString(line)
	if err == nil {
		l.size += int64(written)
	}
}

// rotateLocked renames the current log aside and starts a new one. A failed
// rotation keeps writing to the existing file rather than dropping records.
func (l *Logger) rotateLocked() {
	if err := l.file.Close(); err != nil {
		return
	}
	l.file = nil
	for index := LogHistory; index >= 1; index-- {
		older := fmt.Sprintf("%s.%d", l.path, index)
		if index == LogHistory {
			_ = os.Remove(older)
		}
		if index > 1 {
			_ = os.Rename(fmt.Sprintf("%s.%d", l.path, index-1), older)
		}
	}
	_ = os.Rename(l.path, l.path+".1")
	file, err := os.OpenFile(l.path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		return
	}
	l.file = file
	l.size = 0
}

// sanitizeField collapses a value onto one line so log records stay parseable
// and no multi-line error text can forge a record.
func sanitizeField(value string) string {
	if value == "" {
		return "-"
	}
	replaced := strings.Map(func(r rune) rune {
		if r == '\n' || r == '\r' || r == '\t' {
			return ' '
		}
		if r < 0x20 || r == 0x7f {
			return -1
		}
		return r
	}, value)
	return strings.TrimSpace(replaced)
}

// TailLog returns the last lines of the supervisor log, oldest first. A missing
// log is reported as an empty tail rather than an error, because the service may
// simply not have run yet.
func TailLog(path string, lines int) ([]string, error) {
	if lines <= 0 {
		lines = 50
	}
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("read log %s: %w", path, err)
	}
	all := strings.Split(strings.TrimRight(string(data), "\n"), "\n")
	if len(all) == 1 && all[0] == "" {
		return nil, nil
	}
	if len(all) > lines {
		all = all[len(all)-lines:]
	}
	return all, nil
}
