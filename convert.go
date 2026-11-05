//go:build !lite

package main

import (
	"bufio"
	"bytes"
	"compress/gzip"
	"context"
	_ "embed"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"strconv"
	"strings"
	"sync"
)

// ffmpeg.exe (GPL build, has libx264) is embedded gzip-compressed and unpacked to the temp dir on first use.
//
//go:embed assets/ffmpeg.exe.gz
var ffmpegGz []byte

var (
	ffOnce sync.Once
	ffPath string
	ffErr  error
)

func ffmpeg() (string, error) {
	ffOnce.Do(func() {
		if runtime.GOOS != "windows" {
			ffPath, ffErr = exec.LookPath("ffmpeg")
			return
		}
		dir := filepath.Join(os.TempDir(), "watchparty-ffmpeg")
		os.MkdirAll(dir, 0o755)
		p := filepath.Join(dir, fmt.Sprintf("ffmpeg-%d.exe", len(ffmpegGz)))
		if st, err := os.Stat(p); err == nil && st.Size() > 1<<20 {
			ffPath = p
			return
		}
		zr, err := gzip.NewReader(bytes.NewReader(ffmpegGz))
		if err != nil {
			ffErr = err
			return
		}
		tmp := p + ".part"
		f, err := os.Create(tmp)
		if err != nil {
			ffErr = err
			return
		}
		_, err = io.Copy(f, zr)
		f.Close()
		if err == nil {
			err = os.Rename(tmp, p)
		}
		ffPath, ffErr = p, err
	})
	return ffPath, ffErr
}

type Probe struct {
	Dur            float64
	HasV, HasA     bool
	VCodec, ACodec string
	VDetail        string
}

var (
	reDur    = regexp.MustCompile(`Duration: (\d+):(\d+):([\d.]+)`)
	reStream = regexp.MustCompile(`Stream #\d+:\d+[^:]*: (Video|Audio): (\w+)(.*)`)
)

func probe(path string) (Probe, error) {
	var p Probe
	ff, err := ffmpeg()
	if err != nil {
		return p, err
	}
	pc := exec.Command(ff, "-hide_banner", "-i", path)
	hideWindow(pc)
	out, _ := pc.CombinedOutput() // exits 1 (no output file); we only want the stream info
	s := string(out)
	if m := reDur.FindStringSubmatch(s); m != nil {
		h, _ := strconv.Atoi(m[1])
		mi, _ := strconv.Atoi(m[2])
		sec, _ := strconv.ParseFloat(m[3], 64)
		p.Dur = float64(h*3600+mi*60) + sec
	}
	for _, line := range strings.Split(s, "\n") {
		m := reStream.FindStringSubmatch(line)
		if m == nil || strings.Contains(line, "attached pic") {
			continue
		}
		if m[1] == "Video" && !p.HasV {
			p.HasV, p.VCodec, p.VDetail = true, m[2], m[3]
		} else if m[1] == "Audio" && !p.HasA {
			p.HasA, p.ACodec = true, m[2]
		}
	}
	if !p.HasV {
		return p, fmt.Errorf("no video stream found")
	}
	return p, nil
}

type Plan struct {
	Reasons      []string
	VCopy, ACopy bool
}

func (pl Plan) Fast() bool { return pl.VCopy }

// makePlan lists why a browser can't play the file as-is (empty Reasons = plays fine).
func makePlan(p Probe, ext string) Plan {
	var pl Plan
	vOK := p.VCodec == "h264" || p.VCodec == "vp8" || p.VCodec == "vp9" || p.VCodec == "av1"
	d := p.VDetail
	if strings.Contains(d, "10le") || strings.Contains(d, "10be") || strings.Contains(d, "444") || strings.Contains(d, "High 10") {
		vOK = false
	}
	aOK := !p.HasA || strings.Contains(" aac mp3 opus vorbis flac ", " "+p.ACodec+" ")
	cOK := ext == ".mp4" || ext == ".m4v" || ext == ".webm" || ext == ".mov"
	if !cOK {
		pl.Reasons = append(pl.Reasons, ext+" container")
	}
	if !vOK {
		pl.Reasons = append(pl.Reasons, "video format "+p.VCodec)
	}
	if !aOK {
		pl.Reasons = append(pl.Reasons, "audio format "+p.ACodec)
	}
	pl.VCopy = vOK && p.VCodec != "vp8"
	pl.ACopy = !p.HasA || p.ACodec == "aac" || p.ACodec == "mp3"
	return pl
}

type Conv struct {
	Running bool
	Percent float64
	Err     string
	Name    string
	ItemID  int
	cancel  context.CancelFunc
}

func (c *Conv) itemID() int { return c.ItemID }

var conv = &Conv{}

// probeItem fills in whether a browser can play the file as-is.
func probeItem(it *Item) {
	p, err := probe(it.Path)
	if err != nil {
		return // can't analyse it: let it play as-is
	}
	it.Dur = p.Dur
	if pl := makePlan(p, strings.ToLower(filepath.Ext(it.Path))); len(pl.Reasons) > 0 {
		it.NeedsConv, it.Reasons, it.VCopy, it.ACopy = true, pl.Reasons, pl.VCopy, pl.ACopy
	}
}

// playConverted handles items that browsers can't play directly: they are converted live while playing.
func playConverted(it *Item) (bool, error) {
	if !it.NeedsConv || it.Dur <= 0 {
		return false, nil
	}
	return true, startLiveFor(it.Path, it.Dur, it.ID)
}

// startConvertItem converts one playlist item to an .mp4 file in the background (host's choice, e.g. to pre-convert a season).
func startConvertItem(id int) error {
	room.mu.Lock()
	var cp *Item
	for _, it := range room.playlist {
		if it.ID == id {
			c := *it
			cp = &c
		}
	}
	if cp == nil || !cp.NeedsConv {
		room.mu.Unlock()
		return fmt.Errorf("nothing to convert")
	}
	if conv.Running {
		room.mu.Unlock()
		return fmt.Errorf("another conversion is already running")
	}
	ctx, cancel := context.WithCancel(context.Background())
	conv.Running, conv.Percent, conv.Err, conv.cancel, conv.Name, conv.ItemID = true, 0, "", cancel, cp.Name, id
	room.mu.Unlock()
	go runConvert(ctx, *cp)
	return nil
}

func runConvert(ctx context.Context, it Item) {
	fail := func(msg string) {
		room.mu.Lock()
		conv.Running, conv.Err = false, msg
		room.broadcast()
		room.mu.Unlock()
	}
	ff, err := ffmpeg()
	if err != nil {
		fail(err.Error())
		return
	}
	ext := filepath.Ext(it.Path)
	out := strings.TrimSuffix(it.Path, ext) + ".watchparty.mp4"
	if f, err := os.OpenFile(out, os.O_CREATE|os.O_WRONLY, 0o644); err != nil { // source folder read-only?
		out = filepath.Join(os.TempDir(), filepath.Base(strings.TrimSuffix(it.Path, ext))+".watchparty.mp4")
	} else {
		f.Close()
	}
	args := []string{"-y", "-hide_banner", "-i", it.Path, "-map", "0:v:0", "-map", "0:a:0?", "-sn", "-dn"}
	if it.VCopy {
		args = append(args, "-c:v", "copy")
	} else {
		args = append(args, "-c:v", "libx264", "-preset", "veryfast", "-crf", "20", "-pix_fmt", "yuv420p")
	}
	if it.ACopy {
		args = append(args, "-c:a", "copy")
	} else {
		args = append(args, "-c:a", "aac", "-b:a", "192k", "-ac", "2")
	}
	args = append(args, "-movflags", "+faststart", "-progress", "pipe:1", "-nostats", out)
	cmd := exec.CommandContext(ctx, ff, args...)
	hideWindow(cmd)
	stdout, _ := cmd.StdoutPipe()
	var errBuf bytes.Buffer
	cmd.Stderr = &errBuf
	if err := cmd.Start(); err != nil {
		fail(err.Error())
		return
	}
	sc := bufio.NewScanner(stdout)
	for sc.Scan() {
		k, v, _ := strings.Cut(sc.Text(), "=")
		if k == "out_time_us" || k == "out_time_ms" {
			if us, err := strconv.ParseFloat(v, 64); err == nil && it.Dur > 0 {
				room.mu.Lock()
				conv.Percent = min(99, us/1e6/it.Dur*100)
				room.mu.Unlock()
			}
		}
	}
	if err := cmd.Wait(); err != nil {
		os.Remove(out)
		if ctx.Err() != nil {
			fail("")
			return
		}
		tail := errBuf.String()
		if len(tail) > 400 {
			tail = tail[len(tail)-400:]
		}
		fail("Conversion failed: " + strings.TrimSpace(tail))
		return
	}
	origOf.Store(out, it.Path) // subtitles are still read from the original
	room.mu.Lock()
	conv.Running, conv.Percent = false, 100
	for _, pi := range room.playlist {
		if pi.ID == it.ID {
			pi.Path, pi.NeedsConv, pi.Reasons = out, false, nil
		}
	}
	room.plVer++
	room.broadcast()
	room.mu.Unlock()
}

// convJSON must be called with room.mu held.
func convJSON() any {
	return map[string]any{"running": conv.Running, "percent": conv.Percent, "err": conv.Err, "name": conv.Name, "itemId": conv.ItemID}
}
