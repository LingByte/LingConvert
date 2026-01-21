package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"html/template"
	"io"
	"log"
	"mime/multipart"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/gin-gonic/gin"

	media "github.com/LingByte/LingConvert"
)

type PageData struct {
	OK    bool
	Error string

	View     *ProbeView
	Advanced *AdvancedView

	RawBaseJSON     string
	RawFramesJSON   string
	RawPacketsJSON  string
	RawChaptersJSON string
	RawProgramsJSON string
}

type ProbeView struct {
	Source       string
	FFVersion    string
	Container    string
	DurationSec  float64
	SizeBytes    int64
	TotalBitrate int64

	Video *VideoView
	Audio *AudioView
}

type VideoView struct {
	Codec      string
	Profile    string
	Resolution string
	FPSLabel   string
	PixFmt     string
	Bitrate    int64
}

type AudioView struct {
	Codec      string
	Profile    string
	SampleRate int
	Channels   int
	Layout     string
	Bitrate    int64
}

type AdvancedView struct {
	FramesSummary   string
	PacketsSummary  string
	ChaptersSummary string
	ProgramsSummary string
}

func main() {
	r := gin.Default()

	// 模板函数：中文展示
	r.SetFuncMap(template.FuncMap{
		"humanBitrate":  humanBitrate,
		"humanSize":     humanSize,
		"humanDuration": humanDuration,
	})
	r.LoadHTMLGlob("templates/*.html")

	tool := media.NewDefaultTool()
	tool.Timeout = 25 * time.Second

	r.GET("/", func(c *gin.Context) {
		c.HTML(http.StatusOK, "index.html", PageData{})
	})

	// 页面入口：上传 or URL + 选项
	r.POST("/probe", func(c *gin.Context) {
		ctx, cancel := context.WithTimeout(context.Background(), tool.Timeout)
		defer cancel()

		// 输入源：上传优先
		input, desc, cleanup, err := getInputFromRequest(c)
		if err != nil {
			c.HTML(http.StatusBadRequest, "index.html", PageData{OK: false, Error: err.Error()})
			return
		}
		if cleanup != nil {
			defer cleanup()
		}

		// 解析基础信息
		base, err := tool.Probe(ctx, input)
		if err != nil {
			c.HTML(http.StatusInternalServerError, "index.html", PageData{OK: false, Error: "ffprobe 基础解析失败: " + err.Error()})
			return
		}

		ver, _ := tool.Version(ctx)
		view := buildProbeView(base, desc, ver)

		rawBase, _ := json.MarshalIndent(base, "", "  ")

		// 读取高级选项
		opt := readOptions(c)

		data := PageData{
			OK:          true,
			View:        view,
			Advanced:    &AdvancedView{},
			RawBaseJSON: string(rawBase),
		}

		// 高级：Frames
		if opt.EnableFrames {
			frames, err := tool.ProbeFrames(ctx, input, opt.FramesSelectStreams, opt.FramesReadIntervals)
			if err != nil {
				data.Error = appendErr(data.Error, "frames 解析失败: "+err.Error())
			} else {
				if opt.FramesKeyOnly {
					frames = filterKeyFrames(frames)
				}
				if opt.FramesLimit > 0 {
					frames = limitFrames(frames, opt.FramesLimit)
				}
				raw, _ := json.MarshalIndent(frames, "", "  ")
				data.RawFramesJSON = string(raw)
				data.Advanced.FramesSummary = summarizeFrames(frames)
			}
		}

		// 高级：Packets
		if opt.EnablePackets {
			pkts, err := tool.ProbePackets(ctx, input, opt.PacketsSelectStreams)
			if err != nil {
				data.Error = appendErr(data.Error, "packets 解析失败: "+err.Error())
			} else {
				if opt.PacketsLimit > 0 {
					pkts = limitPackets(pkts, opt.PacketsLimit)
				}
				raw, _ := json.MarshalIndent(pkts, "", "  ")
				data.RawPacketsJSON = string(raw)
				data.Advanced.PacketsSummary = summarizePackets(pkts)
			}
		}

		// 高级：Chapters
		if opt.EnableChapters {
			chs, err := tool.ProbeChapters(ctx, input)
			if err != nil {
				data.Error = appendErr(data.Error, "chapters 解析失败: "+err.Error())
			} else {
				raw, _ := json.MarshalIndent(chs, "", "  ")
				data.RawChaptersJSON = string(raw)
				data.Advanced.ChaptersSummary = fmt.Sprintf("章节数量：%d", len(chs.Chapters))
			}
		}

		// 高级：Programs
		if opt.EnablePrograms {
			pgs, err := tool.ProbePrograms(ctx, input)
			if err != nil {
				data.Error = appendErr(data.Error, "programs 解析失败: "+err.Error())
			} else {
				raw, _ := json.MarshalIndent(pgs, "", "  ")
				data.RawProgramsJSON = string(raw)
				data.Advanced.ProgramsSummary = fmt.Sprintf("Programs 数量：%d", len(pgs.Programs))
			}
		}

		c.HTML(http.StatusOK, "index.html", data)
	})

	log.Println("Listening on http://127.0.0.1:8080")
	_ = r.Run(":8080")
}

func appendErr(existing, add string) string {
	add = strings.TrimSpace(add)
	if add == "" {
		return existing
	}
	if existing == "" {
		return add
	}
	return existing + "\n" + add
}

// --------------------- 输入处理（上传/URL）---------------------

func getInputFromRequest(c *gin.Context) (input string, desc string, cleanup func(), err error) {
	// 1) 上传文件优先
	if fh, ferr := c.FormFile("file"); ferr == nil && fh != nil && fh.Size > 0 {
		path, d, e := saveUploadedToTemp(fh)
		if e != nil {
			return "", "", nil, fmt.Errorf("保存上传文件失败: %w", e)
		}
		return path, d, func() { _ = os.Remove(path) }, nil
	}

	// 2) URL
	rawURL := strings.TrimSpace(c.PostForm("url"))
	if rawURL == "" {
		return "", "", nil, errors.New("请上传文件或输入远程 URL")
	}
	if e := validateRemoteURL(rawURL); e != nil {
		return "", "", nil, fmt.Errorf("URL 不合法: %w", e)
	}
	return rawURL, "远程地址：" + rawURL, nil, nil
}

// ✅ 不用 gin.SaveUploadedFile，避免 macOS chmod 报错
func saveUploadedToTemp(fh *multipart.FileHeader) (string, string, error) {
	src, err := fh.Open()
	if err != nil {
		return "", "", err
	}
	defer src.Close()

	ext := filepath.Ext(fh.Filename)
	if ext == "" {
		ext = ".bin"
	}
	tmp, err := os.CreateTemp("", "probe-*"+ext)
	if err != nil {
		return "", "", err
	}
	defer tmp.Close()

	if _, err := io.Copy(tmp, src); err != nil {
		return "", "", err
	}

	return tmp.Name(), "上传文件：" + fh.Filename, nil
}

func validateRemoteURL(raw string) error {
	u, err := url.Parse(raw)
	if err != nil {
		return err
	}
	if u.Scheme == "" || u.Host == "" {
		return errors.New("必须是完整 URL（例如 https://example.com/a.mp4）")
	}
	switch strings.ToLower(u.Scheme) {
	case "http", "https":
		return nil
	default:
		return errors.New("默认仅允许 http/https（安全考虑避免 SSRF），如需 rtsp/srt 可放开")
	}
}

// --------------------- 选项解析 ---------------------

type Options struct {
	EnableFrames   bool
	EnablePackets  bool
	EnableChapters bool
	EnablePrograms bool

	FramesSelectStreams string
	FramesReadIntervals string
	FramesLimit         int
	FramesKeyOnly       bool

	PacketsSelectStreams string
	PacketsLimit         int
}

func readOptions(c *gin.Context) Options {
	// checkbox: "on" or ""
	on := func(name string) bool { return c.PostForm(name) == "on" }

	opt := Options{
		EnableFrames:   on("enable_frames"),
		EnablePackets:  on("enable_packets"),
		EnableChapters: on("enable_chapters"),
		EnablePrograms: on("enable_programs"),

		FramesSelectStreams: strings.TrimSpace(c.PostForm("frames_streams")), // v:0 / a:0 / ""
		FramesReadIntervals: strings.TrimSpace(c.PostForm("frames_intervals")),
		FramesKeyOnly:       on("frames_keyonly"),

		PacketsSelectStreams: strings.TrimSpace(c.PostForm("packets_streams")),
	}

	opt.FramesLimit = parseInt(c.PostForm("frames_limit"), 300)
	opt.PacketsLimit = parseInt(c.PostForm("packets_limit"), 200)
	return opt
}

func parseInt(s string, def int) int {
	s = strings.TrimSpace(s)
	if s == "" {
		return def
	}
	n, err := strconv.Atoi(s)
	if err != nil {
		return def
	}
	return n
}

// --------------------- 基础信息 ViewModel ---------------------

func buildProbeView(p *media.FFProbeJSON, source string, ver string) *ProbeView {
	v := p.FirstVideo()
	a := p.FirstAudio()

	view := &ProbeView{
		Source:    source,
		FFVersion: ver,
		Container: chooseNonEmpty(p.Format.FormatLongName, p.Format.FormatName),
	}

	view.DurationSec = parseFloat(p.Format.Duration)
	view.SizeBytes = parseInt64(p.Format.Size)
	view.TotalBitrate = parseInt64(p.Format.BitRate)

	if v != nil {
		view.Video = &VideoView{
			Codec:      v.CodecName,
			Profile:    v.Profile,
			Resolution: fmt.Sprintf("%dx%d", v.Width, v.Height),
			FPSLabel:   fpsLabel(v.RFrameRate, v.AvgFrameRate),
			PixFmt:     v.PixFmt,
			Bitrate:    parseInt64(v.BitRate),
		}
	}
	if a != nil {
		view.Audio = &AudioView{
			Codec:      a.CodecName,
			Profile:    a.Profile,
			SampleRate: int(parseInt64(a.SampleRate)),
			Channels:   a.Channels,
			Layout:     a.ChannelLayout,
			Bitrate:    parseInt64(a.BitRate),
		}
	}

	return view
}

func chooseNonEmpty(a, b string) string {
	a = strings.TrimSpace(a)
	if a != "" {
		return a
	}
	return strings.TrimSpace(b)
}

func fpsLabel(r, avg string) string {
	rr := fracToFloat(r)
	aa := fracToFloat(avg)
	if rr > 0 && aa > 0 {
		if almostEqual(rr, aa) {
			return fmt.Sprintf("%.3g fps（恒定/接近恒定）", aa)
		}
		return fmt.Sprintf("标称 %.3g fps，平均 %.3g fps（可能为可变帧率）", rr, aa)
	}
	if aa > 0 {
		return fmt.Sprintf("平均 %.3g fps", aa)
	}
	if rr > 0 {
		return fmt.Sprintf("%.3g fps", rr)
	}
	return "-"
}

func fracToFloat(s string) float64 {
	s = strings.TrimSpace(s)
	if s == "" || s == "0/0" {
		return 0
	}
	parts := strings.Split(s, "/")
	if len(parts) == 1 {
		f, _ := strconv.ParseFloat(parts[0], 64)
		return f
	}
	if len(parts) == 2 {
		n, _ := strconv.ParseFloat(parts[0], 64)
		d, _ := strconv.ParseFloat(parts[1], 64)
		if d == 0 {
			return 0
		}
		return n / d
	}
	return 0
}

func almostEqual(a, b float64) bool {
	diff := a - b
	if diff < 0 {
		diff = -diff
	}
	return diff < 0.0001
}

func parseInt64(s string) int64 {
	s = strings.TrimSpace(s)
	if s == "" {
		return 0
	}
	n, _ := strconv.ParseInt(s, 10, 64)
	return n
}

func parseFloat(s string) float64 {
	s = strings.TrimSpace(s)
	if s == "" {
		return 0
	}
	f, _ := strconv.ParseFloat(s, 64)
	return f
}

// --------------------- 高级数据处理：限制/过滤/摘要 ---------------------

func filterKeyFrames(in *media.FramesJSON) *media.FramesJSON {
	if in == nil {
		return in
	}
	out := &media.FramesJSON{Frames: make([]media.Frame, 0, len(in.Frames))}
	for _, f := range in.Frames {
		if f.MediaType == "video" && f.KeyFrame == 1 {
			out.Frames = append(out.Frames, f)
		}
	}
	return out
}

func limitFrames(in *media.FramesJSON, limit int) *media.FramesJSON {
	if in == nil || limit <= 0 || len(in.Frames) <= limit {
		return in
	}
	out := &media.FramesJSON{Frames: append([]media.Frame(nil), in.Frames[:limit]...)}
	return out
}

func limitPackets(in *media.PacketsJSON, limit int) *media.PacketsJSON {
	if in == nil || limit <= 0 || len(in.Packets) <= limit {
		return in
	}
	out := &media.PacketsJSON{Packets: append([]media.Packet(nil), in.Packets[:limit]...)}
	return out
}

func summarizeFrames(fr *media.FramesJSON) string {
	if fr == nil {
		return ""
	}
	total := len(fr.Frames)
	key := 0
	iCnt, pCnt, bCnt := 0, 0, 0
	for _, f := range fr.Frames {
		if f.MediaType == "video" {
			if f.KeyFrame == 1 {
				key++
			}
			switch strings.ToUpper(f.PictType) {
			case "I":
				iCnt++
			case "P":
				pCnt++
			case "B":
				bCnt++
			}
		}
	}
	msg := fmt.Sprintf("帧数量：%d", total)
	if total > 0 {
		msg += fmt.Sprintf("；关键帧：%d；I/P/B：%d/%d/%d", key, iCnt, pCnt, bCnt)
	}
	return msg
}

func summarizePackets(pk *media.PacketsJSON) string {
	if pk == nil {
		return ""
	}
	total := len(pk.Packets)
	vCnt, aCnt := 0, 0
	for _, p := range pk.Packets {
		switch p.CodecType {
		case "video":
			vCnt++
		case "audio":
			aCnt++
		}
	}
	return fmt.Sprintf("包数量：%d（视频：%d，音频：%d）", total, vCnt, aCnt)
}

// --------------------- 模板展示函数 ---------------------

func humanBitrate(bps int64) string {
	if bps <= 0 {
		return "-"
	}
	if bps >= 1_000_000 {
		return fmt.Sprintf("%.2f Mbps（约 %d kbps）", float64(bps)/1_000_000, bps/1_000)
	}
	if bps >= 1_000 {
		return fmt.Sprintf("%d kbps", bps/1_000)
	}
	return fmt.Sprintf("%d bps", bps)
}

func humanSize(bytes int64) string {
	if bytes <= 0 {
		return "-"
	}
	const (
		KB = 1024
		MB = 1024 * KB
		GB = 1024 * MB
	)
	if bytes >= GB {
		return fmt.Sprintf("%.2f GB", float64(bytes)/float64(GB))
	}
	if bytes >= MB {
		return fmt.Sprintf("%.2f MB", float64(bytes)/float64(MB))
	}
	if bytes >= KB {
		return fmt.Sprintf("%.2f KB", float64(bytes)/float64(KB))
	}
	return fmt.Sprintf("%d B", bytes)
}

func humanDuration(sec float64) string {
	if sec <= 0 {
		return "-"
	}
	totalMs := int64(sec * 1000)
	min := totalMs / 60000
	rem := totalMs % 60000
	s := rem / 1000
	ms := rem % 1000
	return fmt.Sprintf("%02d:%02d.%03d（%.3f 秒）", min, s, ms, sec)
}
