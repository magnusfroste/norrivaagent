// Package watch turns the linked folder into a drop box: put a file in, and
// the agent is told about it.
//
// No inotify, no dependency — a poll every couple of seconds is plenty for a
// folder a person drops files into by hand, and it behaves the same on macOS,
// Linux and a network share. A file counts as arrived only once it has stopped
// changing between two polls, so a copy still in progress is never read half
// way through.
package watch

import (
	"context"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"time"
)

type stamp struct {
	size    int64
	modTime time.Time
}

// Run polls dir until ctx is done. For every file that appears (or changes)
// after Run started and then holds still for one full interval, onFile is
// called with its path relative to dir. Files present at the start are the
// baseline and are not reported; dotfiles and dot-folders are ignored.
func Run(ctx context.Context, dir string, every time.Duration, onFile func(rel string)) error {
	known, err := snapshot(dir)
	if err != nil {
		return err
	}
	pending := map[string]stamp{} // seen changed, waiting to hold still
	tick := time.NewTicker(every)
	defer tick.Stop()
	for {
		select {
		case <-ctx.Done():
			return nil
		case <-tick.C:
		}
		now, err := snapshot(dir)
		if err != nil {
			continue // the folder may be briefly unavailable; try again
		}
		for rel, st := range now {
			if old, ok := known[rel]; ok && old == st {
				continue // unchanged since we last settled it
			}
			if p, ok := pending[rel]; ok && p == st {
				// Held still for a whole interval: it has arrived.
				known[rel] = st
				delete(pending, rel)
				onFile(rel)
				continue
			}
			pending[rel] = st
		}
		for rel := range known {
			if _, ok := now[rel]; !ok {
				delete(known, rel) // removed; if it comes back it is new again
			}
		}
	}
}

func snapshot(dir string) (map[string]stamp, error) {
	out := map[string]stamp{}
	err := filepath.WalkDir(dir, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return nil // unreadable entry: skip it, keep walking
		}
		if path != dir && strings.HasPrefix(d.Name(), ".") {
			if d.IsDir() {
				return filepath.SkipDir
			}
			return nil
		}
		if d.IsDir() {
			return nil
		}
		info, err := d.Info()
		if err != nil {
			return nil
		}
		rel, _ := filepath.Rel(dir, path)
		out[rel] = stamp{size: info.Size(), modTime: info.ModTime()}
		return nil
	})
	if err != nil {
		return nil, err
	}
	if _, err := os.Stat(dir); err != nil {
		return nil, err
	}
	return out, nil
}
