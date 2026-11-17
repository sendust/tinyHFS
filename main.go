// tinyHFS - tiny HTTP file server by sendust
//
// usage: tinyhfs[.exe] [-auth password] [root_path1 root_path2 "path N" ...] [port]
//
//	default root = directory of the executable, default port = 10002
package main

import (
	"archive/zip"
	"compress/flate"
	"embed"
	"encoding/json"
	"errors"
	"fmt"
	"html"
	"io"
	"io/fs"
	"log"
	"mime"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/signal"
	"path"
	"path/filepath"
	"runtime"
	"runtime/debug"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

//go:embed web/index.html web/login.html web/webdav.html web/favicon.ico web/favicon-32x32.png web/apple-touch-icon.png
var webFS embed.FS

// buildDate may be set with: -ldflags "-X 'main.buildDate=2026-01-01 12:00:00'"
var buildDate = ""

const (
	defaultPort = 10002
	partSuffix  = ".tinyhfs-part" // partial upload files (hidden from listing)
)

type entry struct {
	Name  string `json:"name"` // file name (search: path relative to the search folder)
	Dir   bool   `json:"dir"`
	Size  int64  `json:"size"`
	MTime int64  `json:"mtime"`
	// Denied: the name is listed but the file cannot be accessed (e.g. folder without x permission)
	Denied bool `json:"denied,omitempty"`
}

// warnDenied logs inaccessible paths, at most once per folder every 10 minutes
// (the UI polls/refreshes the same folder often).
var deniedLogged sync.Map // folder -> time.Time

func warnDenied(dir string, n int, example string, err error) {
	now := time.Now()
	key := fmt.Sprint(dir, "|", n == 0)
	if t, ok := deniedLogged.Load(key); ok && now.Sub(t.(time.Time)) < 10*time.Minute {
		return
	}
	deniedLogged.Store(key, now)
	if n == 0 {
		logger.Printf("WARN cannot read folder %s: %v (check permissions: folders need r+x for the tinyHFS user)", dir, err)
		return
	}
	logger.Printf("WARN %s: %d entries not accessible, e.g. %q: %v (folders need r+x for the tinyHFS user)", dir, n, example, err)
}

type Root struct {
	Name string
	Path string
}

var (
	roots     []Root
	logger    *log.Logger // stdout + log file
	flog      *log.Logger // log file only
	indexHTML []byte
	version   string
)

// ---------------------------------------------------------------- logging

type dailyWriter struct {
	mu  sync.Mutex
	dir string
	day string
	f   *os.File
}

func (w *dailyWriter) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	d := time.Now().Format("20060102")
	if w.f == nil || d != w.day {
		if w.f != nil {
			w.f.Close()
			w.f = nil
		}
		f, err := os.OpenFile(filepath.Join(w.dir, "tinyhfs_"+d+".log"), os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o644)
		if err != nil {
			return len(p), nil // never fail the caller because of logging
		}
		w.f, w.day = f, d
	}
	return w.f.Write(p)
}

func setupLog() {
	dir := "log"
	if err := os.MkdirAll(dir, 0o755); err != nil {
		fmt.Fprintln(os.Stderr, "cannot create ./log:", err)
	}
	fw := &dailyWriter{dir: dir}
	logger = log.New(io.MultiWriter(os.Stdout, fw), "", log.LstdFlags)
	flog = log.New(fw, "", log.LstdFlags)
}

// ---------------------------------------------------------------- version

func versionString() string {
	if buildDate != "" {
		return buildDate
	}
	if bi, ok := debug.ReadBuildInfo(); ok {
		for _, s := range bi.Settings {
			if s.Key == "vcs.time" && s.Value != "" {
				if t, err := time.Parse(time.RFC3339, s.Value); err == nil {
					return t.Local().Format("2006-01-02 15:04:05")
				}
				return s.Value
			}
		}
	}
	// fallback: executable modification time (~ build time)
	if exe, err := os.Executable(); err == nil {
		if fi, err := os.Stat(exe); err == nil {
			return fi.ModTime().Format("2006-01-02 15:04:05")
		}
	}
	return "dev"
}

// ---------------------------------------------------------------- args

func usage() {
	fmt.Println(`tinyHFS by sendust - tiny HTTP file server

usage: tinyhfs [-auth password] [root_path1 root_path2 "path N" ...] [port]

  -auth pw   require this password (or set env TINYHFS_AUTH)
  root_path  folder(s) to share (default: folder of the executable)
  port       TCP port (default: 10002, increments by 1 if busy)`)
}

func parseArgs(args []string) ([]Root, int, error) {
	port := defaultPort
	var paths []string
	for i, a := range args {
		if i == len(args)-1 {
			if p, err := strconv.Atoi(a); err == nil && p > 0 && p < 65536 {
				if _, e := os.Stat(a); e != nil { // not an existing folder named like a number
					port = p
					continue
				}
			}
		}
		paths = append(paths, a)
	}
	if len(paths) == 0 {
		exe, err := os.Executable()
		if err != nil {
			return nil, 0, err
		}
		if r, err := filepath.EvalSymlinks(exe); err == nil {
			exe = r
		}
		paths = []string{filepath.Dir(exe)}
	}
	var rs []Root
	for _, p := range paths {
		abs, err := filepath.Abs(p)
		if err != nil {
			return nil, 0, err
		}
		fi, err := os.Stat(abs)
		if err != nil {
			return nil, 0, fmt.Errorf("root path %q: %v", p, err)
		}
		if !fi.IsDir() {
			return nil, 0, fmt.Errorf("root path %q is not a folder", p)
		}
		name := filepath.Base(abs)
		if name == "" || name == "." || name == string(filepath.Separator) || strings.HasSuffix(name, `:\`) {
			name = abs
		}
		rs = append(rs, Root{Name: name, Path: abs})
	}
	return rs, port, nil
}

// ---------------------------------------------------------------- path helpers

var errForbidden = errors.New("forbidden path")

// resolve maps (root index, slash path relative to root) to an absolute host path inside the root.
func resolve(rootIdx, rel string) (string, *Root, string, error) {
	i, err := strconv.Atoi(rootIdx)
	if err != nil || i < 0 || i >= len(roots) {
		return "", nil, "", errors.New("invalid root")
	}
	r := &roots[i]
	clean := path.Clean("/" + strings.ReplaceAll(rel, `\`, "/"))
	if runtime.GOOS == "windows" && strings.Contains(clean, ":") {
		return "", nil, "", errForbidden
	}
	full := filepath.Join(r.Path, filepath.FromSlash(clean))
	rp, err := filepath.Rel(r.Path, full)
	if err != nil || rp == ".." || strings.HasPrefix(rp, ".."+string(filepath.Separator)) {
		return "", nil, "", errForbidden
	}
	return full, r, clean, nil
}

// validName checks a single file/folder name segment supplied by a client.
func validName(n string) bool {
	if n == "" || n == "." || n == ".." || len(n) > 255 || strings.ContainsAny(n, `/\`) {
		return false
	}
	for _, c := range n {
		if c < 0x20 {
			return false
		}
	}
	if runtime.GOOS == "windows" && strings.ContainsAny(n, `:*?"<>|`) {
		return false
	}
	return !strings.HasSuffix(n, partSuffix)
}

// validRelPath checks a client supplied relative path like "dir/sub/file.txt".
func validRelPath(p string) bool {
	if p == "" {
		return false
	}
	for _, seg := range strings.Split(p, "/") {
		if !validName(seg) {
			return false
		}
	}
	return true
}

func splitRootPath(prefix, urlPath string) (string, string) {
	rest := strings.TrimPrefix(urlPath, prefix)
	idx, rel, _ := strings.Cut(rest, "/")
	return idx, "/" + rel
}

func rfc5987(s string) string {
	var b strings.Builder
	for _, c := range []byte(s) {
		if (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') || (c >= '0' && c <= '9') || strings.IndexByte("-._~", c) >= 0 {
			b.WriteByte(c)
		} else {
			fmt.Fprintf(&b, "%%%02X", c)
		}
	}
	return b.String()
}

func contentDisposition(kind, name string) string {
	ascii := strings.Map(func(r rune) rune {
		if r < 0x20 || r > 0x7e || r == '"' || r == '\\' {
			return '_'
		}
		return r
	}, name)
	return fmt.Sprintf(`%s; filename="%s"; filename*=UTF-8''%s`, kind, ascii, rfc5987(name))
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}

func jsonErr(w http.ResponseWriter, code int, msg string) {
	writeJSON(w, code, map[string]any{"error": msg})
}

func pathErr(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, fs.ErrNotExist):
		jsonErr(w, http.StatusNotFound, "not found")
	case errors.Is(err, fs.ErrPermission), errors.Is(err, errForbidden):
		jsonErr(w, http.StatusForbidden, err.Error())
	default:
		jsonErr(w, http.StatusBadRequest, err.Error())
	}
}

// ---------------------------------------------------------------- transfer tracking (download progress)

var errCancelled = errors.New("cancelled by user")

type transfer struct {
	name     string
	total    atomic.Int64
	done     atomic.Int64 // source bytes processed
	sent     atomic.Int64 // bytes written to the client
	cancel   atomic.Bool
	finished atomic.Bool
	start    time.Time
	endNs    atomic.Int64
	mu       sync.Mutex
	err      string
}

var transfers sync.Map // tid -> *transfer

func validTid(s string) bool {
	if s == "" || len(s) > 40 {
		return false
	}
	for _, c := range s {
		if !((c >= '0' && c <= '9') || (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z')) {
			return false
		}
	}
	return true
}

func startTransfer(tid, name string) *transfer {
	t := &transfer{name: name, start: time.Now()}
	t.total.Store(-1)
	if validTid(tid) {
		transfers.Store(tid, t)
	}
	return t
}

func (t *transfer) finish(err error) {
	t.mu.Lock()
	if err != nil {
		t.err = err.Error()
	} else if tot := t.total.Load(); tot >= 0 && t.done.Load() < tot {
		t.err = "interrupted"
	}
	t.mu.Unlock()
	t.endNs.Store(time.Now().UnixNano())
	t.finished.Store(true)
}

func transferJanitor() {
	for range time.Tick(time.Minute) {
		now := time.Now()
		transfers.Range(func(k, v any) bool {
			t := v.(*transfer)
			if (t.finished.Load() && now.Sub(time.Unix(0, t.endNs.Load())) > 10*time.Minute) || now.Sub(t.start) > 48*time.Hour {
				transfers.Delete(k)
			}
			return true
		})
	}
}

type trackWriter struct {
	http.ResponseWriter
	t          *transfer
	countTotal bool // take total from Content-Length
	countDone  bool // done = sent (plain file download)
	hdr        bool
}

func (w *trackWriter) WriteHeader(code int) {
	if !w.hdr {
		w.hdr = true
		if w.countTotal {
			if cl, err := strconv.ParseInt(w.Header().Get("Content-Length"), 10, 64); err == nil {
				w.t.total.Store(cl)
			}
		}
	}
	w.ResponseWriter.WriteHeader(code)
}

func (w *trackWriter) Write(p []byte) (int, error) {
	if w.t.cancel.Load() {
		return 0, errCancelled
	}
	if !w.hdr {
		w.WriteHeader(http.StatusOK)
	}
	n, err := w.ResponseWriter.Write(p)
	w.t.sent.Add(int64(n))
	if w.countDone {
		w.t.done.Add(int64(n))
	}
	return n, err
}

type countReader struct {
	r io.Reader
	t *transfer
}

func (c *countReader) Read(p []byte) (int, error) {
	if c.t.cancel.Load() {
		return 0, errCancelled
	}
	n, err := c.r.Read(p)
	c.t.done.Add(int64(n))
	return n, err
}

// ---------------------------------------------------------------- handlers

func handleIndex(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/" {
		http.NotFound(w, r)
		return
	}
	switch r.Method {
	case http.MethodGet, http.MethodHead:
	case http.MethodOptions: // Windows WebDAV client probes the server root
		w.Header().Set("DAV", "1, 2")
		w.Header().Set("MS-Author-Via", "DAV")
		w.Header().Set("Allow", "OPTIONS, GET, HEAD")
		w.WriteHeader(http.StatusOK)
		return
	default:
		w.Header().Set("Allow", "OPTIONS, GET, HEAD")
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-cache")
	w.Write(indexHTML)
}

// static icons embedded from web/
var iconFiles = map[string]string{
	"/favicon.ico":          "image/x-icon",
	"/favicon-32x32.png":    "image/png",
	"/apple-touch-icon.png": "image/png",
}

func handleIcon(w http.ResponseWriter, r *http.Request) {
	ct, ok := iconFiles[r.URL.Path]
	if !ok {
		http.NotFound(w, r)
		return
	}
	b, err := webFS.ReadFile("web" + r.URL.Path)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	w.Header().Set("Content-Type", ct)
	w.Header().Set("Cache-Control", "public, max-age=86400")
	w.Write(b)
}

func handleRoots(w http.ResponseWriter, r *http.Request) {
	type rootInfo struct {
		Index int    `json:"index"`
		Name  string `json:"name"`
		Path  string `json:"path"`
		Free  uint64 `json:"free"`
		Total uint64 `json:"total"`
	}
	out := make([]rootInfo, 0, len(roots))
	for i, rt := range roots {
		f, t := diskUsage(rt.Path)
		out = append(out, rootInfo{i, rt.Name, rt.Path, f, t})
	}
	type ipInfo struct {
		NIC string `json:"nic"`
		IP  string `json:"ip"`
		URL string `json:"url"`
	}
	ips := []ipInfo{}
	for _, a := range ipv4Addrs() {
		ips = append(ips, ipInfo{a.nic, a.ip, fmt.Sprintf("http://%s:%d/", a.ip, listenPort)})
	}
	writeJSON(w, 200, map[string]any{"version": version, "auth": authEnabled, "roots": out, "ips": ips, "hostname": hostName()})
}

func handleList(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	full, _, clean, err := resolve(q.Get("root"), q.Get("path"))
	if err != nil {
		pathErr(w, err)
		return
	}
	des, err := os.ReadDir(full)
	if err != nil {
		if errors.Is(err, fs.ErrPermission) {
			warnDenied(full, 0, "", err)
		}
		if len(des) == 0 {
			pathErr(w, err)
			return
		}
	}
	list := make([]entry, 0, len(des))
	denied, firstDenied := 0, ""
	var deniedErr error
	for _, de := range des {
		name := de.Name()
		if strings.HasSuffix(name, partSuffix) {
			continue
		}
		fi, err := os.Stat(filepath.Join(full, name)) // follow symlinks
		if err != nil {
			var lerr error
			if fi, lerr = de.Info(); lerr != nil {
				if errors.Is(lerr, fs.ErrNotExist) {
					continue // removed while listing
				}
				// name is readable but the file is not: still show it
				if denied == 0 {
					firstDenied, deniedErr = name, lerr
				}
				denied++
				list = append(list, entry{Name: name, Dir: de.IsDir(), Size: -1, Denied: true})
				continue
			}
		}
		e := entry{Name: name, Dir: fi.IsDir(), MTime: fi.ModTime().Unix()}
		if !e.Dir {
			e.Size = fi.Size()
		}
		list = append(list, e)
	}
	if denied > 0 {
		warnDenied(full, denied, firstDenied, deniedErr)
	}
	free, total := diskUsage(full)
	writeJSON(w, 200, map[string]any{"path": clean, "free": free, "total": total, "entries": list})
}

func handleMkdir(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		jsonErr(w, http.StatusMethodNotAllowed, "POST only")
		return
	}
	q := r.URL.Query()
	full, _, _, err := resolve(q.Get("root"), q.Get("path"))
	if err != nil {
		pathErr(w, err)
		return
	}
	name := q.Get("name")
	if !validName(name) {
		jsonErr(w, http.StatusBadRequest, "invalid folder name")
		return
	}
	target := filepath.Join(full, name)
	if err := os.Mkdir(target, 0o755); err != nil {
		if errors.Is(err, fs.ErrExist) {
			jsonErr(w, http.StatusConflict, "already exists")
			return
		}
		pathErr(w, err)
		return
	}
	logger.Printf("mkdir %s by %s", target, clientIP(r))
	writeJSON(w, 200, map[string]any{"ok": true})
}

// upload target from query: root, path (folder), name (relative path, may contain '/')
func uploadTarget(q url.Values) (target, baseDir string, err error) {
	baseDir, _, _, err = resolve(q.Get("root"), q.Get("path"))
	if err != nil {
		return
	}
	name := q.Get("name")
	if !validRelPath(name) {
		return "", "", errors.New("invalid file name")
	}
	target, _, _, err = resolve(q.Get("root"), q.Get("path")+"/"+name)
	return
}

func handleUploadStatus(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	target, baseDir, err := uploadTarget(q)
	if err != nil {
		pathErr(w, err)
		return
	}
	size, err := strconv.ParseInt(q.Get("size"), 10, 64)
	if err != nil || size < 0 {
		jsonErr(w, http.StatusBadRequest, "invalid size")
		return
	}
	exists := false
	if fi, err := os.Stat(target); err == nil {
		if fi.IsDir() {
			jsonErr(w, http.StatusConflict, "a folder with that name exists")
			return
		}
		exists = true
	}
	var offset int64
	part := target + partSuffix
	if fi, err := os.Stat(part); err == nil {
		offset = fi.Size()
		if offset > size {
			os.Truncate(part, 0)
			offset = 0
		}
	}
	free, _ := diskUsage(baseDir)
	if offset > 0 {
		logger.Printf("upload resume %s @%d/%d by %s", target, offset, size, clientIP(r))
	} else {
		logger.Printf("upload begin %s (%d bytes) by %s", target, size, clientIP(r))
	}
	writeJSON(w, 200, map[string]any{"offset": offset, "exists": exists, "free": free})
}

var uploadBusy sync.Map

func handleUpload(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPut && r.Method != http.MethodPost {
		jsonErr(w, http.StatusMethodNotAllowed, "PUT only")
		return
	}
	q := r.URL.Query()
	target, _, err := uploadTarget(q)
	if err != nil {
		pathErr(w, err)
		return
	}
	size, err1 := strconv.ParseInt(q.Get("size"), 10, 64)
	offset, err2 := strconv.ParseInt(q.Get("offset"), 10, 64)
	if err1 != nil || err2 != nil || size < 0 || offset < 0 || offset > size {
		jsonErr(w, http.StatusBadRequest, "invalid size/offset")
		return
	}
	if fi, err := os.Stat(target); err == nil && fi.IsDir() {
		jsonErr(w, http.StatusConflict, "a folder with that name exists")
		return
	}
	if _, busy := uploadBusy.LoadOrStore(target, true); busy {
		jsonErr(w, http.StatusLocked, "file is busy, retry")
		return
	}
	defer uploadBusy.Delete(target)

	if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
		pathErr(w, err)
		return
	}
	part := target + partSuffix
	f, err := os.OpenFile(part, os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		pathErr(w, err)
		return
	}
	fi, err := f.Stat()
	if err != nil {
		f.Close()
		jsonErr(w, 500, err.Error())
		return
	}
	if fi.Size() != offset {
		f.Close()
		writeJSON(w, http.StatusConflict, map[string]any{"error": "offset mismatch", "offset": fi.Size()})
		return
	}
	if _, err := f.Seek(offset, io.SeekStart); err != nil {
		f.Close()
		jsonErr(w, 500, err.Error())
		return
	}
	n, cerr := io.Copy(f, http.MaxBytesReader(w, r.Body, size-offset))
	if err := f.Close(); cerr == nil {
		cerr = err
	}
	newOff := offset + n
	if cerr != nil {
		logger.Printf("upload interrupted %s @%d/%d: %v", target, newOff, size, cerr)
		jsonErr(w, 500, cerr.Error())
		return
	}
	done := newOff == size
	if done {
		if err := os.Rename(part, target); err != nil {
			logger.Printf("upload rename failed %s: %v", target, err)
			jsonErr(w, 500, err.Error())
			return
		}
		if ms, err := strconv.ParseInt(q.Get("mtime"), 10, 64); err == nil && ms > 0 {
			os.Chtimes(target, time.Now(), time.UnixMilli(ms))
		}
		logger.Printf("upload complete %s (%d bytes) by %s", target, size, clientIP(r))
	}
	writeJSON(w, 200, map[string]any{"offset": newOff, "done": done})
}

func handleDownload(w http.ResponseWriter, r *http.Request) {
	idx, rel := splitRootPath("/dl/", r.URL.Path)
	full, _, _, err := resolve(idx, rel)
	if err != nil {
		pathErr(w, err)
		return
	}
	f, err := os.Open(full)
	if err != nil {
		pathErr(w, err)
		return
	}
	defer f.Close()
	fi, err := f.Stat()
	if err != nil {
		pathErr(w, err)
		return
	}
	if fi.IsDir() {
		jsonErr(w, http.StatusBadRequest, "is a folder (use zip download)")
		return
	}
	q := r.URL.Query()
	name := fi.Name()
	disp := "attachment"
	if q.Get("inline") == "1" {
		disp = "inline"
		// opened directly in a tab or the preview iframe (SVG, HTML, ...): never let the file run
		// scripts on our origin. Media / images / PDF are exempt (see inlineNeedsSandbox).
		if inlineNeedsSandbox(name) {
			w.Header().Set("Content-Security-Policy", "default-src 'none'; img-src 'self' data:; media-src 'self'; style-src 'self' 'unsafe-inline'; font-src 'self' data:; sandbox")
		}
		w.Header().Set("X-Content-Type-Options", "nosniff")
	} else {
		w.Header().Set("Content-Type", "application/octet-stream")
	}
	w.Header().Set("Content-Disposition", contentDisposition(disp, name))
	w.Header().Set("ETag", fmt.Sprintf(`"%x-%x"`, fi.ModTime().UnixNano(), fi.Size()))
	t := startTransfer(q.Get("tid"), name)
	tw := &trackWriter{ResponseWriter: w, t: t, countTotal: true, countDone: true}
	http.ServeContent(tw, r, name, fi.ModTime(), f) // handles Range / If-Range => resumable
	var ferr error
	if t.cancel.Load() {
		ferr = errCancelled
	}
	t.finish(ferr)
}

type zipItem struct {
	path   string // host path
	prefix string // name inside the archive
}

func handleZip(w http.ResponseWriter, r *http.Request) {
	idx, rel := splitRootPath("/zip/", r.URL.Path)
	full, root, clean, err := resolve(idx, rel)
	if err != nil {
		pathErr(w, err)
		return
	}
	fi, err := os.Stat(full)
	if err != nil {
		pathErr(w, err)
		return
	}
	if !fi.IsDir() {
		jsonErr(w, http.StatusBadRequest, "not a folder")
		return
	}
	q := r.URL.Query()
	level := flate.BestSpeed // default: less and fast compression
	if lv := q.Get("level"); lv != "" {
		if n, err := strconv.Atoi(lv); err == nil && n >= 0 && n <= 9 {
			level = n
		}
	}
	base := filepath.Base(full)
	if clean == "/" {
		base = root.Name
		if strings.ContainsAny(base, `/\:`) {
			base = "root"
		}
	}
	var items []zipItem
	zipName := base + ".zip"
	if names := q["n"]; len(names) > 0 {
		for _, n := range names {
			if !validRelPath(n) {
				jsonErr(w, http.StatusBadRequest, "invalid name: "+n)
				return
			}
			p := filepath.Join(full, filepath.FromSlash(n))
			if _, err := os.Stat(p); err != nil {
				pathErr(w, err)
				return
			}
			items = append(items, zipItem{p, n})
		}
		if len(names) == 1 {
			zipName = path.Base(names[0]) + ".zip"
		}
	} else {
		items = []zipItem{{full, base}}
	}

	t := startTransfer(q.Get("tid"), zipName)
	// pre-scan total size so the client can show %/ETA
	var total int64
	for _, it := range items {
		filepath.WalkDir(it.path, func(p string, d fs.DirEntry, err error) error {
			if err == nil && d.Type().IsRegular() && !strings.HasSuffix(d.Name(), partSuffix) {
				if inf, e := d.Info(); e == nil {
					total += inf.Size()
				}
			}
			return nil
		})
	}
	t.total.Store(total)

	method := zip.Deflate
	if level == 0 {
		method = zip.Store
	}
	w.Header().Set("Content-Type", "application/zip")
	w.Header().Set("Content-Disposition", contentDisposition("attachment", zipName))
	w.Header().Set("Cache-Control", "no-store")
	tw := &trackWriter{ResponseWriter: w, t: t}
	zw := zip.NewWriter(tw)
	zw.RegisterCompressor(zip.Deflate, func(out io.Writer) (io.WriteCloser, error) {
		return flate.NewWriter(out, level)
	})
	logger.Printf("zip start %s (%d items, %d bytes, level %d) by %s", full, len(items), total, level, clientIP(r))

	var werr error
	for _, it := range items {
		werr = filepath.WalkDir(it.path, func(p string, d fs.DirEntry, err error) error {
			if t.cancel.Load() {
				return errCancelled
			}
			if err != nil {
				flog.Printf("zip: skip %s: %v", p, err)
				if d != nil && d.IsDir() {
					return fs.SkipDir
				}
				return nil
			}
			rp, _ := filepath.Rel(it.path, p)
			name := it.prefix
			if rp != "." {
				name = path.Join(it.prefix, filepath.ToSlash(rp))
			}
			info, err := d.Info()
			if err != nil {
				return nil
			}
			if d.IsDir() {
				hdr := &zip.FileHeader{Name: name + "/", Method: zip.Store, Modified: info.ModTime()}
				hdr.SetMode(info.Mode())
				_, err := zw.CreateHeader(hdr)
				return err
			}
			if !d.Type().IsRegular() || strings.HasSuffix(d.Name(), partSuffix) {
				return nil
			}
			f, err := os.Open(p)
			if err != nil {
				flog.Printf("zip: skip %s: %v", p, err)
				return nil
			}
			defer f.Close()
			hdr, err := zip.FileInfoHeader(info)
			if err != nil {
				return nil
			}
			hdr.Name = name
			hdr.Method = method
			wr, err := zw.CreateHeader(hdr)
			if err != nil {
				return err
			}
			_, err = io.Copy(wr, &countReader{f, t})
			return err
		})
		if werr != nil {
			break
		}
	}
	if werr == nil {
		werr = zw.Close()
	}
	if werr != nil {
		logger.Printf("zip aborted %s: %v", full, werr)
	} else {
		logger.Printf("zip done %s (%d bytes sent)", full, t.sent.Load())
	}
	t.finish(werr)
}

func handleProgress(w http.ResponseWriter, r *http.Request) {
	out := map[string]any{}
	ids := strings.Split(r.URL.Query().Get("ids"), ",")
	if len(ids) > 500 {
		ids = ids[:500]
	}
	for _, id := range ids {
		v, ok := transfers.Load(id)
		if !ok {
			continue
		}
		t := v.(*transfer)
		t.mu.Lock()
		e := t.err
		t.mu.Unlock()
		out[id] = map[string]any{
			"done": t.done.Load(), "total": t.total.Load(), "sent": t.sent.Load(),
			"finished": t.finished.Load(), "error": e,
		}
	}
	writeJSON(w, 200, out)
}

func handleCancel(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		jsonErr(w, http.StatusMethodNotAllowed, "POST only")
		return
	}
	if v, ok := transfers.Load(r.URL.Query().Get("tid")); ok {
		v.(*transfer).cancel.Store(true)
	}
	writeJSON(w, 200, map[string]any{"ok": true})
}

// ---------------------------------------------------------------- access log

type statusWriter struct {
	http.ResponseWriter
	code int
	n    int64
	hdr  bool
}

// Unwrap lets http.ResponseController reach the real writer (Flush for streamed search).
func (s *statusWriter) Unwrap() http.ResponseWriter { return s.ResponseWriter }

func (s *statusWriter) WriteHeader(c int) {
	if !s.hdr {
		s.code, s.hdr = c, true
	}
	s.ResponseWriter.WriteHeader(c)
}

func (s *statusWriter) Write(p []byte) (int, error) {
	s.hdr = true
	n, err := s.ResponseWriter.Write(p)
	s.n += int64(n)
	return n, err
}

func clientIP(r *http.Request) string {
	h, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return h
}

func logMW(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		sw := &statusWriter{ResponseWriter: w, code: 200}
		next.ServeHTTP(sw, r)
		p := r.URL.Path
		quiet := p == "/api/progress" || p == "/api/upload/status" || (p == "/api/upload" && sw.code < 400)
		switch r.Method { // WebDAV clients send these constantly
		case "PROPFIND", "OPTIONS", "LOCK", "UNLOCK", "PROPPATCH":
			quiet = quiet || sw.code < 400 || sw.code == http.StatusUnauthorized
		}
		if quiet {
			return
		}
		uri := r.URL.RequestURI()
		if u, err := url.QueryUnescape(uri); err == nil {
			uri = u
		}
		extra := ""
		if rg := r.Header.Get("Range"); rg != "" {
			extra = " range=" + rg
		}
		logger.Printf("%s %s %s %d %dB %s%s", clientIP(r), r.Method, uri, sw.code, sw.n,
			time.Since(start).Round(time.Millisecond), extra)
	})
}

// ---------------------------------------------------------------- network

type nicIP struct{ nic, ip string }

func hostName() string {
	h, _ := os.Hostname()
	return h
}

// inlineNeedsSandbox: only types that can run script when opened as a page get the sandbox CSP.
// Video/audio/raster images/PDF are exempt: Chrome's built-in media and PDF viewers break
// inside a sandboxed document (e.g. "open original in a new tab" shows nothing).
func inlineNeedsSandbox(name string) bool {
	ct, _, _ := strings.Cut(mime.TypeByExtension(strings.ToLower(filepath.Ext(name))), ";")
	switch {
	case strings.HasPrefix(ct, "video/"), strings.HasPrefix(ct, "audio/"):
		return false
	case strings.HasPrefix(ct, "image/") && ct != "image/svg+xml":
		return false
	case ct == "application/pdf":
		return false
	}
	return true
}

// consistent Content-Type for inline preview (OS mime tables differ, esp. on Windows)
func init() {
	for ext, typ := range map[string]string{
		".mp4": "video/mp4", ".m4v": "video/mp4", ".webm": "video/webm", ".ogv": "video/ogg",
		".mov": "video/quicktime", ".mkv": "video/x-matroska",
		".mp3": "audio/mpeg", ".m4a": "audio/mp4", ".aac": "audio/aac", ".wav": "audio/wav",
		".flac": "audio/flac", ".ogg": "audio/ogg", ".oga": "audio/ogg", ".opus": "audio/ogg", ".weba": "audio/webm",
		".md": "text/plain; charset=utf-8", ".log": "text/plain", ".pdf": "application/pdf",
		".webp": "image/webp", ".avif": "image/avif", ".svg": "image/svg+xml", ".jfif": "image/jpeg", ".apng": "image/apng",
	} {
		mime.AddExtensionType(ext, typ)
	}
}

var listenPort int // port actually bound (shown in the web UI)

// all IPv4 addresses of interfaces that are up with link
func ipv4Addrs() []nicIP {
	var out []nicIP
	seen := map[string]bool{}
	ifs, err := net.Interfaces()
	if err != nil {
		return nil
	}
	for _, ifc := range ifs {
		if ifc.Flags&net.FlagUp == 0 || ifc.Flags&net.FlagRunning == 0 {
			continue
		}
		addrs, err := ifc.Addrs()
		if err != nil {
			continue
		}
		for _, a := range addrs {
			var ip net.IP
			switch v := a.(type) {
			case *net.IPNet:
				ip = v.IP
			case *net.IPAddr:
				ip = v.IP
			}
			ip4 := ip.To4()
			if ip4 == nil || seen[ip4.String()] {
				continue
			}
			seen[ip4.String()] = true
			out = append(out, nicIP{ifc.Name, ip4.String()})
		}
	}
	return out
}

// listenAll binds every address on the same port; when the port is busy, tries port+1.
func listenAll(ips []nicIP, port int) ([]net.Listener, []nicIP, int, error) {
	for p := port; p <= 65535 && p < port+200; p++ {
		var lns []net.Listener
		var used []nicIP
		conflict := false
		for _, n := range ips {
			ln, err := net.Listen("tcp4", net.JoinHostPort(n.ip, strconv.Itoa(p)))
			if err != nil {
				// is it the port (busy) or the address (unusable)?
				if probe, e2 := net.Listen("tcp4", net.JoinHostPort(n.ip, "0")); e2 == nil {
					probe.Close()
					logger.Printf("port %d busy on %s (%v), trying %d", p, n.ip, err, p+1)
					conflict = true
					break
				}
				logger.Printf("skip %s (%s): %v", n.ip, n.nic, err)
				continue
			}
			lns = append(lns, ln)
			used = append(used, n)
		}
		if conflict {
			for _, l := range lns {
				l.Close()
			}
			continue
		}
		if len(lns) == 0 {
			return nil, nil, 0, errors.New("no network interface could be bound")
		}
		return lns, used, p, nil
	}
	return nil, nil, 0, fmt.Errorf("no free port found from %d", port)
}

func fmtBytes(n uint64) string {
	u := []string{"B", "KB", "MB", "GB", "TB", "PB"}
	f := float64(n)
	i := 0
	for f >= 1024 && i < len(u)-1 {
		f /= 1024
		i++
	}
	return fmt.Sprintf("%.1f %s", f, u[i])
}

// ---------------------------------------------------------------- main

func main() {
	for _, a := range os.Args[1:] {
		if a == "-h" || a == "--help" || a == "/?" {
			usage()
			return
		}
	}
	setupLog()
	version = versionString()

	args, pw, err := extractAuth(os.Args[1:])
	if err != nil {
		logger.Println("error:", err)
		usage()
		os.Exit(1)
	}
	if pw == "" {
		pw = os.Getenv("TINYHFS_AUTH")
	}
	setPassword(pw)

	var port int
	roots, port, err = parseArgs(args)
	if err != nil {
		logger.Println("error:", err)
		usage()
		os.Exit(1)
	}

	b, err := webFS.ReadFile("web/index.html")
	if err != nil {
		logger.Fatal(err)
	}
	indexHTML = []byte(strings.ReplaceAll(string(b), "{{VERSION}}", html.EscapeString(version)))
	if b, err = webFS.ReadFile("web/login.html"); err != nil {
		logger.Fatal(err)
	}
	host, _ := os.Hostname()
	if host == "" {
		host = "unknown host"
	}
	loginHTML = []byte(strings.NewReplacer(
		"{{VERSION}}", html.EscapeString(version),
		"{{HOSTNAME}}", html.EscapeString(host),
	).Replace(string(b)))
	if err := initWebDAV(host); err != nil {
		logger.Fatal(err)
	}

	ips := ipv4Addrs()
	if len(ips) == 0 {
		ips = []nicIP{{"any", "0.0.0.0"}}
	}
	lns, used, port, err := listenAll(ips, port)
	if err != nil {
		logger.Fatal(err)
	}
	listenPort = port

	mux := http.NewServeMux()
	mux.HandleFunc("/", handleIndex)
	for p := range iconFiles {
		mux.HandleFunc(p, handleIcon)
	}
	mux.HandleFunc("/api/roots", handleRoots)
	mux.HandleFunc("/api/list", handleList)
	mux.HandleFunc("/api/mkdir", handleMkdir)
	mux.HandleFunc("/api/upload/status", handleUploadStatus)
	mux.HandleFunc("/api/upload", handleUpload)
	mux.HandleFunc("/api/progress", handleProgress)
	mux.HandleFunc("/api/cancel", handleCancel)
	mux.HandleFunc("/api/search", handleSearch)
	mux.HandleFunc("/api/delete", handleDelete)
	mux.HandleFunc("/login", handleLoginPage)
	mux.HandleFunc("/api/login", handleLogin)
	mux.HandleFunc("/api/logout", handleLogout)
	mux.HandleFunc(davPrefix, handleWebDAV)
	mux.HandleFunc(davPrefix+"/", handleWebDAV)
	mux.HandleFunc("/dl/", handleDownload)
	mux.HandleFunc("/zip/", handleZip)

	srv := &http.Server{
		Handler:           logMW(authMW(mux)),
		ReadHeaderTimeout: 30 * time.Second,
		IdleTimeout:       120 * time.Second,
	}
	go transferJanitor()
	go authJanitor()

	var sb strings.Builder
	fmt.Fprintf(&sb, "\n==================================================\n")
	fmt.Fprintf(&sb, " tinyHFS by sendust  (%s)\n", version)
	fmt.Fprintf(&sb, "==================================================\n")
	fmt.Fprintf(&sb, " Shared folders:\n")
	for i, rt := range roots {
		f, t := diskUsage(rt.Path)
		fmt.Fprintf(&sb, "   [%d] %s\n       free %s / %s\n", i, rt.Path, fmtBytes(f), fmtBytes(t))
	}
	fmt.Fprintf(&sb, "\n Open one of these addresses in a web browser:\n")
	for _, n := range used {
		fmt.Fprintf(&sb, "   http://%s:%d/    (%s)\n", n.ip, port, n.nic)
	}
	fmt.Fprintf(&sb, "\n WebDAV (network drive):\n")
	for _, n := range used {
		fmt.Fprintf(&sb, "   http://%s:%d%s/\n", n.ip, port, davPrefix)
	}
	fmt.Fprintf(&sb, "   (open the WebDAV address in a web browser for the connection guide)\n")
	if authEnabled {
		fmt.Fprintf(&sb, "\n Authentication: ON (password required)\n")
	} else {
		fmt.Fprintf(&sb, "\n Authentication: OFF (anyone on the network can access; use -auth <password>)\n")
	}
	cred, wcred := "", ""
	if authEnabled {
		cred, wcred = "-u :<password> ", "--user=x --password=<password> "
	}
	fmt.Fprintf(&sb, "\n Command line download (resumable):\n")
	fmt.Fprintf(&sb, "   curl %s-C - -O http://<address>:%d/dl/0/<path/file>\n", cred, port)
	fmt.Fprintf(&sb, "   wget %s-c http://<address>:%d/dl/0/<path/file>\n", wcred, port)
	fmt.Fprintf(&sb, "\n Logs: %s\n Press Ctrl+C to stop.\n", mustAbs("log"))
	fmt.Fprintf(&sb, "==================================================\n")
	fmt.Print(sb.String())
	flog.Print(sb.String())

	for _, ln := range lns {
		go func(ln net.Listener) {
			if err := srv.Serve(ln); err != nil && !errors.Is(err, http.ErrServerClosed) {
				logger.Printf("serve %s: %v", ln.Addr(), err)
			}
		}(ln)
	}

	sig := make(chan os.Signal, 1)
	signal.Notify(sig, os.Interrupt)
	<-sig
	logger.Println("shutting down")
	srv.Close()
}

func mustAbs(p string) string {
	if a, err := filepath.Abs(p); err == nil {
		return a
	}
	return p
}
