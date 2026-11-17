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
	// when only size/mtime of existing entries change (e.g. a file being written / recorded),
	// clients are notified at most this often; added, removed or renamed entries notify at once
	watchModifyEvery = 10 * time.Second
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
	names, sig, cost := dirSignature(dir)
	var last time.Time // last notification
	pending := false   // modify-only change not sent yet
	for {
		t := time.NewTimer(min(max(cost*10, watchMinInterval), watchMaxInterval)) // big/slow folders are polled less often
		select {
		case <-w.stop:
			t.Stop()
			return
		case <-t.C:
		}
		n, s, c := dirSignature(dir)
		cost = c
		if s != sig {
			pending = true
		}
		listChanged := n != names
		names, sig = n, s
		if !pending || (!listChanged && time.Since(last) < watchModifyEvery) {
			continue
		}
		pending, last = false, time.Now()
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

// dirSignature hashes the visible listing of dir (same entries as /api/list shows):
// names = the set of entry names, full = names + size + mtime.
func dirSignature(dir string) (names, full uint64, cost time.Duration) {
	start := time.Now()
	f, err := os.Open(dir)
	if err != nil {
		h := fnv.New64a()
		fmt.Fprint(h, "error:", err)
		return h.Sum64(), h.Sum64(), time.Since(start)
	}
	ents, err := f.ReadDir(-1)
	f.Close()
	var bad uint64
	if err != nil && len(ents) == 0 {
		h := fnv.New64a()
		fmt.Fprint(h, "error:", err)
		bad = h.Sum64()
	}
	// ReadDir order is not guaranteed to be stable, so combine per-entry hashes order-independently
	var nSum, nXor, fSum, fXor uint64
	for _, d := range ents {
		name := d.Name()
		if strings.HasSuffix(name, partSuffix) {
			continue // uploads in progress
		}
		e := fnv.New64a()
		e.Write([]byte(name))
		v := e.Sum64()
		nSum += v
		nXor ^= v * 0x9E3779B97F4A7C15
		if fi, err := d.Info(); err == nil {
			fmt.Fprint(e, "\x00", fi.Size(), fi.ModTime().UnixNano(), fi.IsDir())
		}
		v = e.Sum64()
		fSum += v
		fXor ^= v * 0x9E3779B97F4A7C15
	}
	return bad ^ nSum ^ (nXor << 1), bad ^ fSum ^ (fXor << 1), time.Since(start)
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
