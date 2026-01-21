package ffmpeg

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os/exec"
	"strings"
	"sync"
)

func (t *FFmpegTool) Run(ctx context.Context, cmd *FFmpegCommand) error {
	_, err := t.RunWithProgress(ctx, cmd, nil)
	return err
}

// RunWithProgress:
// - 若 onProgress != nil，会自动追加：-progress pipe:1 -nostats
// - 进度从 stdout 读；stderr 保留给错误信息
func (t *FFmpegTool) RunWithProgress(
	ctx context.Context,
	cmd *FFmpegCommand,
	onProgress func(p FFmpegProgress) error,
) (FFmpegProgress, error) {
	var last FFmpegProgress

	if err := t.ensureReady(ctx); err != nil {
		return last, err
	}

	timeout := t.Timeout
	var cctx context.Context
	var cancel context.CancelFunc
	if timeout > 0 {
		cctx, cancel = context.WithTimeout(ctx, timeout)
	} else {
		cctx, cancel = context.WithCancel(ctx)
	}
	defer cancel()

	t.mu.Lock()
	bin := t.resolvedPath
	t.mu.Unlock()

	args := cmd.Args()
	if onProgress != nil {
		// ffmpeg progress is key=value lines
		args = append(args, "-progress", "pipe:1", "-nostats")
	}

	execCmd := exec.CommandContext(cctx, bin, args...)

	stdout, err := execCmd.StdoutPipe()
	if err != nil {
		return last, fmt.Errorf("ffmpeg stdout pipe: %w", err)
	}
	stderr, err := execCmd.StderrPipe()
	if err != nil {
		return last, fmt.Errorf("ffmpeg stderr pipe: %w", err)
	}

	if err := execCmd.Start(); err != nil {
		return last, fmt.Errorf("ffmpeg start: %w", err)
	}

	var stderrBuf bytes.Buffer
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		_, _ = io.Copy(&stderrBuf, stderr)
	}()

	if onProgress != nil {
		// parse progress from stdout
		wg.Add(1)
		go func() {
			defer wg.Done()
			scanProgress(stdout, &last, onProgress, cancel)
		}()
	} else {
		// 不需要 progress，就把 stdout 消耗掉，避免管道堵塞
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, _ = io.Copy(io.Discard, stdout)
		}()
	}

	waitErr := execCmd.Wait()
	wg.Wait()

	if waitErr != nil {
		// context timeout / cancel
		if errors.Is(cctx.Err(), context.DeadlineExceeded) {
			return last, fmt.Errorf("ffmpeg timed out after %s; stderr=%s", timeout, trimSpace(stderrBuf.String()))
		}
		if errors.Is(cctx.Err(), context.Canceled) && onProgress != nil && last.Done {
			// 正常完成也可能 cancel？一般不会，保守不走这里
		}
		// exit error
		var ee *exec.ExitError
		if errors.As(waitErr, &ee) {
			return last, fmt.Errorf("ffmpeg failed: %w; stderr=%s", waitErr, trimSpace(stderrBuf.String()))
		}
		return last, fmt.Errorf("ffmpeg exec error: %w; stderr=%s", waitErr, trimSpace(stderrBuf.String()))
	}

	return last, nil
}

func scanProgress(r io.Reader, last *FFmpegProgress, cb func(p FFmpegProgress) error, cancel context.CancelFunc) {
	// progress 输出是一行一个 key=value
	// 使用 bufio.Scanner 足够；如果你担心超长行，可自定义 SplitFunc
	sc := bufio.NewScanner(r)
	var p FFmpegProgress
	for sc.Scan() {
		line := sc.Text()
		parseProgressLine(line, &p)
		*last = p
		if cb != nil {
			if err := cb(p); err != nil {
				// 业务想中止
				cancel()
				return
			}
		}
		if p.Done {
			return
		}
	}
}

func trimSpace(s string) string {
	s = strings.TrimSpace(s)
	// stderr 可能很长，你也可以在这里做截断策略（例如最多 64KB）
	return s
}
