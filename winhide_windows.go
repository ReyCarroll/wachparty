package main

import (
	"os/exec"
	"syscall"
)

// hideWindow hides ffmpeg's console window and runs it at BELOW_NORMAL priority, so a heavy conversion on the host's
// own PC can't starve the browser's audio/video playback (crackly or slowed sound while the converter is busy).
func hideWindow(c *exec.Cmd) {
	const belowNormalPriorityClass = 0x00004000
	c.SysProcAttr = &syscall.SysProcAttr{HideWindow: true, CreationFlags: belowNormalPriorityClass}
}
