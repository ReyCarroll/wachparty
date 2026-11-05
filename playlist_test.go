package main

import (
	"sort"
	"testing"
)

func TestNaturalLess(t *testing.T) {
	in := []string{"Show S01E10.mkv", "Show S01E2.mkv", "show s01e01.mkv", "Show S01E02b.mkv", "Show S01E100.mkv"}
	sort.Slice(in, func(i, j int) bool { return naturalLess(in[i], in[j]) })
	want := []string{"show s01e01.mkv", "Show S01E2.mkv", "Show S01E02b.mkv", "Show S01E10.mkv", "Show S01E100.mkv"}
	for i := range want {
		if in[i] != want[i] {
			t.Fatalf("got %v want %v", in, want)
		}
	}
}
