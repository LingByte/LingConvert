package ffmpeg

import (
	"fmt"
	"strings"
)

type FFmpegCommand struct {
	args []string
}

func NewFFmpegCommand() *FFmpegCommand {
	// 默认覆盖输出，避免交互卡住
	return &FFmpegCommand{args: []string{"-y"}}
}

func (c *FFmpegCommand) Args() []string {
	out := make([]string, len(c.args))
	copy(out, c.args)
	return out
}

func (c *FFmpegCommand) AppendArgs(args ...string) *FFmpegCommand {
	c.args = append(c.args, args...)
	return c
}

func (c *FFmpegCommand) HideBanner() *FFmpegCommand {
	return c.AppendArgs("-hide_banner")
}

func (c *FFmpegCommand) LogLevel(level string) *FFmpegCommand {
	// "error" / "warning" / "info" / "quiet"
	return c.AppendArgs("-v", level)
}

func (c *FFmpegCommand) Input(path string) *FFmpegCommand {
	return c.AppendArgs("-i", path)
}

func (c *FFmpegCommand) Overwrite(on bool) *FFmpegCommand {
	// 默认已经 -y，这里允许业务明确关闭
	if on {
		return c // already -y
	}
	// remove "-y" if present; then add "-n"
	n := make([]string, 0, len(c.args))
	for _, a := range c.args {
		if a == "-y" {
			continue
		}
		n = append(n, a)
	}
	c.args = n
	return c.AppendArgs("-n")
}

func (c *FFmpegCommand) Output(path string) *FFmpegCommand {
	return c.AppendArgs(path)
}

func (c *FFmpegCommand) VideoCodec(codec string) *FFmpegCommand {
	return c.AppendArgs("-c:v", codec)
}
func (c *FFmpegCommand) AudioCodec(codec string) *FFmpegCommand {
	return c.AppendArgs("-c:a", codec)
}
func (c *FFmpegCommand) CopyVideo() *FFmpegCommand { return c.VideoCodec("copy") }
func (c *FFmpegCommand) CopyAudio() *FFmpegCommand { return c.AudioCodec("copy") }

func (c *FFmpegCommand) CRF(v int) *FFmpegCommand {
	return c.AppendArgs("-crf", itoa(v))
}
func (c *FFmpegCommand) Preset(p string) *FFmpegCommand {
	return c.AppendArgs("-preset", p)
}
func (c *FFmpegCommand) Tune(t string) *FFmpegCommand {
	return c.AppendArgs("-tune", t)
}
func (c *FFmpegCommand) MovFlagsFastStart() *FFmpegCommand {
	return c.AppendArgs("-movflags", "+faststart")
}

func (c *FFmpegCommand) Map(spec string) *FFmpegCommand {
	// e.g. "0:v:0" or "0:a?" etc.
	return c.AppendArgs("-map", spec)
}

func (c *FFmpegCommand) Scale(w, h int) *FFmpegCommand {
	// 若你需要叠加多个滤镜，可以做一个 FilterGraph builder；先给一个够用的
	return c.AppendArgs("-vf", "scale="+itoa(w)+":"+itoa(h))
}

func (c *FFmpegCommand) FPS(fps string) *FFmpegCommand {
	// fps can be "30" or "30000/1001"
	return c.AppendArgs("-r", fps)
}

func (c *FFmpegCommand) StartAt(seconds float64) *FFmpegCommand {
	// -ss before -i: 快速 seek；这里给简单语义：业务自行控制插入位置时可用 AppendArgs
	return c.AppendArgs("-ss", trimFloat(seconds))
}

func trimFloat(f float64) string {
	s := fmt.Sprintf("%.3f", f)
	s = strings.TrimRight(s, "0")
	s = strings.TrimRight(s, ".")
	if s == "" {
		return "0"
	}
	return s
}
