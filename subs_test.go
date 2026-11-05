//go:build !lite

package main

import "testing"

func TestCP1252(t *testing.T) {
	if n := len(cp1252hi); n != 32 {
		t.Fatalf("table has %d runes", n)
	}
	if got := cp1252ToUTF8([]byte{0x93, 'h', 'i', 0x94, 0xE9}); got != "“hi”é" {
		t.Fatalf("got %q", got)
	}
}
