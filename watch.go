package main

// Live folder updates (Server-Sent Events).
//
//   GET /api/watch?root=N&path=/sub   -> text/event-stream
//     event: change   data: 1            the listing changed (client re-reads /api/list)
//     event: active   data: ["a.ts",…]   files being written right now (size or mtime changed
//                                        since the previous poll, or mtime within 2 s of now)
//
// Folders are watched by polling (name + size + mtime of every entry), not inotify:
// inotify does not see changes made by other hosts on SMB/NFS shares, polling does,
// and it needs no extra dependency. A folder is watched only while at least one
// client is subscribed to it; the last client leaving stops its watcher.
// The "active" list comes from the same poll, so it costs no extra file system work
// and no extra HTTP requests (nothing is logged).

import (
	"encoding/json"
	"fmt"
	"hash/fnv"
	"net/http"
	"os"
	"slices"
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
	watchRecent      = 2 * time.Second // mtime this close to now = being written
	watchMaxActive   = 500             // names sent in one "active" event
)

type watchSub struct {
	change chan struct{} // listing changed
	active chan struct{} // active file list changed
}

type dirWatch struct {
	subs   map[*watchSub]struct{}
	stop   chan struct{}
	active []string // guarded by watchMu
}

var (
	watchMu sync.Mutex
	watches = map[string]*dirWatch{}
)

func notify(ch chan struct{}) {
	select {
	case ch <- struct{}{}:
	default: // a notification is already pending
	}
}

// watchSubscribe registers a listener for folder dir; the returned func unsubscribes.
func watchSubscribe(dir string) (*watchSub, *dirWatch, func(), bool) {
	watchMu.Lock()
	defer watchMu.Unlock()
	w := watches[dir]
	if w == nil {
		if len(watches) >= watchMaxDirs {
			return nil, nil, nil, false
		}
		w = &dirWatch{subs: map[*watchSub]struct{}{}, stop: make(chan struct{})}
		watches[dir] = w
		go w.run(dir)
	}
	s := &watchSub{change: make(chan struct{}, 1), active: make(chan struct{}, 1)}
	if len(w.active) > 0 {
		notify(s.active) // a new client gets the current list at once
	}
	w.subs[s] = struct{}{}
	return s, w, func() {
		watchMu.Lock()
		defer watchMu.Unlock()
		delete(w.subs, s)
		if len(w.subs) == 0 && watches[dir] == w {
			delete(watches, dir) // nobody is looking at this folder any more
			close(w.stop)
		}
	}, true
}

func (w *dirWatch) activeList() []string {
	watchMu.Lock()
	defer watchMu.Unlock()
	return w.active
}

func (w *dirWatch) run(dir string) {
	snap := dirSnapshot(dir, nil)
	var last time.Time // last change notification
	pending := false   // modify-only change not sent yet
	w.setActive(snap.active)
	for {
		t := time.NewTimer(min(max(snap.cost*10, watchMinInterval), watchMaxInterval)) // big/slow folders are polled less often
		select {
		case <-w.stop:
			t.Stop()
			return
		case <-t.C:
		}
		n := dirSnapshot(dir, snap.files)
		if n.full != snap.full {
			pending = true
		}
		listChanged := n.names != snap.names
		snap = n
		w.setActive(n.active)
		if !pending || (!listChanged && time.Since(last) < watchModifyEvery) {
			continue
		}
		pending, last = false, time.Now()
		watchMu.Lock()
		for s := range w.subs {
			notify(s.change)
		}
		watchMu.Unlock()
	}
}

func (w *dirWatch) setActive(a []string) {
	watchMu.Lock()
	defer watchMu.Unlock()
	if slices.Equal(a, w.active) {
		return
	}
	w.active = a
	for s := range w.subs {
		notify(s.active)
	}
}

type fileStat struct {
	size  int64
	mtime int64 // UnixNano
}

type snapshot struct {
	names, full uint64              // hash of the name set / of names + size + mtime
	files       map[string]fileStat // regular files (for change detection on the next poll)
	active      []string            // sorted, files being written
	cost        time.Duration
}

// dirSnapshot reads dir once (same entries as /api/list shows). prev is the files map of the
// previous poll: a file whose size or mtime differs from it is "active".
func dirSnapshot(dir string, prev map[string]fileStat) snapshot {
	start := time.Now()
	sn := snapshot{files: map[string]fileStat{}}
	f, err := os.Open(dir)
	if err != nil {
		h := fnv.New64a()
		fmt.Fprint(h, "error:", err)
		sn.names, sn.full, sn.cost = h.Sum64(), h.Sum64(), time.Since(start)
		return sn
	}
	ents, err := f.ReadDir(-1)
	f.Close()
	var bad uint64
	if err != nil && len(ents) == 0 {
		h := fnv.New64a()
		fmt.Fprint(h, "error:", err)
		bad = h.Sum64()
	}
	now := time.Now().UnixNano()
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
			st := fileStat{fi.Size(), fi.ModTime().UnixNano()}
			fmt.Fprint(e, "\x00", st.size, st.mtime, fi.IsDir())
			if !fi.IsDir() {
				sn.files[name] = st
				p, seen := prev[name]
				age := now - st.mtime
				if (seen && p != st) || (age >= 0 && age <= int64(watchRecent)) {
					if len(sn.active) < watchMaxActive {
						sn.active = append(sn.active, name)
					}
				}
			}
		}
		v = e.Sum64()
		fSum += v
		fXor ^= v * 0x9E3779B97F4A7C15
	}
	slices.Sort(sn.active)
	sn.names, sn.full, sn.cost = bad^nSum^(nXor<<1), bad^fSum^(fXor<<1), time.Since(start)
	return sn
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
	sub, dw, unsub, ok := watchSubscribe(full)
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
		case <-sub.change:
			if !send("event: change\ndata: 1\n\n") {
				return
			}
		case <-sub.active:
			a := dw.activeList()
			if a == nil {
				a = []string{}
			}
			b, _ := json.Marshal(a) // one line: JSON never contains a raw newline
			if !send("event: active\ndata: " + string(b) + "\n\n") {
				return
			}
		case <-ka.C:
			if !send(": ka\n\n") {
				return
			}
		}
	}
}
