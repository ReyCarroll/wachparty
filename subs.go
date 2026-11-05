//go:build !lite

package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode/utf8"
)

// origOf maps a converted file back to the video it came from, so subtitles are found next to / inside the original.
const hasSubs = true

var origOf sync.Map

var subDir string // temp dir for the current video's .vtt files; guarded by room.mu

// afterFile runs (in the background) every time a new video goes live: find sidecar subtitles, then extract embedded ones.
func afterFile(path string, id int) {
	src := path
	if o, ok := origOf.Load(path); ok {
		src = o.(string)
	}
	room.mu.Lock()
	if subDir != "" {
		os.RemoveAll(subDir)
	}
	dir, _ := os.MkdirTemp("", "watchparty-subs-")
	subDir = dir
	room.mu.Unlock()
	go func() {
		base := strings.TrimSuffix(filepath.Base(src), filepath.Ext(src))
		matches, _ := filepath.Glob(filepath.Join(filepath.Dir(src), globEscape(base)+"*"))
		sort.Strings(matches)
		for _, m := range matches {
			switch strings.ToLower(filepath.Ext(m)) {
			case ".srt", ".vtt", ".ass", ".ssa":
				label := strings.Trim(strings.TrimSuffix(strings.TrimPrefix(filepath.Base(m), base), filepath.Ext(m)), ". _-")
				if label == "" {
					label = "Subtitles"
				}
				if err := addSubFile(id, dir, label, m); err != nil {
					log.Println("subtitle", m, "skipped:", err)
				}
			}
		}
		extractEmbedded(id, dir, src)
	}()
}

func globEscape(s string) string { return regexp.MustCompile(`[\[\]*?\\]`).ReplaceAllString(s, `\$0`) }

var reSub = regexp.MustCompile(`Stream #0:(\d+)(?:\[[^\]]*\])?(?:\((\w+)\))?[^:]*: Subtitle: (\w+)`)

func extractEmbedded(id int, dir, src string) {
	ff, err := ffmpeg()
	if err != nil {
		return
	}
	pc := exec.Command(ff, "-hide_banner", "-i", src)
	hideWindow(pc)
	out, _ := pc.CombinedOutput()
	n := 0
	for _, line := range strings.Split(string(out), "\n") {
		m := reSub.FindStringSubmatch(line)
		if m == nil {
			continue
		}
		idx, codec, lang := m[1], m[3], m[2]
		n++
		if codec == "hdmv_pgs_subtitle" || codec == "dvd_subtitle" || codec == "dvb_subtitle" || codec == "xsub" {
			continue // picture-based, can't be shown as text
		}
		label := fmt.Sprintf("Embedded %d", n)
		if lang != "" {
			label = lang + " (embedded)"
		}
		if !currentFile(id) {
			return
		}
		dst := filepath.Join(dir, fmt.Sprintf("emb%s.vtt", idx))
		if runFF(ff, 15*time.Minute, "-y", "-i", src, "-map", "0:"+idx, "-f", "webvtt", dst) == nil {
			registerSub(id, label, dst)
		}
	}
}

func runFF(ff string, d time.Duration, args ...string) error {
	ctx, cancel := context.WithTimeout(context.Background(), d)
	defer cancel()
	c := exec.CommandContext(ctx, ff, append([]string{"-hide_banner", "-loglevel", "error"}, args...)...)
	hideWindow(c)
	return c.Run()
}

func currentFile(id int) bool {
	room.mu.Lock()
	defer room.mu.Unlock()
	return room.fileID == id
}

func registerSub(id int, label, vtt string) {
	room.mu.Lock()
	defer room.mu.Unlock()
	if room.fileID != id {
		return
	}
	room.subs = append(room.subs, Sub{ID: len(room.subs), Label: label, path: vtt})
}

// addSubFile converts a .srt/.ass/.vtt (any text encoding) to UTF-8 WebVTT.
func addSubFile(id int, dir, label, p string) error {
	ff, err := ffmpeg()
	if err != nil {
		return err
	}
	raw, err := os.ReadFile(p)
	if err != nil {
		return err
	}
	if !utf8.Valid(raw) {
		raw = []byte(cp1252ToUTF8(raw))
	}
	raw = bytes.TrimPrefix(raw, []byte{0xEF, 0xBB, 0xBF})
	tmp, err := os.CreateTemp(dir, "in-*"+strings.ToLower(filepath.Ext(p)))
	if err != nil {
		return err
	}
	tmp.Write(raw)
	tmp.Close()
	defer os.Remove(tmp.Name())
	dst := filepath.Join(dir, fmt.Sprintf("f%d.vtt", time.Now().UnixNano()))
	if err := runFF(ff, 2*time.Minute, "-y", "-i", tmp.Name(), "-f", "webvtt", dst); err != nil {
		return err
	}
	registerSub(id, label, dst)
	return nil
}

var cp1252hi = []rune("€\u0081‚ƒ„…†‡ˆ‰Š‹Œ\u008dŽ\u008f\u0090‘’“”•–—˜™š›œ\u009džŸ")

func cp1252ToUTF8(b []byte) string {
	r := make([]rune, len(b))
	for i, c := range b {
		switch {
		case c >= 0x80 && c < 0xA0:
			r[i] = cp1252hi[c-0x80]
		default:
			r[i] = rune(c)
		}
	}
	return string(r)
}

func handleSubs(w http.ResponseWriter, req *http.Request) {
	room.mu.Lock()
	defer room.mu.Unlock()
	list := []Sub{}
	list = append(list, room.subs...)
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	json.NewEncoder(w).Encode(list)
}

func handleSubFile(w http.ResponseWriter, req *http.Request) {
	n, err := strconv.Atoi(strings.TrimSuffix(strings.TrimPrefix(req.URL.Path, "/sub/"), ".vtt"))
	room.mu.Lock()
	var p string
	if err == nil && n >= 0 && n < len(room.subs) {
		p = room.subs[n].path
	}
	room.mu.Unlock()
	if p == "" {
		http.NotFound(w, req)
		return
	}
	w.Header().Set("Content-Type", "text/vtt; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	http.ServeFile(w, req, p)
}

func handleSubPick(w http.ResponseWriter, req *http.Request) {
	if !isLocal(req) {
		http.Error(w, "host only", 403)
		return
	}
	if runtimeGOOS() != "windows" {
		http.Error(w, "file dialog only on Windows", 500)
		return
	}
	room.mu.Lock()
	id, dir := room.fileID, subDir
	room.mu.Unlock()
	if id == 0 {
		http.Error(w, "pick a video first", 400)
		return
	}
	p, err := pickSubtitle()
	if err != nil || p == "" {
		if err != nil {
			http.Error(w, err.Error(), 500)
			return
		}
		w.WriteHeader(204)
		return
	}
	if err := addSubFile(id, dir, strings.TrimSuffix(filepath.Base(p), filepath.Ext(p)), p); err != nil {
		http.Error(w, "could not read that subtitle file", 500)
		return
	}
	w.WriteHeader(204)
}
