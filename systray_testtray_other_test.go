//go:build !((linux || freebsd || openbsd || netbsd) && !android)

package systray

import "sync"

// startTestTray runs the tray event loop for tests and returns a function that stops it.
func startTestTray() (stop func()) {
	var wait sync.WaitGroup
	wait.Add(1)
	go Run(func() {
		SetTitle("Test Tray")
		SetTooltip("Test Tray Tooltip")
		wait.Done()
	}, nil)
	wait.Wait()
	return Quit
}
