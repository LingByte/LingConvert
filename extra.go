package media

import (
	"context"
	"encoding/json"
	"fmt"
	"os/exec"
)

func (t *Tool) runFFProbeJSON(ctx context.Context, args []string, out any) error {
	if err := t.ensureReady(ctx); err != nil {
		return err
	}

	t.mu.Lock()
	ffprobeBin := t.resolvedPath
	t.mu.Unlock()

	cmd := exec.CommandContext(ctx, ffprobeBin, args...)
	b, err := cmd.Output()
	if err != nil {
		if ee, ok := err.(*exec.ExitError); ok {
			return fmt.Errorf("ffprobe failed: %w; stderr=%s", err, string(ee.Stderr))
		}
		return fmt.Errorf("ffprobe exec error: %w", err)
	}
	if out == nil {
		return nil
	}
	if err := json.Unmarshal(b, out); err != nil {
		return fmt.Errorf("parse ffprobe json: %w", err)
	}
	return nil
}

type Packet struct {
	CodecType   string `json:"codec_type"`
	StreamIndex int    `json:"stream_index"`

	Pts     *int64 `json:"pts"`
	PtsTime string `json:"pts_time"`
	Dts     *int64 `json:"dts"`
	DtsTime string `json:"dts_time"`

	Duration     *int64 `json:"duration"`
	DurationTime string `json:"duration_time"`

	Size  string `json:"size"`
	Pos   string `json:"pos"`
	Flags string `json:"flags"`
}

type PacketsJSON struct {
	Packets []Packet `json:"packets"`
}

func (t *Tool) ProbePackets(ctx context.Context, input string, selectStreams string) (*PacketsJSON, error) {
	// selectStreams e.g. "v:0" / "a:0" / ""(all)
	args := []string{
		"-v", "error",
		"-hide_banner",
		"-show_packets",
		"-of", "json",
	}
	if selectStreams != "" {
		args = append(args, "-select_streams", selectStreams)
	}
	args = append(args, input)

	var out PacketsJSON
	if err := t.runFFProbeJSON(ctx, args, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

type Frame struct {
	MediaType string `json:"media_type"` // video/audio

	KeyFrame int    `json:"key_frame"` // 1/0
	PictType string `json:"pict_type"` // I/P/B
	PtsTime  string `json:"pts_time"`
	DtsTime  string `json:"dts_time"`

	BestEffortTimestampTime string `json:"best_effort_timestamp_time"`

	PktDurationTime string `json:"pkt_duration_time"`

	Width  int    `json:"width"`
	Height int    `json:"height"`
	PixFmt string `json:"pix_fmt"`

	SampleRate string `json:"sample_rate"`
	NbSamples  int    `json:"nb_samples"`
	Channels   int    `json:"channels"`

	Tags map[string]string `json:"tags"`
	// side_data_list 如果你想要 HDR/旋转等，可以加：
	SideDataList []map[string]any `json:"side_data_list"`
}

type FramesJSON struct {
	Frames []Frame `json:"frames"`
}

// readIntervals 例子： "0%+5" 表示从 0 秒开始取 5 秒，减少输出量
func (t *Tool) ProbeFrames(ctx context.Context, input string, selectStreams string, readIntervals string) (*FramesJSON, error) {
	args := []string{
		"-v", "error",
		"-hide_banner",
		"-show_frames",
		"-of", "json",
	}
	if selectStreams != "" {
		args = append(args, "-select_streams", selectStreams)
	}
	if readIntervals != "" {
		args = append(args, "-read_intervals", readIntervals)
	}
	args = append(args, input)

	var out FramesJSON
	if err := t.runFFProbeJSON(ctx, args, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

type Chapter struct {
	ID        int               `json:"id"`
	TimeBase  string            `json:"time_base"`
	Start     int64             `json:"start"`
	StartTime string            `json:"start_time"`
	End       int64             `json:"end"`
	EndTime   string            `json:"end_time"`
	Tags      map[string]string `json:"tags"`
}

type ChaptersJSON struct {
	Chapters []Chapter `json:"chapters"`
}

func (t *Tool) ProbeChapters(ctx context.Context, input string) (*ChaptersJSON, error) {
	args := []string{
		"-v", "error",
		"-hide_banner",
		"-show_chapters",
		"-of", "json",
		input,
	}
	var out ChaptersJSON
	if err := t.runFFProbeJSON(ctx, args, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

type Program struct {
	ProgramID  int               `json:"program_id"`
	ProgramNum int               `json:"program_num"`
	NbStreams  int               `json:"nb_streams"`
	Tags       map[string]string `json:"tags"`
}

type ProgramsJSON struct {
	Programs []Program `json:"programs"`
}

func (t *Tool) ProbePrograms(ctx context.Context, input string) (*ProgramsJSON, error) {
	args := []string{
		"-v", "error",
		"-hide_banner",
		"-show_programs",
		"-of", "json",
		input,
	}
	var out ProgramsJSON
	if err := t.runFFProbeJSON(ctx, args, &out); err != nil {
		return nil, err
	}
	return &out, nil
}
