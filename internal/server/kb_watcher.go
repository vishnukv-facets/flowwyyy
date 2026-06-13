package server

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/fsnotify/fsnotify"
)

// kbWatcher pushes a live "kb" ui_change whenever a KB markdown file changes on
// disk, so the Knowledge screen updates over SSE instead of polling. KB files
// are plain files written by several actors — the session KB distiller's agent
// (a separate process), the close-out sweep (another process), the dreamer's
// prune (this process), and the UI Edit (this process) — so there is no single
// server code path to hook publishUIChange into. Watching the directory catches
// all of them in one place, regardless of which process wrote. Event-driven
// (fsnotify), so it costs nothing when the KB is idle.
type kbWatcher struct {
	srv *Server

	mu   sync.Mutex
	w    *fsnotify.Watcher
	done chan struct{}
}

func newKBWatcher(srv *Server) *kbWatcher { return &kbWatcher{srv: srv} }

func (k *kbWatcher) start() {
	if k == nil || k.srv == nil {
		return
	}
	root := strings.TrimSpace(k.srv.cfg.FlowRoot)
	if root == "" {
		return
	}
	dir := filepath.Join(root, "kb")
	w, err := fsnotify.NewWatcher()
	if err != nil {
		fmt.Fprintf(os.Stderr, "kb watcher: new: %v\n", err)
		return
	}
	if err := w.Add(dir); err != nil {
		// The kb dir may not exist yet (pre-init); skip rather than fail. Live KB
		// updates simply fall back to react-query's on-focus refetch until then.
		fmt.Fprintf(os.Stderr, "kb watcher: watch %s: %v\n", dir, err)
		_ = w.Close()
		return
	}
	k.mu.Lock()
	k.w = w
	k.done = make(chan struct{})
	k.mu.Unlock()
	go k.loop(w)
}

func (k *kbWatcher) stop() {
	k.mu.Lock()
	w := k.w
	done := k.done
	k.w = nil
	k.done = nil
	k.mu.Unlock()
	if w != nil {
		_ = w.Close() // closes Events/Errors channels → loop returns
	}
	if done != nil {
		<-done
	}
}

func (k *kbWatcher) loop(w *fsnotify.Watcher) {
	defer close(k.done)
	// Debounce: a single sweep can rewrite several files in a burst; coalesce
	// into one invalidation. The SSE handler also dedups by fingerprint, so this
	// is just politeness.
	const debounce = 300 * time.Millisecond
	var timer *time.Timer
	defer func() {
		if timer != nil {
			timer.Stop()
		}
	}()
	for {
		select {
		case ev, ok := <-w.Events:
			if !ok {
				return
			}
			if ev.Op&(fsnotify.Write|fsnotify.Create|fsnotify.Rename|fsnotify.Remove) == 0 {
				continue
			}
			if !strings.HasSuffix(ev.Name, ".md") {
				continue
			}
			if timer == nil {
				timer = time.AfterFunc(debounce, func() { k.srv.publishUIChange("kb") })
			} else {
				timer.Reset(debounce)
			}
		case _, ok := <-w.Errors:
			if !ok {
				return
			}
		}
	}
}
