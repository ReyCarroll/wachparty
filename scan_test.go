package main

import (
	"os"
	"path/filepath"
	"testing"
)

func TestScanPaths(t *testing.T) {
	root := filepath.Join(t.TempDir(), "My Show")
	files := []string{"Season 10/E1.mp4", "Season 2/E10.mkv", "Season 2/E2.mkv", "Season 2/notes.txt", "Season 2/E2.watchparty.mp4",
		"Extras/Behind/clip.webm", ".hidden/x.mp4", "Season 1/Part 1/E1.mp4"}
	for _, f := range files {
		p := filepath.Join(root, filepath.FromSlash(f))
		os.MkdirAll(filepath.Dir(p), 0o755)
		os.WriteFile(p, []byte("x"), 0o644)
	}
	loose := filepath.Join(t.TempDir(), "loose.mp4")
	os.WriteFile(loose, []byte("x"), 0o644)

	got := scanPaths([]string{root, loose})
	want := []entry{
		{loose, ""},
		{"", "My Show/Extras/Behind"}, // clip.webm
		{"", "My Show/Season 1/Part 1"},
		{"", "My Show/Season 2"}, // E2
		{"", "My Show/Season 2"}, // E10
		{"", "My Show/Season 10"},
	}
	if len(got) != len(want) {
		t.Fatalf("got %d entries: %+v", len(got), got)
	}
	for i := range want {
		if got[i].group != want[i].group {
			t.Errorf("%d: group %q want %q (%s)", i, got[i].group, want[i].group, got[i].path)
		}
	}
	if filepath.Base(got[3].path) != "E2.mkv" || filepath.Base(got[4].path) != "E10.mkv" {
		t.Errorf("episode order wrong: %s, %s", got[3].path, got[4].path)
	}
}
