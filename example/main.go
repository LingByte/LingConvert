package main

import (
	"context"
	"fmt"
	"log"

	media "github.com/LingByte/LingConvert/media/ffprobe"
)

func main() {
	tool := media.NewDefaultTool()
	info, err := tool.Probe(context.Background(), "example.mp4")
	if err != nil {
		log.Fatal(err)
	}

	v := info.FirstVideo()
	a := info.FirstAudio()

	fmt.Println("Format:", info.Format.FormatName, "Duration:", info.Format.Duration, "Bitrate:", info.Format.BitRate)

	if v != nil {
		fmt.Printf("Video: %s %dx%d fps=%s avg=%s\n", v.CodecName, v.Width, v.Height, v.RFrameRate, v.AvgFrameRate)
	}
	if a != nil {
		fmt.Printf("Audio: %s %sHz ch=%d layout=%s\n", a.CodecName, a.SampleRate, a.Channels, a.ChannelLayout)
	}
}
