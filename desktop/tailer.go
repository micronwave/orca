package main

import (
	"context"
	"os"
	"path/filepath"
	"time"

	"github.com/wailsapp/wails/v2/pkg/runtime"
)

// tailEventLog polls the events.log file for new content and emits a
// "state:refresh" Wails event whenever the file grows. Runs until ctx is cancelled.
func (a *App) tailEventLog(ctx context.Context) {
	var lastSize int64

	ticker := time.NewTicker(2 * time.Second)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			// Re-read dir() each tick so SetOrcaDir changes take effect.
			logPath := filepath.Join(a.dir(), "events.log")
			info, err := os.Stat(logPath)
			if err != nil {
				continue // file may not exist yet
			}
			if info.Size() != lastSize {
				lastSize = info.Size()
				if a.ctx != nil {
					runtime.EventsEmit(a.ctx, "state:refresh")
				}
			}
		}
	}
}
