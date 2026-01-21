package ffprobe

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os/exec"
	"strings"
	"sync"
	"time"
)

// FFProbeJSON for ffprobe -show_format -show_streams -of json output
type FFProbeJSON struct {
	Streams []Stream `json:"streams"`
	Format  Format   `json:"format"`
}

type Format struct {
	Filename       string            `json:"filename"`
	NbStreams      int               `json:"nb_streams"`
	NbPrograms     int               `json:"nb_programs"`
	FormatName     string            `json:"format_name"`
	FormatLongName string            `json:"format_long_name"`
	StartTime      string            `json:"start_time"`
	Duration       string            `json:"duration"`
	Size           string            `json:"size"`
	BitRate        string            `json:"bit_rate"`
	ProbeScore     int               `json:"probe_score"`
	Tags           map[string]string `json:"tags"`
}

type Stream struct {
	Index          int               `json:"index"`
	CodecName      string            `json:"codec_name"`
	CodecLongName  string            `json:"codec_long_name"`
	CodecType      string            `json:"codec_type"` // "video" / "audio" / "subtitle"
	Profile        string            `json:"profile"`
	CodecTagString string            `json:"codec_tag_string"`
	CodecTimeBase  string            `json:"codec_time_base"`
	Width          int               `json:"width"`
	Height         int               `json:"height"`
	PixFmt         string            `json:"pix_fmt"`
	RFrameRate     string            `json:"r_frame_rate"`   // e.g. "30000/1001"
	AvgFrameRate   string            `json:"avg_frame_rate"` // e.g. "30000/1001"
	TimeBase       string            `json:"time_base"`
	BitRate        string            `json:"bit_rate"` // may be empty
	SampleRate     string            `json:"sample_rate"`
	Channels       int               `json:"channels"`
	ChannelLayout  string            `json:"channel_layout"`
	Duration       string            `json:"duration"`
	Disposition    map[string]int    `json:"disposition"`
	Tags           map[string]string `json:"tags"`
}

// Tool 封装一个可复用的 ffprobe 工具
type Tool struct {
	FFProbePath string        // default "ffprobe" or absolute path
	Timeout     time.Duration // default 10s~30s

	mu           sync.Mutex
	checked      bool   // whether ffprobe is already checked
	resolvedPath string // absolute path resolved by LookPath
	version      string // detected version, best-effort
	checkErr     error  // cached check error
}

func NewDefaultTool() *Tool {
	return &Tool{
		FFProbePath: "ffprobe",
		Timeout:     15 * time.Second,
	}
}

// ensureReady makes sure ffprobe exists and is runnable.
// It caches results so it only runs the expensive checks once per Tool instance.
func (t *Tool) ensureReady(ctx context.Context) error {
	t.mu.Lock()
	if t.checked {
		err := t.checkErr
		t.mu.Unlock()
		return err
	}
	t.mu.Unlock()

	// Do the actual check without holding the lock
	ffprobePath := t.FFProbePath
	if ffprobePath == "" {
		ffprobePath = "ffprobe"
	}

	resolved, err := exec.LookPath(ffprobePath)
	if err != nil {
		t.mu.Lock()
		t.checked = true
		t.checkErr = fmt.Errorf("ffprobe not found (FFProbePath=%q): %w", ffprobePath, err)
		t.mu.Unlock()
		return t.checkErr
	}

	timeout := t.Timeout
	if timeout <= 0 {
		timeout = 15 * time.Second
	}

	// Quick run "-version" to verify it executes; use smaller timeout to avoid hanging
	cctx, cancel := context.WithTimeout(ctx, minDuration(timeout, 5*time.Second))
	defer cancel()

	cmd := exec.CommandContext(cctx, resolved, "-version")
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr

	runErr := cmd.Run()
	if runErr != nil {
		if errors.Is(cctx.Err(), context.DeadlineExceeded) {
			err = fmt.Errorf("ffprobe check timed out (path=%q)", resolved)
		} else {
			err = fmt.Errorf("ffprobe exists but cannot run (path=%q): %w; stderr=%s",
				resolved, runErr, strings.TrimSpace(stderr.String()))
		}
		t.mu.Lock()
		t.checked = true
		t.resolvedPath = resolved
		t.checkErr = err
		t.mu.Unlock()
		return err
	}

	ver := parseFFProbeVersion(stdout.String())
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

// Version returns detected ffprobe version. It will auto-check on first call.
func (t *Tool) Version(ctx context.Context) (string, error) {
	if err := t.ensureReady(ctx); err != nil {
		return "", err
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.version, nil
}

// Probe 执行 ffprobe 并返回解析后的结构体（会自动检测 ffprobe 一次）
func (t *Tool) Probe(ctx context.Context, input string) (*FFProbeJSON, error) {
	if err := t.ensureReady(ctx); err != nil {
		return nil, err
	}

	timeout := t.Timeout
	if timeout <= 0 {
		timeout = 15 * time.Second
	}

	cctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	args := []string{
		"-v", "error",
		"-hide_banner",
		"-show_format",
		"-show_streams",
		"-of", "json",
		input,
	}

	// use resolvedPath to avoid PATH issues
	t.mu.Lock()
	ffprobeBin := t.resolvedPath
	t.mu.Unlock()

	cmd := exec.CommandContext(cctx, ffprobeBin, args...)

	out, err := cmd.Output()
	if err != nil {
		var ee *exec.ExitError
		if errors.As(err, &ee) {
			return nil, fmt.Errorf("ffprobe failed: %w; stderr=%s", err, string(ee.Stderr))
		}
		if errors.Is(cctx.Err(), context.DeadlineExceeded) {
			return nil, fmt.Errorf("ffprobe timed out after %s", timeout)
		}
		return nil, fmt.Errorf("ffprobe exec error: %w", err)
	}

	var parsed FFProbeJSON
	if err := json.Unmarshal(out, &parsed); err != nil {
		return nil, fmt.Errorf("parse ffprobe json: %w", err)
	}
	return &parsed, nil
}

// ProbeSafe kept for compatibility: now it's identical to Probe + Version.
func (t *Tool) ProbeSafe(ctx context.Context, input string) (*FFProbeJSON, string, error) {
	info, err := t.Probe(ctx, input)
	if err != nil {
		// if probe failed, still try to return version if available
		ver, verr := t.Version(ctx)
		if verr != nil {
			return nil, "", err
		}
		return nil, ver, err
	}
	ver, err := t.Version(ctx)
	if err != nil {
		return info, "", nil
	}
	return info, ver, nil
}

// 常用：取第一个视频流 / 音频流（没有就返回 nil）
func (p *FFProbeJSON) FirstVideo() *Stream {
	for i := range p.Streams {
		if p.Streams[i].CodecType == "video" {
			return &p.Streams[i]
		}
	}
	return nil
}

func (p *FFProbeJSON) FirstAudio() *Stream {
	for i := range p.Streams {
		if p.Streams[i].CodecType == "audio" {
			return &p.Streams[i]
		}
	}
	return nil
}

// parseFFProbeVersion tries to extract version from "ffprobe version x.y.z ..."
func parseFFProbeVersion(s string) string {
	lines := strings.Split(s, "\n")
	if len(lines) == 0 {
		return ""
	}
	origFirst := strings.TrimSpace(lines[0])
	first := strings.ToLower(origFirst)
	if !strings.Contains(first, "ffprobe version") {
		return ""
	}
	origParts := strings.Fields(origFirst)
	for i := 0; i < len(origParts)-1; i++ {
		if strings.ToLower(origParts[i]) == "version" {
			return origParts[i+1]
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
