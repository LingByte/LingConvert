package ffmpeg

import (
	"strconv"
	"strings"
)

type FFmpegProgress struct {
	Frame     int
	FPS       float64
	Bitrate   string
	Speed     string
	OutTimeMs int64

	// ffmpeg 会输出 progress=continue / progress=end
	Done bool

	// 保留未知字段，便于排障/扩展
	Extra map[string]string
}

func (p *FFmpegProgress) applyKV(k, v string) {
	switch k {
	case "frame":
		if n, err := strconv.Atoi(v); err == nil {
			p.Frame = n
		}
	case "fps":
		if f, err := strconv.ParseFloat(v, 64); err == nil {
			p.FPS = f
		}
	case "bitrate":
		p.Bitrate = v
	case "speed":
		p.Speed = v
	case "out_time_ms":
		if n, err := strconv.ParseInt(v, 10, 64); err == nil {
			p.OutTimeMs = n
		}
	case "progress":
		p.Done = (v == "end")
	default:
		if p.Extra == nil {
			p.Extra = map[string]string{}
		}
		p.Extra[k] = v
	}
}

func parseProgressLine(line string, p *FFmpegProgress) {
	line = strings.TrimSpace(line)
	if line == "" {
		return
	}
	i := strings.IndexByte(line, '=')
	if i <= 0 {
		return
	}
	k := line[:i]
	v := line[i+1:]
	p.applyKV(k, v)
}
