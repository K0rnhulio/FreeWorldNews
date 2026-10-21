package main

import (
	"os/exec"
	"strconv"
	"syscall"
)

func prepareBrowserProcess(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{HideWindow: true}
}

func killBrowserProcess(cmd *exec.Cmd) {
	// Terminate this worker's descendants as well if graceful cancellation fails.
	kill := exec.Command("taskkill.exe", "/PID", strconv.Itoa(cmd.Process.Pid), "/T", "/F")
	prepareBrowserProcess(kill)
	if kill.Run() != nil {
		cmd.Process.Kill()
	}
}
