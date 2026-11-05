//go:build lite

package main

import (
	"fmt"
	"net/http"
)

// Lite build: no ffmpeg, no conversion. Files are shared exactly as chosen.

const hasSubs = false

type Live struct{}

func (l *Live) stop() {}

type Conv struct {
	Running bool
	cancel  func()
}

func (c *Conv) itemID() int { return 0 }

var conv = &Conv{}

func probeItem(it *Item)                                     {}
func playConverted(it *Item) (bool, error)                   { return false, nil }
func startConvertItem(id int) error                          { return fmt.Errorf("not available in the lite build") }
func handleLive(w http.ResponseWriter, req *http.Request)    { http.NotFound(w, req) }
func afterFile(path string, id int)                          {}
func handleSubs(w http.ResponseWriter, req *http.Request)    { w.Write([]byte("[]")) }
func handleSubFile(w http.ResponseWriter, req *http.Request) { http.NotFound(w, req) }
func handleSubPick(w http.ResponseWriter, req *http.Request) { http.NotFound(w, req) }
func convJSON() any                                          { return map[string]any{} }
