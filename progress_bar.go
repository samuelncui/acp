package acp

import (
	"fmt"
	"os"
	"time"

	"github.com/schollz/progressbar/v3"
)

func NewProgressBar() EventHandler {
	// progressBar := progressbar.DefaultBytes(1, "[0/0] indexing...")
	bar := progressbar.NewOptions64(
		1,
		progressbar.OptionSetDescription("[0/0] indexing..."),
		progressbar.OptionSetWriter(os.Stderr),
		progressbar.OptionShowBytes(true),
		progressbar.OptionSetWidth(10),
		progressbar.OptionThrottle(65*time.Millisecond),
		progressbar.OptionShowCount(),
		progressbar.OptionOnCompletion(func() {}),
		progressbar.OptionSpinnerType(14),
		progressbar.OptionFullWidth(),
		progressbar.OptionSetRenderBlankState(true),
	)

	var totalFiles int64
	return func(ev Event) {
		switch e := ev.(type) {
		case *EventUpdateCount:
			totalFiles = e.Files
			bar.Describe(fmt.Sprintf("[0/%d] indexing...", totalFiles))
			bar.ChangeMax64(e.Bytes)
			return
		case *EventUpdateProgress:
			bar.Set64(e.Bytes)

			if !e.Finished {
				bar.Describe(fmt.Sprintf("[%d/%d] copying...", e.Files, totalFiles))
				return
			}

			bar.Describe(fmt.Sprintf("[%d/%d] finishing...", e.Files, totalFiles))
			return
		default:
			return
		}
	}
}
