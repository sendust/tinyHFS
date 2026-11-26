package main

// Recursive file name search under a folder.
//
//   q = space separated terms, all must match (AND), case-insensitive.
//   A term containing * ? or [ is a glob matched against the whole name (e.g. *.mp4),
//   otherwise it is a substring match.
//
// Folders are read in parallel (searchWorkers at a time): on network shares (SMB/NFS)
// every directory read is a round trip, so a sequential walk is very slow.
//
// stream=1 returns NDJSON lines while searching:
//   {"t":"p","scanned":N,"dirs":N,"found":N,"ms":N,"dir":"sub/folder"}   progress
//   {"t":"r","entries":[...]}                                             new results
//   {"t":"end","scanned":N,"dirs":N,"found":N,"ms":N,"truncated":b,"timedOut":b}
//   {"t":"err","error":"..."}
// without stream=1 a single JSON object is returned when the search is finished.

import (
	"context"
	"encoding/json"
	"io/fs"
	"net/http"
	"os"
	"path"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

const (
	searchDefaultLimit = 1000
	searchMaxLimit     = 5000
	searchTimeLimit    = 2 * time.Minute
	searchWorkers      = 16
)

type nameMatcher []string

func newMatcher(q string) nameMatcher { return strings.Fields(strings.ToLower(q)) }

func (m nameMatcher) match(name string) bool {
	n := strings.ToLower(name)
	for _, t := range m {
		if strings.ContainsAny(t, "*?[") {
			if ok, err := path.Match(t, n); err != nil || !ok {
				return false
			}
		} else if !strings.Contains(n, t) {
			return false
		}
	}
	return true
}

type searcher struct {
	base     string
	m        nameMatcher
	limit    int
	ctx      context.Context
	deadline time.Time

	sem               chan struct{}
	wg                sync.WaitGroup
	scanned, dirs     atomic.Int64
	stop              atomic.Bool
	truncated, timedO atomic.Bool
	cur               atomic.Value // string: folder being read (relative)

	mu      sync.Mutex
	results []entry
}

func (s *searcher) stopped() bool {
	if s.stop.Load() {
		return true
	}
	if s.ctx.Err() != nil {
		s.stop.Store(true)
		return true
	}
	if time.Now().After(s.deadline) {
		s.timedO.Store(true)
		s.stop.Store(true)
		return true
	}
	return false
}

func (s *searcher) visit(dir string) {
	defer s.wg.Done()
	if s.stopped() {
		return
	}
	s.sem <- struct{}{}
	if s.stopped() {
		<-s.sem
		return
	}
	rel, _ := filepath.Rel(s.base, dir)
	s.cur.Store(filepath.ToSlash(rel))
	var ents []fs.DirEntry
	f, err := os.Open(dir)
	if err == nil {
		ents, err = f.ReadDir(-1) // unsorted: faster than os.ReadDir for big folders
		f.Close()
	}
	<-s.sem
	if err != nil && len(ents) == 0 {
		return // unreadable folder: skip
	}
	s.dirs.Add(1)
	for _, d := range ents {
		if s.stop.Load() {
			return
		}
		s.scanned.Add(1)
		name := d.Name()
		p := filepath.Join(dir, name)
		if d.IsDir() { // symlinked folders are not followed (no loops)
			s.wg.Add(1)
			go s.visit(p)
		}
		if strings.HasSuffix(name, partSuffix) || !s.m.match(name) {
			continue
		}
		var info fs.FileInfo
		if d.Type()&fs.ModeSymlink != 0 {
			info, err = os.Stat(p)
		} else {
			info, err = d.Info()
		}
		if err != nil {
			continue
		}
		r, _ := filepath.Rel(s.base, p)
		e := entry{Name: filepath.ToSlash(r), Dir: info.IsDir(), MTime: info.ModTime().Unix()}
		if !e.Dir {
			e.Size = info.Size()
		}
		s.mu.Lock()
		if len(s.results) < s.limit {
			s.results = append(s.results, e)
		}
		if len(s.results) >= s.limit {
			s.truncated.Store(true)
			s.stop.Store(true)
		}
		s.mu.Unlock()
	}
}

func handleSearch(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	full, _, clean, err := resolve(q.Get("root"), q.Get("path"))
	if err != nil {
		pathErr(w, err)
		return
	}
	query := strings.TrimSpace(q.Get("q"))
	if query == "" {
		jsonErr(w, http.StatusBadRequest, "empty search")
		return
	}
	if fi, err := os.Stat(full); err != nil {
		pathErr(w, err)
		return
	} else if !fi.IsDir() {
		jsonErr(w, http.StatusBadRequest, "not a folder")
		return
	}
	limit := searchDefaultLimit
	if n, err := strconv.Atoi(q.Get("limit")); err == nil && n > 0 {
		limit = min(n, searchMaxLimit)
	}
	start := time.Now()
	s := &searcher{
		base: full, m: newMatcher(query), limit: limit, ctx: r.Context(),
		deadline: start.Add(searchTimeLimit), sem: make(chan struct{}, searchWorkers),
	}
	s.cur.Store("")
	done := make(chan struct{})
	s.wg.Add(1)
	go s.visit(full)
	go func() { s.wg.Wait(); close(done) }()

	summary := func() map[string]any {
		s.mu.Lock()
		found := len(s.results)
		s.mu.Unlock()
		return map[string]any{
			"scanned": s.scanned.Load(), "dirs": s.dirs.Load(), "found": found,
			"ms":        time.Since(start).Milliseconds(),
			"truncated": s.truncated.Load(), "timedOut": s.timedO.Load(),
		}
	}

	if q.Get("stream") != "1" {
		<-done
		if r.Context().Err() != nil {
			return
		}
		sm := summary()
		addLinkTokens(r, s.results, q.Get("root"), clean)
		writeJSON(w, 200, map[string]any{
			"path": clean, "q": query, "entries": s.results,
			"truncated": sm["truncated"], "timedOut": sm["timedOut"],
			"scanned": sm["scanned"], "elapsedMs": sm["ms"],
		})
		return
	}

	// streaming NDJSON
	w.Header().Set("Content-Type", "application/x-ndjson; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("X-Accel-Buffering", "no")
	rc := http.NewResponseController(w)
	enc := json.NewEncoder(w)
	sent := 0
	send := func(final bool) bool {
		s.mu.Lock()
		newer := append([]entry(nil), s.results[sent:]...)
		s.mu.Unlock()
		if len(newer) > 0 {
			addLinkTokens(r, newer, q.Get("root"), clean)
			if enc.Encode(map[string]any{"t": "r", "entries": newer}) != nil {
				return false
			}
			sent += len(newer)
		}
		m := summary()
		if final {
			m["t"] = "end"
		} else {
			m["t"] = "p"
			m["dir"] = s.cur.Load()
		}
		if enc.Encode(m) != nil {
			return false
		}
		return rc.Flush() == nil
	}
	tick := time.NewTicker(300 * time.Millisecond)
	defer tick.Stop()
	if !send(false) {
		s.stop.Store(true)
		return
	}
	for {
		select {
		case <-done:
			send(true)
			return
		case <-r.Context().Done():
			s.stop.Store(true)
			return
		case <-tick.C:
			if !send(false) {
				s.stop.Store(true)
				return
			}
		}
	}
}
