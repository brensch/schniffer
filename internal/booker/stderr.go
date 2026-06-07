package booker

import (
	"bufio"
	"io"
	"log/slog"
	"regexp"
	"sync"
)

// newChromeStderr returns an io.Writer that splits Chrome's stderr stream by
// line and emits each line through slog. The DevTools listening URL and any
// FATAL/ERROR lines are surfaced at the appropriate level so a crash is
// visible in production logs without grepping raw stderr.
func newChromeStderr(logger *slog.Logger, profileDir string) io.Writer {
	pr, pw := io.Pipe()
	w := &chromeStderrWriter{w: pw}
	go drainChromeStderr(pr, logger.With("profile", profileDir))
	return w
}

type chromeStderrWriter struct {
	mu sync.Mutex
	w  *io.PipeWriter
}

func (c *chromeStderrWriter) Write(p []byte) (int, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.w.Write(p)
}

// Chrome stderr is famously noisy with DBus warnings and GPU shader errors
// that are not actionable. Suppress them at debug, surface anything that
// looks like an actual crash or fatal at warn/error.
var (
	dbusNoise   = regexp.MustCompile(`(?i)dbus|gpu_command_buffer|TensorFlow|XNNPACK|WebGL[12]? blocklisted|libva|x11_error|GpuMemoryBufferFactory`)
	chromeFatal = regexp.MustCompile(`(?i)\bFATAL\b|\bCHECK failed\b|Received signal\b|sigsegv|sigbus|out of memory|allocation failed|shared memory|Aw, ?Snap`)
	chromeError = regexp.MustCompile(`(?i)\bERROR\b|crash|aborted|killed`)
)

func drainChromeStderr(r io.Reader, logger *slog.Logger) {
	scanner := bufio.NewScanner(r)
	scanner.Buffer(make([]byte, 64*1024), 1024*1024)
	for scanner.Scan() {
		line := scanner.Text()
		if line == "" {
			continue
		}
		switch {
		case chromeFatal.MatchString(line):
			logger.Error("chrome stderr", slog.String("line", line))
		case chromeError.MatchString(line):
			logger.Warn("chrome stderr", slog.String("line", line))
		case dbusNoise.MatchString(line):
			// Drop entirely; not actionable and very chatty.
		default:
			logger.Debug("chrome stderr", slog.String("line", line))
		}
	}
	// scanner exits when the pipe is closed; that's fine.
}
