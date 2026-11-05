//go:build !lite

package main

import (
	"context"
	"fmt"
	"math"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"
)

const (
	segLen    = 4  // seconds per HLS segment; keyframes are forced on this grid so the whole playlist can be published up front
	lookSegs  = 15 // a request this far past the encoder's frontier (60 s) is treated as a seek: restart the encoder there
	segWaitTo = 3 * time.Minute
)

// Live is a conversion that viewers can play from while ffmpeg is still working. Segments sit on a fixed 4 s grid, so
// the encoder can be (re)started at any segment: jumping far ahead restarts it there instead of waiting for it to
// crawl through everything before.
type Live struct {
	dir  string
	path string
	dur  float64
	nseg int
	ff   string

	ctx    context.Context
	cancel context.CancelFunc

	mu     sync.Mutex
	job    *encJob
	jobSeq int
}

type encJob struct {
	id     int
	start  int
	cancel context.CancelFunc
	done   chan struct{}
	have   map[int]bool // segments that already existed when this job started (made by an earlier job)
}

func (j *encJob) finished() bool {
	select {
	case <-j.done:
		return true
	default:
		return false
	}
}

func (l *Live) segPath(i int) string { return filepath.Join(l.dir, fmt.Sprintf("seg%05d.ts", i)) }

func (l *Live) exists(i int) bool {
	_, err := os.Stat(l.segPath(i))
	return err == nil
}

// next is the segment this job will produce next (contiguous from its start, counting only what it made itself).
func (j *encJob) next(l *Live) int {
	i := j.start
	for i < l.nseg && l.exists(i) && !j.have[i] {
		i++
	}
	return i
}

// startJobLocked (re)starts the encoder at segment n. Call with l.mu held.
func (l *Live) startJobLocked(n int) error {
	if l.job != nil {
		l.job.cancel()
	}
	have := map[int]bool{}
	for i := n; i < l.nseg; i++ {
		if l.exists(i) {
			have[i] = true
		}
	}
	ctx, cancel := context.WithCancel(l.ctx)
	j := &encJob{id: l.jobSeq, start: n, cancel: cancel, done: make(chan struct{}), have: have}
	l.jobSeq++

	var args []string
	// -readrate 4: never encode faster than 4x playback speed (no point racing ahead, and it keeps the host PC's CPU free)
	// -threads: leave some cores for the host's own browser/audio
	args = append(args, "-hide_banner", "-y", "-readrate", "4", "-threads", encThreads())
	if n > 0 {
		args = append(args, "-ss", strconv.Itoa(n*segLen)) // accurate seek: decodes from the previous keyframe up to here
	}
	args = append(args, "-i", l.path, "-threads", encThreads(), "-map", "0:v:0", "-map", "0:a:0?", "-sn", "-dn",
		"-c:v", "libx264", "-preset", "veryfast", "-crf", "23", "-pix_fmt", "yuv420p",
		"-sc_threshold", "0", "-force_key_frames", fmt.Sprintf("expr:gte(t,n_forced*%d)", segLen),
		"-c:a", "aac", "-b:a", "160k", "-ac", "2")
	if n > 0 {
		args = append(args, "-output_ts_offset", strconv.Itoa(n*segLen)) // keep timestamps on the real timeline
	}
	args = append(args, "-f", "hls", "-hls_time", strconv.Itoa(segLen), "-hls_list_size", "0", "-hls_flags", "temp_file",
		"-start_number", strconv.Itoa(n),
		"-hls_segment_filename", filepath.Join(l.dir, "seg%05d.ts"), filepath.Join(l.dir, fmt.Sprintf("job%d.m3u8", j.id)))

	cmd := exec.CommandContext(ctx, l.ff, args...)
	hideWindow(cmd)
	if err := cmd.Start(); err != nil {
		cancel()
		return err
	}
	l.job = j
	go func() {
		cmd.Wait()
		close(j.done)
	}()
	go l.monitor(j)
	return nil
}

// monitor stops a job that is about to redo segments an earlier job already made, and moves on to the first gap.
func (l *Live) monitor(j *encJob) {
	t := time.NewTicker(300 * time.Millisecond)
	defer t.Stop()
	for {
		select {
		case <-j.done:
			return
		case <-t.C:
		}
		l.mu.Lock()
		if l.job != j {
			l.mu.Unlock()
			return
		}
		nx := j.next(l)
		if nx < l.nseg && j.have[nx] {
			m := nx
			for m < l.nseg && l.exists(m) {
				m++
			}
			if m >= l.nseg {
				j.cancel()
			} else {
				l.startJobLocked(m)
			}
			l.mu.Unlock()
			return
		}
		l.mu.Unlock()
	}
}

// ensure makes sure segment n is being (or will soon be) produced; far-away requests restart the encoder there.
func (l *Live) ensure(n int) error {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.exists(n) {
		return nil
	}
	if j := l.job; j != nil && !j.finished() {
		if nx := j.next(l); n >= j.start && n < nx+lookSegs {
			return nil // the running encoder will get there soon
		}
	}
	return l.startJobLocked(n)
}

func (l *Live) alive() bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.job != nil && !l.job.finished()
}

func (l *Live) stop() {
	l.cancel()
	l.mu.Lock()
	j := l.job
	l.mu.Unlock()
	go func() {
		if j != nil {
			<-j.done
		}
		os.RemoveAll(l.dir)
	}()
}

func startLiveFor(path string, dur float64, itemID int) error {
	if dur <= 0 {
		return fmt.Errorf("cannot play this live (unknown length)")
	}
	ff, err := ffmpeg()
	if err != nil {
		return err
	}
	dir, err := os.MkdirTemp("", "watchparty-live-")
	if err != nil {
		return err
	}
	ctx, cancel := context.WithCancel(context.Background())
	l := &Live{dir: dir, path: path, dur: dur, nseg: int(math.Ceil(dur / segLen)), ff: ff, ctx: ctx, cancel: cancel}
	l.mu.Lock()
	err = l.startJobLocked(0)
	l.mu.Unlock()
	if err != nil {
		cancel()
		os.RemoveAll(dir)
		return err
	}
	return room.setFileLive(path, l, itemID)
}

func (l *Live) playlist() string {
	var b strings.Builder
	fmt.Fprintf(&b, "#EXTM3U\n#EXT-X-VERSION:3\n#EXT-X-TARGETDURATION:%d\n#EXT-X-MEDIA-SEQUENCE:0\n#EXT-X-PLAYLIST-TYPE:VOD\n", segLen+1)
	for i := 0; i < l.nseg; i++ {
		d := float64(segLen)
		if i == l.nseg-1 {
			d = l.dur - float64(segLen*(l.nseg-1))
		}
		fmt.Fprintf(&b, "#EXTINF:%.3f,\nseg%05d.ts\n", d, i)
	}
	b.WriteString("#EXT-X-ENDLIST\n")
	return b.String()
}

var reSeg = regexp.MustCompile(`^seg(\d{5})\.ts$`)

func handleLive(w http.ResponseWriter, req *http.Request) {
	room.mu.Lock()
	l := room.live
	room.mu.Unlock()
	if l == nil {
		http.NotFound(w, req)
		return
	}
	name := strings.TrimPrefix(req.URL.Path, "/live/")
	w.Header().Set("Cache-Control", "no-store")
	if name == "index.m3u8" {
		w.Header().Set("Content-Type", "application/vnd.apple.mpegurl")
		fmt.Fprint(w, l.playlist())
		return
	}
	m := reSeg.FindStringSubmatch(name)
	if m == nil {
		http.NotFound(w, req)
		return
	}
	n, _ := strconv.Atoi(m[1])
	if n >= l.nseg {
		http.NotFound(w, req)
		return
	}
	if err := l.ensure(n); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	// ffmpeg renames a segment into place only once it is complete (hls_flags temp_file), so wait for it
	deadline := time.Now().Add(segWaitTo)
	for !l.exists(n) {
		select {
		case <-req.Context().Done():
			return
		case <-time.After(80 * time.Millisecond):
		}
		if !l.alive() && !l.exists(n) {
			http.Error(w, "conversion stopped", http.StatusGone)
			return
		}
		if time.Now().After(deadline) {
			http.Error(w, "segment not ready", http.StatusGatewayTimeout)
			return
		}
	}
	w.Header().Set("Content-Type", "video/mp2t")
	http.ServeFile(w, req, l.segPath(n))
}
