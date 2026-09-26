package disk

import (
	"context"
	"fmt"
	"time"

	"github.com/fsnotify/fsnotify"

	"phenix/util/plog"
)

// Watch calls onChange once images in dir stop changing for the debounce
// period after being created, written, renamed or removed (an upload writes
// many times), until ctx is done. Subdirectories are not watched, matching
// what GetImages lists.
func Watch(ctx context.Context, dir string, debounce time.Duration, onChange func()) error {
	watcher, err := fsnotify.NewWatcher()
	if err != nil {
		return fmt.Errorf("creating disk image watcher: %w", err)
	}

	if err := watcher.Add(dir); err != nil {
		_ = watcher.Close()

		return fmt.Errorf("watching %s: %w", dir, err)
	}

	go func() {
		defer func() { _ = watcher.Close() }()

		timer := time.NewTimer(debounce)
		timer.Stop()

		for {
			select {
			case <-ctx.Done():
				timer.Stop()

				return
			case event, ok := <-watcher.Events:
				if !ok {
					return
				}

				if knownImage(event.Name) {
					timer.Reset(debounce)
				}
			case err, ok := <-watcher.Errors:
				if !ok {
					return
				}

				plog.Warn(plog.TypeSystem, "watching disk images", "dir", dir, "err", err)
			case <-timer.C:
				onChange()
			}
		}
	}()

	return nil
}
