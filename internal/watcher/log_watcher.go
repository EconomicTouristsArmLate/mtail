// Copyright 2015 Google Inc. All Rights Reserved.
// This file is available under the Apache license.

package watcher

import (
	"context"
	"os"
	"path"
	"path/filepath"
	"sync"
	"time"

	"github.com/golang/glog"
	"github.com/pkg/errors"
)

type watch struct {
	ps []Processor
	fi os.FileInfo
}

// hasChanged indicates that a FileInfo has changed.
// http://apenwarr.ca/log/20181113 suggests that comparing mtime is
// insufficient for sub-second resolution on many platforms, and we can do
// better by comparing a few fields in the FileInfo.  This set of tests is less
// than the ones suggested in the blog post, but seem sufficient for making
// tests (notably, sub-millisecond accuracy) pass quickly.  mtime-only diff has
// caused race conditions in test and likely caused strange behaviour in
// production environments.
func hasChanged(a, b os.FileInfo) bool {
	if a == nil || b == nil {
		glog.V(2).Info("One or both FileInfos are nil")
		return true
	}
	if a.ModTime() != b.ModTime() {
		glog.V(2).Info("modtimes differ")
		return true
	}
	if a.Size() != b.Size() {
		glog.V(2).Info("sizes differ")
		return true
	}
	if a.Mode() != b.Mode() {
		glog.V(2).Info("modes differ")
		return true
	}
	return false
}

// LogWatcher implements a Watcher for watching real filesystems.
type LogWatcher struct {
	watchedMu sync.RWMutex // protects `watched'
	watched   map[string]*watch

	pollMu sync.Mutex // protects `Poll()`
}

// NewLogWatcher returns a new LogWatcher, or returns an error.
func NewLogWatcher(ctx context.Context, pollInterval time.Duration) (*LogWatcher, error) {
	w := &LogWatcher{
		watched: make(map[string]*watch),
	}
	if pollInterval > 0 {
		ticker := time.NewTicker(pollInterval)
		go func() {
			defer ticker.Stop()
			glog.V(2).Infof("starting watch ticker with %s interval", pollInterval)
			for {
				select {
				case <-ctx.Done():
					return
				case <-ticker.C:
					w.Poll()
				}
			}
		}()
	}
	return w, nil
}

func (w *LogWatcher) sendEvent(e Event) {
	w.watchedMu.RLock()
	watch, ok := w.watched[e.Pathname]
	w.watchedMu.RUnlock()
	if !ok {
		d := filepath.Dir(e.Pathname)
		w.watchedMu.RLock()
		watch, ok = w.watched[d]
		w.watchedMu.RUnlock()
		if !ok {
			glog.V(2).Infof("No watch for path %q", e.Pathname)
			return
		}
	}
	w.sendWatchedEvent(watch, e)
}

// Send an event to a watch; all locks assumed to be held.
func (w *LogWatcher) sendWatchedEvent(watch *watch, e Event) {
	for _, p := range watch.ps {
		p.ProcessFileEvent(context.TODO(), e)
	}
}

// Poll all watched objects for updates, dispatching events if required.
func (w *LogWatcher) Poll() {
	w.pollMu.Lock()
	defer w.pollMu.Unlock()
	glog.V(2).Info("Polling watched files.")
	w.watchedMu.RLock()
	for n, watch := range w.watched {
		w.watchedMu.RUnlock()
		w.pollWatchedPath(n, watch)
		w.watchedMu.RLock()
	}
	w.watchedMu.RUnlock()
}

// pollWatchedPathLocked polls an already-watched path for updates.
func (w *LogWatcher) pollWatchedPath(pathname string, watched *watch) {
	glog.V(2).Infof("Stat %q", pathname)
	fi, err := os.Stat(pathname)
	if err != nil {
		if os.IsNotExist(err) {
			glog.V(2).Infof("sending delete for %s", pathname)
			w.sendWatchedEvent(watched, Event{Delete, pathname})
			// Need to remove the watch for any subsequent create to be sent.
			w.watchedMu.Lock()
			delete(w.watched, pathname)
			w.watchedMu.Unlock()
		} else {
			glog.V(1).Info(err)
		}
		return
	}

	if fi.IsDir() {
		w.pollDirectory(watched, pathname)
	} else if hasChanged(fi, watched.fi) {
		glog.V(2).Infof("sending update for %s", pathname)
		w.sendWatchedEvent(watched, Event{Update, pathname})
	}

	w.watchedMu.Lock()
	if _, ok := w.watched[pathname]; ok {
		w.watched[pathname].fi = fi
	}
	w.watchedMu.Unlock()
}

// pollDirectory walks the directory tree for a parent watch, and notifies of any new files.
func (w *LogWatcher) pollDirectory(parentWatch *watch, pathname string) {
	matches, err := filepath.Glob(path.Join(pathname, "*"))
	if err != nil {
		glog.V(1).Info(err)
		return
	}
	for _, match := range matches {
		w.watchedMu.RLock()
		_, ok := w.watched[match]
		w.watchedMu.RUnlock()
		if !ok {
			// The object has no watch object so it must be new, but we can't
			// decide to watch it yet -- wait for the Tailer to match pattern
			// and instruct us to Observe it directly.  Technically not
			// everything is created here, it's literally everything in a path
			// that we aren't watching, so we make a lot of stats below, but we
			// need to find which ones are directories so we can traverse them.
			// TODO(jaq): teach log watcher about the TailPatterns from tailer.
			glog.V(2).Infof("sending create for %s", match)
			w.sendWatchedEvent(parentWatch, Event{Create, match})
		}
		fi, err := os.Stat(match)
		if err != nil {
			glog.V(1).Info(err)
			continue
		}
		if fi.IsDir() {
			w.pollDirectory(parentWatch, match)
		}
	}
}

// Observe adds a path to the list of watched items.
// If this path has a new event, then the processor being registered will be sent the event.
func (w *LogWatcher) Observe(path string, processor Processor) error {
	absPath, err := w.addWatch(path)
	if err != nil {
		return err
	}
	w.watchedMu.Lock()
	defer w.watchedMu.Unlock()
	watched, ok := w.watched[absPath]
	if !ok {
		fi, err := os.Stat(absPath)
		if err != nil {
			glog.V(1).Info(err)
		}
		w.watched[absPath] = &watch{ps: []Processor{processor}, fi: fi}
		glog.Infof("No abspath in watched list, added new one for %s", absPath)
		return nil
	}
	for _, p := range watched.ps {
		if p == processor {
			glog.Infof("Found this processor in watched list")
			return nil
		}
	}
	watched.ps = append(watched.ps, processor)
	glog.Infof("appended this processor")
	return nil
}

func (w *LogWatcher) addWatch(path string) (string, error) {
	absPath, err := filepath.Abs(path)
	if err != nil {
		return "", errors.Wrapf(err, "Failed to lookup absolutepath of %q", path)
	}
	glog.V(2).Infof("Adding a watch on resolved path %q", absPath)
	_, err = os.Stat(absPath)
	if err != nil {
		glog.V(2).Info(err)
		return absPath, err
	}
	return absPath, nil
}

// IsWatching indicates if the path is being watched. It includes both
// filenames and directories.
func (w *LogWatcher) IsWatching(path string) bool {
	absPath, err := filepath.Abs(path)
	if err != nil {
		glog.V(2).Infof("Couldn't resolve path %q: %s", absPath, err)
		return false
	}
	glog.V(2).Infof("Resolved path for lookup %q", absPath)
	w.watchedMu.RLock()
	_, ok := w.watched[absPath]
	w.watchedMu.RUnlock()
	return ok
}

func (w *LogWatcher) Unobserve(path string, processor Processor) error {
	w.watchedMu.Lock()
	defer w.watchedMu.Unlock()
	_, ok := w.watched[path]
	if !ok {
		return nil
	}

	for i, p := range w.watched[path].ps {
		if p == processor {
			w.watched[path].ps = append(w.watched[path].ps[0:i], w.watched[path].ps[i+1:]...)
			break
		}
	}
	if len(w.watched[path].ps) == 0 {
		delete(w.watched, path)
	}
	return nil
}
