package main

// Live folder updates (Server-Sent Events).
//
//   GET /api/watch?root=N&path=/sub   -> text/event-stream, "event: change" when the folder changes
//
// Folders are watched by polling (name + size + mtime of every entry), not inotify:
// inotify does not see changes made by other hosts on SMB/NFS shares, polling does,
// and it needs no extra dependency. A folder is watched only while at least one
// client is subscribed to it; the last client leaving stops its watcher.

import (
	"fmt"
	"hash/fnv"
	"net/http"
	"os"
	"strings"
	"sync"
	"time"
)

const (
	watchMinInterval = 2 * time.Second
	watchMaxInterval = 30 * time.Second
	watchMaxDirs     = 256 // distinct folders watched at the same time
	watchKeepAlive   = 25 * time.Second
)

type dirWatch struct {
	subs map[chan struct{}]struct{}
	stop chan struct{}
}

var (
	watchMu sync.Mutex
	watches = map[string]*dirWatch{}
)

// watchSubscribe registers a listener for folder dir; the returned func unsubscribes.
func watchSubscribe(dir string) (chan struct{}, func(), bool) {
	watchMu.Lock()
	defer watchMu.Unlock()
	w := watches[dir]
	if w == nil {
		if len(watches) >= watchMaxDirs {
			return nil, nil, false
		}
		w = &dirWatch{subs: map[chan struct{}]struct{}{}, stop: make(chan struct{})}
		watches[dir] = w
		go w.run(dir)
	}
	ch := make(chan struct{}, 1)
	w.subs[ch] = struct{}{}
	return ch, func() {
		watchMu.Lock()
		defer watchMu.Unlock()
		delete(w.subs, ch)
		if len(w.subs) == 0 && watches[dir] == w {
			delete(watches, dir) // nobody is looking at this folder any more
			close(w.stop)
		}
	}, true
}

func (w *dirWatch) run(dir string) {
	sig, cost := dirSignature(dir)
	for {
		t := time.NewTimer(min(max(cost*10, watchMinInterval), watchMaxInterval)) // big/slow folders are polled less often
		select {
		case <-w.stop:
			t.Stop()
			return
		case <-t.C:
		}
		var s uint64
		s, cost = dirSignature(dir)
		if s == sig {
			continue
		}
		sig = s
		watchMu.Lock()
		for ch := range w.subs {
			select {
			case ch <- struct{}{}:
			default: // a notification is already pending
			}
		}
		watchMu.Unlock()
	}
}

// dirSignature hashes the visible listing of dir (same entries as /api/list shows).
func dirSignature(dir string) (uint64, time.Duration) {
	start := time.Now()
	h := fnv.New64a()
	f, err := os.Open(dir)
	if err != nil {
		fmt.Fprint(h, "error:", err)
		return h.Sum64(), time.Since(start)
	}
	ents, err := f.ReadDir(-1)
	f.Close()
	if err != nil && len(ents) == 0 {
		fmt.Fprint(h, "error:", err)
	}
	// ReadDir order is not guaranteed to be stable, so combine per-entry hashes order-independently
	var sum, xor uint64
	for _, d := range ents {
		name := d.Name()
		if strings.HasSuffix(name, partSuffix) {
			continue // uploads in progress
		}
		e := fnv.New64a()
		e.Write([]byte(name))
		if fi, err := d.Info(); err == nil {
			fmt.Fprint(e, "\x00", fi.Size(), fi.ModTime().UnixNano(), fi.IsDir())
		}
		v := e.Sum64()
		sum += v
		xor ^= v * 0x9E3779B97F4A7C15
	}
	fmt.Fprint(h, len(ents), sum, xor)
	return h.Sum64(), time.Since(start)
}

func handleWatch(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	full, _, _, err := resolve(q.Get("root"), q.Get("path"))
	if err != nil {
		pathErr(w, err)
		return
	}
	if fi, err := os.Stat(full); err != nil {
		pathErr(w, err)
		return
	} else if !fi.IsDir() {
		jsonErr(w, http.StatusBadRequest, "not a folder")
		return
	}
	ch, unsub, ok := watchSubscribe(full)
	if !ok {
		jsonErr(w, http.StatusServiceUnavailable, "too many watched folders")
		return
	}
	defer unsub()

	w.Header().Set("Content-Type", "text/event-stream; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("X-Accel-Buffering", "no")
	rc := http.NewResponseController(w)
	send := func(s string) bool {
		if _, err := fmt.Fprint(w, s); err != nil {
			return false
		}
		return rc.Flush() == nil
	}
	if !send("retry: 5000\n: watching\n\n") {
		return
	}
	ka := time.NewTicker(watchKeepAlive) // keeps proxies / NAT from dropping an idle connection
	defer ka.Stop()
	for {
		select {
		case <-r.Context().Done():
			return
		case <-ch:
			if !send("event: change\ndata: 1\n\n") {
				return
			}
		case <-ka.C:
			if !send(": ka\n\n") {
				return
			}
		}
	}
}
