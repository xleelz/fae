package runtime

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"github.com/xleelz/fae/domain"
	"os"
	"path/filepath"
	"syscall"
	"time"
)

func monitorToolEvents(ctx context.Context, dir string, callback func(domain.ToolEvent)) func() {
	if callback == nil {
		return func() {}
	}
	stop, done := make(chan struct{}), make(chan struct{})
	go func() {
		defer close(done)
		offset := int64(0)
		discarding := false
		poll := func() {
			f, err := os.OpenFile(filepath.Join(dir, "tool-events.jsonl"), os.O_RDONLY|syscall.O_NOFOLLOW, 0)
			if err != nil {
				return
			}
			defer f.Close()
			_, _ = f.Seek(offset, 0)
			reader := bufio.NewReaderSize(f, 8192)
			for {
				line, err := reader.ReadSlice('\n')
				if errors.Is(err, bufio.ErrBufferFull) {
					offset += int64(len(line))
					discarding = true
					continue
				}
				if err != nil {
					break
				}
				offset += int64(len(line))
				if discarding {
					discarding = false
					continue
				}
				var e domain.ToolEvent
				if json.Unmarshal(line, &e) == nil && len(e.Tool) <= 128 && len(e.Paths) <= 32 {
					callback(e)
				}
			}
		}
		ticker := time.NewTicker(100 * time.Millisecond)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				poll()
				return
			case <-stop:
				poll()
				return
			case <-ticker.C:
				poll()
			}
		}
	}()
	return func() { close(stop); <-done }
}
