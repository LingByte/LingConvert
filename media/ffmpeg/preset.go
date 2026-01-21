package ffmpeg

import "fmt"

// 统一输出 MP4(H.264 + AAC)，并 faststart
func PresetTranscodeMP4H264AAC(input, output string, crf int, preset string) *FFmpegCommand {
	if crf <= 0 {
		crf = 23
	}
	if preset == "" {
		preset = "medium"
	}
	return NewFFmpegCommand().
		HideBanner().
		LogLevel("error").
		Input(input).
		VideoCodec("libx264").
		AudioCodec("aac").
		CRF(crf).
		Preset(preset).
		MovFlagsFastStart().
		Output(output)
}

// 仅 remux（不转码），适合容器换壳
func PresetRemux(input, output string) *FFmpegCommand {
	return NewFFmpegCommand().
		HideBanner().
		LogLevel("error").
		Input(input).
		CopyVideo().
		CopyAudio().
		Output(output)
}

// 抽取音频为 AAC
func PresetExtractAAC(input, output string, bitrate string) *FFmpegCommand {
	if bitrate == "" {
		bitrate = "128k"
	}
	return NewFFmpegCommand().
		HideBanner().
		LogLevel("error").
		Input(input).
		AppendArgs("-vn").
		AudioCodec("aac").
		AppendArgs("-b:a", bitrate).
		Output(output)
}

// 截图：atSeconds 秒处取 1 帧
func PresetSnapshot(input, output string, atSeconds float64) *FFmpegCommand {
	// 经典做法：-ss 放在 -i 前更快，但精度略受 GOP 影响；需要精确可把 -ss 放在 -i 后
	return NewFFmpegCommand().
		HideBanner().
		LogLevel("error").
		AppendArgs("-ss", fmt.Sprintf("%.3f", atSeconds)).
		Input(input).
		AppendArgs("-frames:v", "1").
		Output(output)
}
