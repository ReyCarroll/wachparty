package main

import (
	"embed"
	"encoding/json"
	"fmt"
	"io/fs"
	"log"
	"mime"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"sync"
	"time"
)

//go:embed web/*
var webFS embed.FS

const basePort = 8765

type client struct {
	id, name string
	ready    bool
	forced   bool // host/anyone pressed "skip waiting" for this client
	note     string
	ch       chan []byte
}

type Room struct {
	mu      sync.Mutex
	path    string
	name    string
	mime    string
	size    int64
	fileID  int
	desired bool // someone pressed play
	running bool // desired && everyone ready
	pos     float64
	at      time.Time
	clients map[string]*client
	live    *Live
	subs    []Sub

	playlist []*Item
	curItem  int // ID of the playlist item currently loaded (0 = none)
	plVer    int // bumped whenever the list itself changes
	nextID   int
	hintPrev int // neighbours of the playing video if it was removed from the list
	hintNext int
	autoNext bool
}

// Sub is a subtitle track (already converted to WebVTT) offered to every viewer.
type Sub struct {
	ID    int    `json:"id"`
	Label string `json:"label"`
	path  string
}

var room = &Room{clients: map[string]*client{}, autoNext: true}

func (r *Room) curPos() float64 {
	if r.running {
		return r.pos + time.Since(r.at).Seconds()
	}
	return r.pos
}

// recompute must be called with r.mu held.
func (r *Room) recompute() {
	all := len(r.clients) > 0
	for _, c := range r.clients {
		if !c.ready && !c.forced {
			all = false
		}
	}
	run := r.desired && all && r.path != ""
	if run != r.running {
		r.pos = r.curPos()
		r.at = time.Now()
		r.running = run
	}
}

// snapshot must be called with r.mu held.
func (r *Room) snapshot() []byte {
	type cl struct {
		Name  string `json:"name"`
		Ready bool   `json:"ready"`
		Note  string `json:"note"`
	}
	cls := []cl{}
	for _, c := range r.clients {
		cls = append(cls, cl{c.name, c.ready || c.forced, c.note})
	}
	b, _ := json.Marshal(map[string]any{
		"fileId":   r.fileID,
		"desired":  r.desired,
		"running":  r.running,
		"pos":      r.curPos(),
		"sent":     time.Now().UnixMilli(),
		"clients":  cls,
		"plVer":    r.plVer,
		"cur":      r.curItem,
		"autoNext": r.autoNext,
	})
	return b
}

// broadcast must be called with r.mu held.
func (r *Room) broadcast() {
	b := r.snapshot()
	for _, c := range r.clients {
		select {
		case c.ch <- b:
		default: // slow client; the heartbeat will catch it up
		}
	}
}

func (r *Room) setFileLive(p string, lv *Live, itemID int) error {
	st, err := os.Stat(p)
	if err != nil || st.IsDir() {
		return fmt.Errorf("cannot open %s", p)
	}
	m := mime.TypeByExtension(strings.ToLower(filepath.Ext(p)))
	switch strings.ToLower(filepath.Ext(p)) {
	case ".mkv":
		m = "video/webm" // Chrome/Edge play H.264/VP9 + AAC/Opus mkv when served as webm
	case ".mp4", ".m4v":
		m = "video/mp4"
	case "":
		m = "application/octet-stream"
	}
	if m == "" {
		m = "video/mp4"
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.live != nil {
		r.live.stop()
	}
	r.live = lv
	r.subs = nil
	r.curItem = itemID
	r.path, r.name, r.mime, r.size = p, filepath.Base(p), m, st.Size()
	r.fileID++
	r.desired, r.running, r.pos, r.at = false, false, 0, time.Now()
	for _, c := range r.clients {
		c.ready, c.forced = false, false
	}
	r.broadcast()
	go afterFile(p, r.fileID)
	return nil
}

func isLocal(req *http.Request) bool {
	h, _, _ := net.SplitHostPort(req.RemoteAddr)
	ip := net.ParseIP(h)
	return ip != nil && ip.IsLoopback()
}

func handleEvents(w http.ResponseWriter, req *http.Request) {
	fl, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "no streaming", 500)
		return
	}
	id := req.URL.Query().Get("id")
	name := req.URL.Query().Get("name")
	if id == "" {
		http.Error(w, "id required", 400)
		return
	}
	if name == "" {
		name = "guest"
	}
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	c := &client{id: id, name: name, ch: make(chan []byte, 32)}
	room.mu.Lock()
	room.clients[id] = c
	room.recompute()
	room.broadcast()
	room.mu.Unlock()
	defer func() {
		room.mu.Lock()
		if room.clients[id] == c {
			delete(room.clients, id)
			room.recompute()
			room.broadcast()
		}
		room.mu.Unlock()
	}()
	tick := time.NewTicker(3 * time.Second)
	defer tick.Stop()
	for {
		select {
		case b := <-c.ch:
			fmt.Fprintf(w, "data: %s\n\n", b)
			fl.Flush()
		case <-tick.C:
			room.mu.Lock()
			b := room.snapshot()
			room.mu.Unlock()
			fmt.Fprintf(w, "data: %s\n\n", b)
			fl.Flush()
		case <-req.Context().Done():
			return
		}
	}
}

func handleCmd(w http.ResponseWriter, req *http.Request) {
	var m struct {
		ID     string  `json:"id"`
		Action string  `json:"action"`
		Pos    float64 `json:"pos"`
		Ready  bool    `json:"ready"`
		Note   string  `json:"note"`
		FileID int     `json:"fileId"`
		T      int64   `json:"t"` // client event time, in server clock (ms)
	}
	if json.NewDecoder(req.Body).Decode(&m) != nil {
		http.Error(w, "bad json", 400)
		return
	}
	if m.Action == "ended" {
		handleEnded(m.FileID)
		w.WriteHeader(204)
		return
	}
	at := time.Now()
	if m.T > 0 { // use when the viewer actually pressed the button, not when it reached us
		if ct := time.UnixMilli(m.T); ct.Before(at) && at.Sub(ct) < 3*time.Second {
			at = ct
		}
	}
	room.mu.Lock()
	defer room.mu.Unlock()
	switch m.Action {
	case "play":
		room.desired, room.pos, room.at = true, m.Pos, at
	case "pause":
		room.desired, room.pos, room.at = false, m.Pos, at
	case "seek":
		room.pos, room.at = m.Pos, at
	case "setauto":
		room.autoNext = m.Ready
	case "skipwait":
		for _, c := range room.clients {
			if !c.ready {
				c.forced = true
			}
		}
	case "ready":
		if c := room.clients[m.ID]; c != nil {
			c.ready, c.note = m.Ready, m.Note
			if m.Ready {
				c.forced = false
			}
		}
	}
	room.recompute()
	room.broadcast()
	w.WriteHeader(204)
}

// Addr is an address of this PC that others can reach, tagged by which network it belongs to.
type Addr struct {
	IP      string `json:"ip"`
	Kind    string `json:"kind"` // hamachi, tailscale, zerotier, lan
	If      string `json:"if"`
	Primary bool   `json:"primary"` // the address this PC uses to reach the internet: almost always the real Wi-Fi/Ethernet
}

func classify(name string, ip net.IP) string {
	ip = ip.To4()
	switch {
	case strings.Contains(name, "hamachi") || ip[0] == 25:
		return "hamachi"
	case strings.Contains(name, "tailscale") || (ip[0] == 100 && ip[1]&0xC0 == 64): // 100.64.0.0/10
		return "tailscale"
	case strings.Contains(name, "zerotier") || strings.HasPrefix(name, "zt"):
		return "zerotier"
	case ip[0] == 169 && ip[1] == 254:
		return ""
	}
	for _, v := range []string{"vethernet", "virtualbox", "vmware", "vbox", "wsl", "docker", "hyper-v", "bluetooth", "teredo", "isatap", "loopback", "npcap", "vpn client"} {
		if strings.Contains(name, v) {
			return "virtual" // WSL / VM / Docker adapters: phones on your Wi-Fi can't reach these
		}
	}
	return "lan"
}

// primaryIP is the local address Windows would use to reach the internet (no packet is actually sent).
func primaryIP() string {
	c, err := net.Dial("udp4", "8.8.8.8:80")
	if err != nil {
		return ""
	}
	defer c.Close()
	return c.LocalAddr().(*net.UDPAddr).IP.String()
}

// networkAddrs lists every IPv4 address friends/phones could use. The server listens on all
// adapters, so Hamachi, Tailscale, ZeroTier and Wi-Fi/LAN all work at the same time.
func networkAddrs() []Addr {
	out := []Addr{}
	ifs, _ := net.Interfaces()
	for _, ifc := range ifs {
		if ifc.Flags&net.FlagUp == 0 || ifc.Flags&net.FlagLoopback != 0 {
			continue
		}
		name := strings.ToLower(ifc.Name)
		addrs, _ := ifc.Addrs()
		for _, a := range addrs {
			ipn, ok := a.(*net.IPNet)
			if !ok || ipn.IP.To4() == nil {
				continue
			}
			ip := ipn.IP.To4()
			s := ip.String()
			kind := classify(name, ip)
			if kind == "" {
				continue
			}
			out = append(out, Addr{IP: s, Kind: kind, If: ifc.Name})
		}
	}
	prim := primaryIP()
	rank := map[string]int{"hamachi": 0, "tailscale": 1, "zerotier": 2, "lan": 3, "virtual": 4}
	for i := range out {
		out[i].Primary = out[i].IP == prim && out[i].Kind == "lan"
	}
	sort.SliceStable(out, func(i, j int) bool { // real Wi-Fi/Ethernet first among LAN addresses
		if out[i].Kind != out[j].Kind {
			return rank[out[i].Kind] < rank[out[j].Kind]
		}
		return out[i].Primary && !out[j].Primary
	})
	return out
}

// fixFirewall adds an inbound allow rule for this exe on every network profile (Private, Public, Domain), via a UAC prompt.
// Windows usually only gets one profile ticked in the first-run prompt, which is why VPN adapters work but the Wi-Fi doesn't.
func fixFirewall() error {
	if runtime.GOOS != "windows" {
		return fmt.Errorf("Windows only")
	}
	exe, err := os.Executable()
	if err != nil {
		return err
	}
	bat := filepath.Join(os.TempDir(), "watchparty-firewall.bat")
	script := "@echo off\r\n" +
		"netsh advfirewall firewall delete rule name=\"Watch Party\" >nul 2>&1\r\n" +
		"netsh advfirewall firewall add rule name=\"Watch Party\" dir=in action=allow program=\"" + exe + "\" enable=yes profile=any\r\n"
	if err := os.WriteFile(bat, []byte(script), 0o644); err != nil {
		return err
	}
	defer os.Remove(bat)
	ps := "Start-Process -FilePath '" + strings.ReplaceAll(bat, "'", "''") + "' -Verb RunAs -WindowStyle Hidden -Wait"
	out, err := exec.Command("powershell", "-NoProfile", "-Command", ps).CombinedOutput()
	if err != nil {
		return fmt.Errorf("%s", strings.TrimSpace(string(out)+" "+err.Error()))
	}
	return nil
}

func runtimeGOOS() string { return runtime.GOOS }

func pickSubtitle() (string, error) {
	ps := `Add-Type -AssemblyName System.Windows.Forms
$f = New-Object System.Windows.Forms.Form -Property @{TopMost=$true}
$d = New-Object System.Windows.Forms.OpenFileDialog
$d.Title = 'Pick a subtitle file'
$d.Filter = 'Subtitles|*.srt;*.vtt;*.ass;*.ssa|All files|*.*'
if ($d.ShowDialog($f) -eq 'OK') { [Console]::Out.Write($d.FileName) }`
	out, err := exec.Command("powershell", "-NoProfile", "-STA", "-WindowStyle", "Hidden", "-Command", ps).Output()
	return strings.TrimSpace(string(out)), err
}

func pickFiles() ([]string, error) {
	if runtime.GOOS != "windows" {
		return nil, fmt.Errorf("file dialog only on Windows; pass the file paths as arguments")
	}
	ps := `Add-Type -AssemblyName System.Windows.Forms
$f = New-Object System.Windows.Forms.Form -Property @{TopMost=$true}
$d = New-Object System.Windows.Forms.OpenFileDialog
$d.Title = 'Pick one or more videos (hold Ctrl or Shift to select several)'
$d.Multiselect = $true
$d.Filter = 'Video files|*.mp4;*.m4v;*.webm;*.mkv;*.mov;*.ogv;*.avi|All files|*.*'
if ($d.ShowDialog($f) -eq 'OK') { [Console]::Out.Write([string]::Join([char]10, $d.FileNames)) }`
	out, err := exec.Command("powershell", "-NoProfile", "-STA", "-WindowStyle", "Hidden", "-Command", ps).Output()
	var paths []string
	for _, l := range strings.Split(string(out), "\n") {
		if l = strings.TrimSpace(l); l != "" {
			paths = append(paths, l)
		}
	}
	return paths, err
}

func pickFolder() (string, error) {
	if runtime.GOOS != "windows" {
		return "", fmt.Errorf("folder dialog only on Windows; drag the folder onto the exe instead")
	}
	ps := `Add-Type -AssemblyName System.Windows.Forms
$f = New-Object System.Windows.Forms.Form -Property @{TopMost=$true}
$d = New-Object System.Windows.Forms.FolderBrowserDialog
$d.Description = 'Pick a folder: every video inside it, including subfolders, will be added'
$d.ShowNewFolderButton = $false
if ($d.ShowDialog($f) -eq 'OK') { [Console]::Out.Write($d.SelectedPath) }`
	out, err := exec.Command("powershell", "-NoProfile", "-STA", "-WindowStyle", "Hidden", "-Command", ps).Output()
	return strings.TrimSpace(string(out)), err
}

func openBrowser(u string) {
	var cmd *exec.Cmd
	switch runtime.GOOS {
	case "windows":
		cmd = exec.Command("rundll32", "url.dll,FileProtocolHandler", u)
	case "darwin":
		cmd = exec.Command("open", u)
	default:
		cmd = exec.Command("xdg-open", u)
	}
	_ = cmd.Start()
}

func main() {
	if len(os.Args) > 1 {
		addFiles(os.Args[1:]) // drag videos onto the exe
	}

	sub, _ := fs.Sub(webFS, "web")
	mux := http.NewServeMux()
	mux.Handle("/", http.FileServer(http.FS(sub)))

	mux.HandleFunc("/video", func(w http.ResponseWriter, req *http.Request) {
		room.mu.Lock()
		p, m := room.path, room.mime
		room.mu.Unlock()
		if p == "" {
			http.NotFound(w, req)
			return
		}
		f, err := os.Open(p)
		if err != nil {
			http.Error(w, err.Error(), 500)
			return
		}
		defer f.Close()
		st, _ := f.Stat()
		w.Header().Set("Content-Type", m)
		w.Header().Set("Cache-Control", "no-store")
		http.ServeContent(w, req, "", st.ModTime(), f) // handles Range requests
	})

	mux.HandleFunc("/api/status", func(w http.ResponseWriter, req *http.Request) {
		addrs := networkAddrs()
		room.mu.Lock()
		defer room.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{
			"local": isLocal(req), "name": room.name, "size": room.size,
			"mime": room.mime, "fileId": room.fileID, "addrs": addrs,
			"port":    req.Context().Value(http.LocalAddrContextKey).(net.Addr).(*net.TCPAddr).Port,
			"viewers": len(room.clients), "live": room.live != nil, "canSubs": hasSubs, "conv": convJSON(),
			"itemId": room.curItem, "items": len(room.playlist),
		})
	})

	mux.HandleFunc("/api/pick", func(w http.ResponseWriter, req *http.Request) {
		if !isLocal(req) {
			http.Error(w, "host only", 403)
			return
		}
		paths, err := pickFiles()
		if len(paths) > 0 {
			addFiles(paths)
			err = nil
		}
		if err != nil {
			http.Error(w, err.Error(), 500)
			return
		}
		w.WriteHeader(204)
	})

	mux.HandleFunc("/api/pick/folder", func(w http.ResponseWriter, req *http.Request) {
		if !isLocal(req) {
			http.Error(w, "host only", 403)
			return
		}
		dir, err := pickFolder()
		if dir != "" {
			addFiles([]string{dir})
			err = nil
		}
		if err != nil {
			http.Error(w, err.Error(), 500)
			return
		}
		w.WriteHeader(204)
	})
	mux.HandleFunc("/api/firewall", func(w http.ResponseWriter, req *http.Request) {
		if !isLocal(req) {
			http.Error(w, "host only", 403)
			return
		}
		if err := fixFirewall(); err != nil {
			http.Error(w, "Could not change the firewall: "+err.Error()+" (If you pressed No on the admin prompt, try again and press Yes.)", 500)
			return
		}
		w.WriteHeader(204)
	})
	mux.HandleFunc("/api/time", func(w http.ResponseWriter, req *http.Request) {
		w.Header().Set("Cache-Control", "no-store")
		fmt.Fprint(w, time.Now().UnixMilli())
	})
	hostOnly := func(f func(http.ResponseWriter, *http.Request)) http.HandlerFunc {
		return func(w http.ResponseWriter, req *http.Request) {
			if !isLocal(req) {
				http.Error(w, "host only", 403)
				return
			}
			f(w, req)
		}
	}
	mux.HandleFunc("/api/convert/cancel", hostOnly(func(w http.ResponseWriter, req *http.Request) {
		room.mu.Lock()
		if conv.Running && conv.cancel != nil {
			conv.cancel()
		}
		room.mu.Unlock()
		w.WriteHeader(204)
	}))
	mux.HandleFunc("/api/item/convert", hostOnly(handleItemConvert))
	mux.HandleFunc("/api/playlist", handlePlaylist)
	mux.HandleFunc("/api/playlist/remove", hostOnly(handleRemove))
	mux.HandleFunc("/api/play", handlePlay) // anyone connected can choose what plays
	mux.HandleFunc("/live/", handleLive)
	mux.HandleFunc("/api/subs", handleSubs)
	mux.HandleFunc("/api/subs/pick", handleSubPick)
	mux.HandleFunc("/sub/", handleSubFile)
	mux.HandleFunc("/events", handleEvents)
	mux.HandleFunc("/api/cmd", handleCmd)

	var ln net.Listener
	var err error
	port := basePort
	for ; port < basePort+20; port++ {
		if ln, err = net.Listen("tcp", fmt.Sprintf("0.0.0.0:%d", port)); err == nil {
			break
		}
	}
	if err != nil {
		log.Fatal("could not open a port: ", err)
	}

	fmt.Println("==============================================")
	fmt.Println("  Watch Party is running. Keep this window open.")
	fmt.Printf("  Local page:  http://localhost:%d\n", port)
	haveHama := false
	for _, a := range networkAddrs() {
		fmt.Printf("  Join via %-9s %s:%d\n", a.Kind+":", a.IP, port)
		haveHama = haveHama || a.Kind == "hamachi"
	}
	if !haveHama {
		fmt.Println("  (No Hamachi address found - is Hamachi connected?)")
	}
	fmt.Println("==============================================")

	go func() {
		time.Sleep(300 * time.Millisecond)
		openBrowser("http://localhost:" + fmt.Sprint(port) + "/?" + url.Values{}.Encode())
	}()
	log.Fatal(http.Serve(ln, mux))
}
