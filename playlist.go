package main

import (
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// Item is one video in the shared playlist. Anyone connected can pick which one plays.
type Item struct {
	ID         int      `json:"id"`
	Name       string   `json:"name"`
	NeedsConv  bool     `json:"needsConv"` // browsers can't play it as-is (full build converts it live)
	Reasons    []string `json:"reasons"`
	Group      string   `json:"group"` // folder path inside the added folder, "/"-separated ("" = loose file)
	Dur        float64  `json:"dur"`
	Converting bool     `json:"converting"`
	Path       string   `json:"-"`
	VCopy      bool     `json:"-"`
	ACopy      bool     `json:"-"`
}

func isDigit(c byte) bool { return c >= '0' && c <= '9' }

// naturalLess sorts "Ep 2" before "Ep 10".
func naturalLess(a, b string) bool {
	a, b = strings.ToLower(a), strings.ToLower(b)
	i, j := 0, 0
	for i < len(a) && j < len(b) {
		if isDigit(a[i]) && isDigit(b[j]) {
			si, sj := i, j
			for i < len(a) && isDigit(a[i]) {
				i++
			}
			for j < len(b) && isDigit(b[j]) {
				j++
			}
			na, nb := strings.TrimLeft(a[si:i], "0"), strings.TrimLeft(b[sj:j], "0")
			if len(na) != len(nb) {
				return len(na) < len(nb)
			}
			if na != nb {
				return na < nb
			}
			continue
		}
		if a[i] != b[j] {
			return a[i] < b[j]
		}
		i++
		j++
	}
	return len(a)-i < len(b)-j
}

var videoExts = map[string]bool{".mp4": true, ".m4v": true, ".webm": true, ".mkv": true, ".mov": true, ".ogv": true,
	".avi": true, ".ts": true, ".wmv": true, ".flv": true, ".mpg": true, ".mpeg": true}

const maxScan = 5000 // safety cap when a whole library folder is added

type entry struct{ path, group string }

func cmpNatural(a, b string) int {
	if naturalLess(a, b) {
		return -1
	}
	if naturalLess(b, a) {
		return 1
	}
	return 0
}

// entryLess orders loose files first, then folder by folder (Season 2 before Season 10), episodes in natural order.
func entryLess(a, b entry) bool {
	if (a.group == "") != (b.group == "") {
		return a.group == ""
	}
	pa := append(strings.Split(a.group, "/"), filepath.Base(a.path))
	pb := append(strings.Split(b.group, "/"), filepath.Base(b.path))
	for i := 0; i < len(pa) && i < len(pb); i++ {
		if c := cmpNatural(pa[i], pb[i]); c != 0 {
			return c < 0
		}
	}
	return len(pa) < len(pb)
}

// scanPaths expands chosen files and folders (any depth) into video entries, remembering each video's folder.
func scanPaths(paths []string) []entry {
	var out []entry
	for _, p := range paths {
		st, err := os.Stat(p)
		if err != nil {
			continue
		}
		if !st.IsDir() {
			out = append(out, entry{p, ""})
			continue
		}
		root := strings.Trim(filepath.Base(p), `\/: .`)
		if root == "" {
			root = "Videos"
		}
		filepath.WalkDir(p, func(path string, d os.DirEntry, err error) error {
			if err != nil {
				return nil // unreadable folder: skip it
			}
			if d.IsDir() {
				if path != p && strings.HasPrefix(d.Name(), ".") {
					return filepath.SkipDir
				}
				return nil
			}
			low := strings.ToLower(d.Name())
			if !videoExts[filepath.Ext(low)] || strings.Contains(low, ".watchparty.") {
				return nil // not a video, or one of our own converted copies
			}
			rel, _ := filepath.Rel(p, filepath.Dir(path))
			group := root
			if rel != "." {
				group = root + "/" + filepath.ToSlash(rel)
			}
			out = append(out, entry{path, group})
			if len(out) >= maxScan {
				return filepath.SkipAll
			}
			return nil
		})
	}
	sort.SliceStable(out, func(i, j int) bool { return entryLess(out[i], out[j]) })
	return out
}

// addFiles appends videos and/or whole folders (with subfolders) to the playlist, probing each in the background.
// If nothing is loaded yet, the first one is loaded, paused.
func addFiles(paths []string) {
	paths = append([]string(nil), paths...)
	go func() {
		for _, e := range scanPaths(paths) {
			room.mu.Lock()
			dup := false
			for _, it := range room.playlist {
				dup = dup || it.Path == e.path
			}
			room.mu.Unlock()
			if dup {
				continue
			}
			it := &Item{Name: filepath.Base(e.path), Path: e.path, Group: e.group}
			probeItem(it)
			room.mu.Lock()
			room.nextID++
			it.ID = room.nextID
			room.playlist = append(room.playlist, it)
			room.plVer++
			first := room.path == ""
			room.broadcast()
			room.mu.Unlock()
			if first {
				playItem(it.ID, false)
			}
		}
	}()
}

// playItem switches the whole room to a playlist item. autoplay starts it as soon as everyone has buffered.
func playItem(id int, autoplay bool) error {
	room.mu.Lock()
	var cp Item
	found := false
	for _, it := range room.playlist {
		if it.ID == id {
			cp, found = *it, true
		}
	}
	room.mu.Unlock()
	if !found {
		return fmt.Errorf("that video is no longer in the playlist")
	}
	handled, err := playConverted(&cp)
	if !handled {
		err = room.setFileLive(cp.Path, nil, cp.ID)
	}
	if err != nil {
		return err
	}
	room.mu.Lock()
	room.desired = autoplay
	room.recompute()
	room.broadcast()
	room.mu.Unlock()
	return nil
}

// neighbors returns the playlist items before/after the one playing. If the playing video was removed from the
// list, it falls back to where it used to sit. Call with room.mu held.
func (r *Room) neighbors() (prev, next int) {
	has := func(id int) bool {
		for _, it := range r.playlist {
			if it.ID == id {
				return true
			}
		}
		return false
	}
	for i, it := range r.playlist {
		if it.ID == r.curItem {
			if i > 0 {
				prev = r.playlist[i-1].ID
			}
			if i+1 < len(r.playlist) {
				next = r.playlist[i+1].ID
			}
			return
		}
	}
	if has(r.hintPrev) {
		prev = r.hintPrev
	}
	if has(r.hintNext) {
		next = r.hintNext
	}
	return
}

// handleEnded advances to the next video when the current one finishes (if auto-play next is on).
func handleEnded(fileID int) {
	room.mu.Lock()
	next := 0
	if room.fileID == fileID && room.autoNext {
		_, next = room.neighbors()
	}
	room.mu.Unlock()
	if next != 0 {
		playItem(next, true)
	}
}

func handlePlaylist(w http.ResponseWriter, req *http.Request) {
	room.mu.Lock()
	defer room.mu.Unlock()
	items := make([]Item, 0, len(room.playlist))
	for _, it := range room.playlist {
		c := *it
		c.Converting = conv.Running && conv.itemID() == it.ID
		items = append(items, c)
	}
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	prev, next := room.neighbors()
	json.NewEncoder(w).Encode(map[string]any{"items": items, "cur": room.curItem, "autoNext": room.autoNext, "plVer": room.plVer, "prevId": prev, "nextId": next})
}

func handlePlay(w http.ResponseWriter, req *http.Request) {
	var m struct {
		ID int `json:"id"`
	}
	if json.NewDecoder(req.Body).Decode(&m) != nil {
		http.Error(w, "bad json", 400)
		return
	}
	if err := playItem(m.ID, true); err != nil {
		http.Error(w, err.Error(), 400)
		return
	}
	w.WriteHeader(204)
}

func handleRemove(w http.ResponseWriter, req *http.Request) {
	var m struct {
		ID    int    `json:"id"`    // 0 and no group = clear all
		Group string `json:"group"` // remove a whole folder (and its subfolders)
	}
	json.NewDecoder(req.Body).Decode(&m)
	room.mu.Lock()
	before := append([]*Item(nil), room.playlist...)
	if m.Group != "" {
		keep := room.playlist[:0:0]
		for _, it := range room.playlist {
			if it.Group != m.Group && !strings.HasPrefix(it.Group, m.Group+"/") {
				keep = append(keep, it)
			}
		}
		room.playlist = keep
	} else if m.ID == 0 {
		room.playlist = nil
	} else {
		keep := room.playlist[:0:0]
		for _, it := range room.playlist {
			if it.ID != m.ID {
				keep = append(keep, it)
			}
		}
		room.playlist = keep
	}
	// if the playing video just left the list, remember where it was so Next / auto-advance still work
	stillThere := false
	for _, it := range room.playlist {
		stillThere = stillThere || it.ID == room.curItem
	}
	if !stillThere && room.curItem != 0 {
		kept := map[int]bool{}
		for _, it := range room.playlist {
			kept[it.ID] = true
		}
		ci := -1
		for i, it := range before {
			if it.ID == room.curItem {
				ci = i
			}
		}
		if ci >= 0 {
			room.hintPrev, room.hintNext = 0, 0
			for i := ci - 1; i >= 0; i-- {
				if kept[before[i].ID] {
					room.hintPrev = before[i].ID
					break
				}
			}
			for i := ci + 1; i < len(before); i++ {
				if kept[before[i].ID] {
					room.hintNext = before[i].ID
					break
				}
			}
		}
	}
	room.plVer++
	room.broadcast()
	room.mu.Unlock()
	w.WriteHeader(204)
}

func handleItemConvert(w http.ResponseWriter, req *http.Request) {
	var m struct {
		ID int `json:"id"`
	}
	json.NewDecoder(req.Body).Decode(&m)
	if err := startConvertItem(m.ID); err != nil {
		http.Error(w, err.Error(), 400)
		return
	}
	w.WriteHeader(204)
}
