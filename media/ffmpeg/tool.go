package ffmpeg

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os/exec"
	"strconv"
	"strings"
	"sync"
	"time"
)

type FFmpegTool struct {
	FFmpegPath string        // default "ffmpeg"
	Timeout    time.Duration // 0 = no timeout (recommended for long transcodes)

	mu           sync.Mutex
	checked      bool
	resolvedPath string
	version      string
	checkErr     error
}

func NewDefaultFFmpeg() *FFmpegTool {
	return &FFmpegTool{
		FFmpegPath: "ffmpeg",
		Timeout:    0,
	}
}

func (t *FFmpegTool) ensureReady(ctx context.Context) error {
	t.mu.Lock()
	if t.checked {
		err := t.checkErr
		t.mu.Unlock()
		return err
	}
	t.mu.Unlock()

	path := t.FFmpegPath
	if path == "" {
		path = "ffmpeg"
	}

	resolved, err := exec.LookPath(path)
	if err != nil {
		t.mu.Lock()
		t.checked = true
		t.checkErr = fmt.Errorf("ffmpeg not found (FFmpegPath=%q): %w", path, err)
		t.mu.Unlock()
		return t.checkErr
	}

	// quick -version
	cctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()

	cmd := exec.CommandContext(cctx, resolved, "-version")
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr

	runErr := cmd.Run()
	if runErr != nil {
		var finalErr error
		if errors.Is(cctx.Err(), context.DeadlineExceeded) {
			finalErr = fmt.Errorf("ffmpeg check timed out (path=%q)", resolved)
		} else {
			finalErr = fmt.Errorf("ffmpeg exists but cannot run (path=%q): %w; stderr=%s",
				resolved, runErr, strings.TrimSpace(stderr.String()))
		}
		t.mu.Lock()
		t.checked = true
		t.resolvedPath = resolved
		t.checkErr = finalErr
		t.mu.Unlock()
		return finalErr
	}

	ver := parseFFmpegVersion(stdout.String())
	if ver == "" {
		ver = "unknown"
	}

	t.mu.Lock()
	t.checked = true
	t.resolvedPath = resolved
	t.version = ver
	t.checkErr = nil
	t.mu.Unlock()
	return nil
}

func (t *FFmpegTool) Version(ctx context.Context) (string, error) {
	if err := t.ensureReady(ctx); err != nil {
		return "", err
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.version, nil
}

func parseFFmpegVersion(s string) string {
	lines := strings.Split(s, "\n")
	if len(lines) == 0 {
		return ""
	}
	first := strings.TrimSpace(lines[0])
	parts := strings.Fields(first)
	for i := 0; i < len(parts)-1; i++ {
		if strings.ToLower(parts[i]) == "version" {
			return parts[i+1]
		}
	}
	return ""
}

func minDuration(a, b time.Duration) time.Duration {
	if a <= b {
		return a
	}
	return b
}

func itoa(i int) string { return strconv.Itoa(i) }
