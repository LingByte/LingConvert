package main

import (
	"context"
	"crypto/rand"
	"encoding/hex"
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
	"sync"
	"time"

	"github.com/LingByte/LingConvert/media/ffmpeg"
	"github.com/LingByte/LingConvert/media/ffprobe"
	"github.com/gin-gonic/gin"
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

	FFJob *JobView // 新增：ffmpeg job
}

type JobView struct {
	ID     string
	Status string
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

// --------------------- ffmpeg Job 管理（内存任务表 + SSE 广播）---------------------

type sseEvent struct {
	Event string
	Data  string
}

type FFJob struct {
	ID        string
	Status    string // created/running/done/error
	CreatedAt time.Time

	InputPath    string
	InputDesc    string
	InputCleanup func()

	OutputPath string
	OutputName string

	ErrText string

	mu   sync.Mutex
	subs map[chan sseEvent]struct{}
}

type JobStore struct {
	mu   sync.Mutex
	jobs map[string]*FFJob
}

func NewJobStore() *JobStore {
	return &JobStore{jobs: map[string]*FFJob{}}
}

func (s *JobStore) Put(j *FFJob) {
	s.mu.Lock()
	s.jobs[j.ID] = j
	s.mu.Unlock()
}

func (s *JobStore) Get(id string) (*FFJob, bool) {
	s.mu.Lock()
	j, ok := s.jobs[id]
	s.mu.Unlock()
	return j, ok
}

func (s *JobStore) Delete(id string) {
	s.mu.Lock()
	delete(s.jobs, id)
	s.mu.Unlock()
}

func newID() string {
	b := make([]byte, 16)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

func (j *FFJob) subscribe() chan sseEvent {
	ch := make(chan sseEvent, 16)
	j.mu.Lock()
	if j.subs == nil {
		j.subs = map[chan sseEvent]struct{}{}
	}
	j.subs[ch] = struct{}{}
	j.mu.Unlock()
	return ch
}

func (j *FFJob) unsubscribe(ch chan sseEvent) {
	j.mu.Lock()
	if j.subs != nil {
		delete(j.subs, ch)
	}
	j.mu.Unlock()
	close(ch)
}

func (j *FFJob) broadcast(ev sseEvent) {
	j.mu.Lock()
	for ch := range j.subs {
		select {
		case ch <- ev:
		default:
			// slow subscriber -> drop
		}
	}
	j.mu.Unlock()
}

// --------------------- main ---------------------

func main() {
	r := gin.Default()

	// 模板函数：中文展示
	r.SetFuncMap(template.FuncMap{
		"humanBitrate":  humanBitrate,
		"humanSize":     humanSize,
		"humanDuration": humanDuration,
	})
	r.LoadHTMLGlob("templates/*.html")

	// ffprobe tool
	probeTool := ffprobe.NewDefaultTool()
	probeTool.Timeout = 25 * time.Second

	// ffmpeg tool
	ffTool := ffmpeg.NewDefaultFFmpeg()
	// ffmpeg 默认不建议死超时；如需限制可设置 ffTool.Timeout = 10*time.Minute 等
	// ffTool.Timeout = 0

	jobs := NewJobStore()

	r.GET("/", func(c *gin.Context) {
		c.HTML(http.StatusOK, "index.html", PageData{})
	})

	// --------------------- ffprobe 页面入口：上传 or URL + 选项 ---------------------
	r.POST("/probe", func(c *gin.Context) {
		ctx, cancel := context.WithTimeout(context.Background(), probeTool.Timeout)
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
		base, err := probeTool.Probe(ctx, input)
		if err != nil {
			c.HTML(http.StatusInternalServerError, "index.html", PageData{OK: false, Error: "ffprobe 基础解析失败: " + err.Error()})
			return
		}

		ver, _ := probeTool.Version(ctx)
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
			frames, err := probeTool.ProbeFrames(ctx, input, opt.FramesSelectStreams, opt.FramesReadIntervals)
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
			pkts, err := probeTool.ProbePackets(ctx, input, opt.PacketsSelectStreams)
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
			chs, err := probeTool.ProbeChapters(ctx, input)
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
			pgs, err := probeTool.ProbePrograms(ctx, input)
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

	// --------------------- ffmpeg：开始任务 ---------------------
	r.POST("/ffmpeg/start", func(c *gin.Context) {
		// 输入源：上传优先
		input, desc, cleanup, err := getInputFromRequest(c)
		if err != nil {
			c.HTML(http.StatusBadRequest, "index.html", PageData{OK: false, Error: err.Error()})
			return
		}

		action := strings.TrimSpace(c.PostForm("action"))
		outName := strings.TrimSpace(c.PostForm("out_name"))
		crf := parseInt(c.PostForm("crf"), 23)
		preset := strings.TrimSpace(c.PostForm("preset"))
		if preset == "" {
			preset = "medium"
		}
		abitrate := strings.TrimSpace(c.PostForm("abitrate"))
		if abitrate == "" {
			abitrate = "128k"
		}
		atSec, _ := strconv.ParseFloat(strings.TrimSpace(c.PostForm("at")), 64)
		if atSec < 0 {
			atSec = 0
		}

		if outName == "" {
			switch action {
			case "extract_aac":
				outName = "out.aac"
			case "snapshot":
				outName = "shot.jpg"
			case "remux":
				outName = "out.mp4"
			default:
				outName = "out.mp4"
			}
		}

		ext := filepath.Ext(outName)
		if ext == "" {
			ext = ".bin"
		}

		outFile, err := os.CreateTemp("", "ffout-*"+ext)
		if err != nil {
			if cleanup != nil {
				cleanup()
			}
			c.HTML(http.StatusInternalServerError, "index.html", PageData{OK: false, Error: "创建输出文件失败: " + err.Error()})
			return
		}
		_ = outFile.Close()

		job := &FFJob{
			ID:           newID(),
			Status:       "created",
			CreatedAt:    time.Now(),
			InputPath:    input,
			InputDesc:    desc,
			InputCleanup: cleanup,
			OutputPath:   outFile.Name(),
			OutputName:   outName,
		}
		jobs.Put(job)

		// 后台执行 ffmpeg
		go func() {
			defer func() {
				// 清理上传临时输入
				if job.InputCleanup != nil {
					job.InputCleanup()
				}
				// 输出文件保留一段时间后清理，避免磁盘堆满
				time.AfterFunc(30*time.Minute, func() {
					_ = os.Remove(job.OutputPath)
					jobs.Delete(job.ID)
				})
			}()

			job.Status = "running"
			job.broadcast(sseEvent{Event: "status", Data: "running"})

			// 构建命令（按你的 ffmpeg preset）
			var cmd *ffmpeg.FFmpegCommand
			switch action {
			case "extract_aac":
				cmd = ffmpeg.PresetExtractAAC(job.InputPath, job.OutputPath, abitrate)
			case "snapshot":
				cmd = ffmpeg.PresetSnapshot(job.InputPath, job.OutputPath, atSec)
			case "remux":
				cmd = ffmpeg.PresetRemux(job.InputPath, job.OutputPath)
			default:
				cmd = ffmpeg.PresetTranscodeMP4H264AAC(job.InputPath, job.OutputPath, crf, preset)
			}

			// 执行 + progress
			_, runErr := ffTool.RunWithProgress(context.Background(), cmd, func(p ffmpeg.FFmpegProgress) error {
				b, _ := json.Marshal(map[string]any{
					"frame":       p.Frame,
					"fps":         p.FPS,
					"out_time_ms": p.OutTimeMs,
					"speed":       p.Speed,
				})
				job.broadcast(sseEvent{Event: "progress", Data: string(b)})
				return nil
			})

			if runErr != nil {
				job.Status = "error"
				job.ErrText = runErr.Error()
				job.broadcast(sseEvent{Event: "status", Data: "error"})
				job.broadcast(sseEvent{Event: "fferror", Data: job.ErrText})
				return
			}

			job.Status = "done"
			job.broadcast(sseEvent{Event: "status", Data: "done"})
			donePayload, _ := json.Marshal(map[string]any{
				"download": "/ffmpeg/download/" + job.ID,
				"name":     job.OutputName,
			})
			job.broadcast(sseEvent{Event: "done", Data: string(donePayload)})
		}()

		// 直接渲染同一页，让前端用 SSE 订阅 job
		c.HTML(http.StatusOK, "index.html", PageData{
			OK: true,
			FFJob: &JobView{
				ID:     job.ID,
				Status: job.Status,
			},
		})
	})

	// --------------------- ffmpeg：SSE 事件流 ---------------------
	r.GET("/ffmpeg/events/:id", func(c *gin.Context) {
		id := c.Param("id")
		job, ok := jobs.Get(id)
		if !ok {
			c.Status(http.StatusNotFound)
			return
		}

		c.Header("Content-Type", "text/event-stream")
		c.Header("Cache-Control", "no-cache")
		c.Header("Connection", "keep-alive")

		flusher, ok := c.Writer.(http.Flusher)
		if !ok {
			c.Status(http.StatusInternalServerError)
			return
		}

		sub := job.subscribe()
		defer job.unsubscribe(sub)

		// 先发当前状态
		writeSSE(c.Writer, "status", job.Status)
		flusher.Flush()

		for {
			select {
			case <-c.Request.Context().Done():
				return
			case ev := <-sub:
				writeSSE(c.Writer, ev.Event, ev.Data)
				flusher.Flush()
				if ev.Event == "done" || ev.Event == "fferror" {
					return
				}
			}
		}
	})

	// --------------------- ffmpeg：下载输出 ---------------------
	r.GET("/ffmpeg/download/:id", func(c *gin.Context) {
		id := c.Param("id")
		job, ok := jobs.Get(id)
		if !ok {
			c.String(http.StatusNotFound, "job not found")
			return
		}
		if job.Status != "done" {
			c.String(http.StatusBadRequest, "job not done")
			return
		}
		c.Header("Content-Disposition", fmt.Sprintf(`attachment; filename="%s"`, sanitizeFilename(job.OutputName)))
		c.File(job.OutputPath)
	})

	log.Println("Listening on http://127.0.0.1:8080")
	_ = r.Run(":8080")
}

func writeSSE(w io.Writer, event, data string) {
	// SSE：event + data（多行 data 需拆行，这里简单替换换行）
	data = strings.ReplaceAll(data, "\r", "")
	data = strings.ReplaceAll(data, "\n", "\\n")
	_, _ = fmt.Fprintf(w, "event: %s\n", event)
	_, _ = fmt.Fprintf(w, "data: %s\n\n", data)
}

func sanitizeFilename(name string) string {
	name = strings.ReplaceAll(name, `"`, "")
	name = strings.ReplaceAll(name, "\n", "")
	name = strings.ReplaceAll(name, "\r", "")
	name = strings.TrimSpace(name)
	if name == "" {
		return "output.bin"
	}
	return name
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

func buildProbeView(p *ffprobe.FFProbeJSON, source string, ver string) *ProbeView {
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

func filterKeyFrames(in *ffprobe.FramesJSON) *ffprobe.FramesJSON {
	if in == nil {
		return in
	}
	out := &ffprobe.FramesJSON{Frames: make([]ffprobe.Frame, 0, len(in.Frames))}
	for _, f := range in.Frames {
		if f.MediaType == "video" && f.KeyFrame == 1 {
			out.Frames = append(out.Frames, f)
		}
	}
	return out
}

func limitFrames(in *ffprobe.FramesJSON, limit int) *ffprobe.FramesJSON {
	if in == nil || limit <= 0 || len(in.Frames) <= limit {
		return in
	}
	out := &ffprobe.FramesJSON{Frames: append([]ffprobe.Frame(nil), in.Frames[:limit]...)}
	return out
}

func limitPackets(in *ffprobe.PacketsJSON, limit int) *ffprobe.PacketsJSON {
	if in == nil || limit <= 0 || len(in.Packets) <= limit {
		return in
	}
	out := &ffprobe.PacketsJSON{Packets: append([]ffprobe.Packet(nil), in.Packets[:limit]...)}
	return out
}

func summarizeFrames(fr *ffprobe.FramesJSON) string {
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

func summarizePackets(pk *ffprobe.PacketsJSON) string {
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
