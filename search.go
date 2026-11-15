package main

// Recursive file name search under a folder.
//
//   q = space separated terms, all must match (AND), case-insensitive.
//   A term containing * ? or [ is a glob matched against the whole name (e.g. *.mp4),
//   otherwise it is a substring match.

import (
	"errors"
	"io/fs"
	"net/http"
	"os"
	"path"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

const (
	searchDefaultLimit = 1000
	searchMaxLimit     = 5000
	searchTimeLimit    = 20 * time.Second
)

var errStopWalk = errors.New("stop")

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
	m := newMatcher(query)
	limit := searchDefaultLimit
	if n, err := strconv.Atoi(q.Get("limit")); err == nil && n > 0 {
		limit = min(n, searchMaxLimit)
	}

	start := time.Now()
	deadline := start.Add(searchTimeLimit)
	ctx := r.Context()
	results := make([]entry, 0, 64)
	scanned, truncated, timedOut := 0, false, false

	werr := filepath.WalkDir(full, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			if p == full {
				return err
			}
			if d != nil && d.IsDir() {
				return fs.SkipDir
			}
			return nil
		}
		if p == full {
			return nil
		}
		scanned++
		if scanned%256 == 0 {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			if time.Now().After(deadline) {
				timedOut = true
				return errStopWalk
			}
		}
		name := d.Name()
		if strings.HasSuffix(name, partSuffix) || !m.match(name) {
			return nil
		}
		var info fs.FileInfo
		if d.Type()&fs.ModeSymlink != 0 {
			info, err = os.Stat(p)
		} else {
			info, err = d.Info()
		}
		if err != nil {
			return nil
		}
		rel, _ := filepath.Rel(full, p)
		e := entry{Name: filepath.ToSlash(rel), Dir: info.IsDir(), MTime: info.ModTime().Unix()}
		if !e.Dir {
			e.Size = info.Size()
		}
		results = append(results, e)
		if len(results) >= limit {
			truncated = true
			return errStopWalk
		}
		return nil
	})
	if werr != nil && !errors.Is(werr, errStopWalk) {
		if ctx.Err() != nil {
			return // client went away
		}
		pathErr(w, werr)
		return
	}
	writeJSON(w, 200, map[string]any{
		"path": clean, "q": query, "entries": results,
		"truncated": truncated, "timedOut": timedOut,
		"scanned": scanned, "elapsedMs": time.Since(start).Milliseconds(),
	})
}
