package main

// Server to server copy ("drag a file from one tinyHFS browser tab, drop it on another").
//
// The page of server A puts {origin, items, token} into the drag data. The page of server B
// sends it to its own server (POST /api/rcopy) and server B downloads the items directly
// from server A over HTTP (/api/list + /dl/), so the data never passes through the browser.
//
// When A runs with -auth, B has no session there. Every /api/list response of A therefore
// carries "xtok": an HMAC signed, read-only token for that folder subtree, valid for 12 h.
// B sends it in the X-TinyHFS-Token header (headers are not written to the access log).
//
// SSRF limits: B only sends GET requests for /api/list and /dl/ to the origin taken from the
// drop, does not follow redirects, and gives up unless the origin answers like tinyHFS.
//
// Fallbacks when B cannot reach A at the address the browser used:
//  1. the drag also carries A's other IPv4 addresses ("alts"); B probes all of them at once
//     and uses the first one (in order) that answers
//  2. if none answers, /api/rcopy returns 502 {"unreachable":true} and the page relays the
//     files itself: it reads them from A (CORS, allowed only with a valid token, see corsMW)
//     and uploads them to B with the normal resumable upload.

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"path"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync/atomic"
	"time"
)

const (
	xTokenHeader   = "X-TinyHFS-Token"
	xTokenTTL      = 12 * time.Hour
	rcopyMaxJobs   = 4      // copy jobs running at the same time on this server
	rcopyMaxFiles  = 200000 // files in one job
	rcopyRetries   = 5
	rcopyPartInfix = ".rcopy" // name.rcopy.tinyhfs-part: never collides with an upload's part file
)

var xSecret = func() []byte {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		panic(err)
	}
	return b
}()

// ---------------------------------------------------------------- token (source side)

func xSign(payload string) string {
	m := hmac.New(sha256.New, xSecret)
	m.Write([]byte(payload))
	return base64.RawURLEncoding.EncodeToString(m.Sum(nil)[:18])
}

// makeXToken grants read access to root/dir and everything below it.
func makeXToken(root int, dir string) string {
	p := fmt.Sprintf("%d|%d|%s", root, time.Now().Add(xTokenTTL).Unix(), dir)
	return base64.RawURLEncoding.EncodeToString([]byte(p)) + "." + xSign(p)
}

func xTokenAllows(tok, root, p string) bool {
	b64, sig, ok := strings.Cut(tok, ".")
	if !ok || len(tok) > 4096 {
		return false
	}
	raw, err := base64.RawURLEncoding.DecodeString(b64)
	if err != nil || !hmac.Equal([]byte(sig), []byte(xSign(string(raw)))) {
		return false
	}
	parts := strings.SplitN(string(raw), "|", 3)
	if len(parts) != 3 || parts[0] != root {
		return false
	}
	if exp, err := strconv.ParseInt(parts[1], 10, 64); err != nil || time.Now().Unix() > exp {
		return false
	}
	dir, p := parts[2], path.Clean("/"+p)
	return dir == "/" || p == dir || strings.HasPrefix(p, dir+"/")
}

// corsMW lets a tinyHFS page of another server read /api/list and /dl/ of this server
// (browser relay fallback). Only requests carrying a valid token get CORS headers, so other
// web sites still cannot read anything; preflights reveal nothing and are answered for
// those two paths only. No cookies are involved (Allow-Credentials is never sent).
func corsMW(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		p := r.URL.Path
		origin := r.Header.Get("Origin")
		if origin == "" || (p != "/api/list" && !strings.HasPrefix(p, "/dl/")) {
			next.ServeHTTP(w, r)
			return
		}
		h := w.Header()
		if r.Method == http.MethodOptions && r.Header.Get("Access-Control-Request-Method") != "" {
			if r.Header.Get("Access-Control-Request-Method") == http.MethodGet &&
				strings.Contains(strings.ToLower(r.Header.Get("Access-Control-Request-Headers")), strings.ToLower(xTokenHeader)) {
				h.Set("Access-Control-Allow-Origin", origin)
				h.Set("Access-Control-Allow-Methods", "GET")
				h.Set("Access-Control-Allow-Headers", xTokenHeader+", Range")
				h.Set("Access-Control-Max-Age", "600")
				if r.Header.Get("Access-Control-Request-Private-Network") == "true" {
					h.Set("Access-Control-Allow-Private-Network", "true")
				}
				h.Add("Vary", "Origin")
			}
			w.WriteHeader(http.StatusNoContent)
			return
		}
		if r.Header.Get(xTokenHeader) == "" {
			next.ServeHTTP(w, r) // no CORS headers: other web sites cannot read the response
			return
		}
		h.Set("Access-Control-Allow-Origin", origin)
		h.Set("Access-Control-Expose-Headers", "Content-Length, Content-Range, ETag, Last-Modified")
		h.Add("Vary", "Origin")
		if !xTokenAuthorized(r) { // readable 401 so the page can say "token expired"
			jsonErr(w, http.StatusUnauthorized, "invalid or expired copy token")
			return
		}
		next.ServeHTTP(w, r)
	})
}

// xTokenAuthorized: read-only requests of another tinyHFS server holding a valid token.
func xTokenAuthorized(r *http.Request) bool {
	tok := r.Header.Get(xTokenHeader)
	if tok == "" || (r.Method != http.MethodGet && r.Method != http.MethodHead) {
		return false
	}
	switch {
	case r.URL.Path == "/api/list":
		q := r.URL.Query()
		return xTokenAllows(tok, q.Get("root"), q.Get("path"))
	case strings.HasPrefix(r.URL.Path, "/dl/"):
		idx, rel := splitRootPath("/dl/", r.URL.Path)
		return xTokenAllows(tok, idx, rel)
	}
	return false
}

// ---------------------------------------------------------------- copy job (destination side)

type rcopyReq struct {
	Src struct {
		Origin string   `json:"origin"`
		Alts   []string `json:"alts"` // other addresses of the source server, tried in order
		Root   int      `json:"root"`
		Token  string   `json:"token"`
		Items  []struct {
			Path string `json:"path"` // source path relative to the source root, e.g. "/dir/file.mp4"
		} `json:"items"`
	} `json:"src"`
	Dst struct {
		Root int    `json:"root"`
		Path string `json:"path"`
	} `json:"dst"`
	Policy string `json:"policy"` // "overwrite" | "skip" (existing destination files)
	Tid    string `json:"tid"`
}

type rcopyFile struct {
	src   string // source path (slash, relative to source root)
	dst   string // destination host path
	size  int64
	mtime int64
}

type rcopyJob struct {
	base   string // http(s)://host:port of the source
	root   int
	token  string
	client *http.Client
	t      *transfer
	ctx    context.Context

	dirs   []string // destination host folders to create
	files  []rcopyFile
	denied int

	copied, skipped, failed int
	firstErr                string
}

var rcopyRunning atomic.Int32

var rcopyClient = &http.Client{
	Transport: &http.Transport{
		Proxy:                 nil, // servers talk to each other directly
		DialContext:           (&net.Dialer{Timeout: 10 * time.Second, KeepAlive: 30 * time.Second}).DialContext,
		ResponseHeaderTimeout: 60 * time.Second,
		MaxIdleConnsPerHost:   4,
		IdleConnTimeout:       60 * time.Second,
	},
	CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
}

func handleRCopy(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		jsonErr(w, http.StatusMethodNotAllowed, "POST only")
		return
	}
	if !strings.HasPrefix(r.Header.Get("Content-Type"), "application/json") {
		jsonErr(w, http.StatusUnsupportedMediaType, "JSON body required")
		return
	}
	var req rcopyReq
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20)).Decode(&req); err != nil {
		jsonErr(w, http.StatusBadRequest, "bad request")
		return
	}
	bases := []string{}
	for i, o := range append([]string{req.Src.Origin}, req.Src.Alts...) {
		b, ok := originBase(o)
		if !ok {
			if i == 0 {
				jsonErr(w, http.StatusBadRequest, "invalid source server address")
				return
			}
			continue
		}
		if !slices.Contains(bases, b) && len(bases) < 16 {
			bases = append(bases, b)
		}
	}
	if len(req.Src.Items) == 0 || len(req.Src.Items) > 10000 {
		jsonErr(w, http.StatusBadRequest, "nothing to copy")
		return
	}
	for _, it := range req.Src.Items {
		if p := strings.Trim(it.Path, "/"); !validRelPath(p) {
			jsonErr(w, http.StatusBadRequest, "invalid source path")
			return
		}
	}
	dstFull, _, dstClean, err := resolve(strconv.Itoa(req.Dst.Root), req.Dst.Path)
	if err != nil {
		pathErr(w, err)
		return
	}
	if fi, err := os.Stat(dstFull); err != nil || !fi.IsDir() {
		jsonErr(w, http.StatusBadRequest, "destination folder not found")
		return
	}
	if !validTid(req.Tid) {
		jsonErr(w, http.StatusBadRequest, "invalid tid")
		return
	}
	if rcopyRunning.Add(1) > rcopyMaxJobs {
		rcopyRunning.Add(-1)
		jsonErr(w, http.StatusTooManyRequests, "too many copies running on this server, try again later")
		return
	}
	j := &rcopyJob{root: req.Src.Root, token: req.Src.Token, client: rcopyClient, ctx: context.Background()}
	first := "/" + strings.Trim(req.Src.Items[0].Path, "/")
	base, err := j.probe(r.Context(), bases, path.Dir(first))
	if err != nil {
		rcopyRunning.Add(-1)
		var ne netErr
		if errors.As(err, &ne) { // the page falls back to copying through the browser
			logger.Printf("rcopy: source %s not reachable from this server (%v), client relays", strings.Join(bases, " "), ne.error)
			writeJSON(w, http.StatusBadGateway, map[string]any{"error": "source server not reachable from this server", "unreachable": true})
			return
		}
		jsonErr(w, http.StatusBadGateway, err.Error())
		return
	}
	j.base, j.t = base, startTransfer(req.Tid, "copy")
	ip := clientIP(r)
	go func() {
		defer rcopyRunning.Add(-1)
		j.run(req, dstFull, dstClean, ip)
	}()
	writeJSON(w, 200, map[string]any{"ok": true, "tid": req.Tid})
}

func (j *rcopyJob) run(req rcopyReq, dstFull, dstClean, ip string) {
	start := time.Now()
	desc := fmt.Sprintf("%s [%d]%s", j.base, j.root, path.Dir("/"+strings.Trim(req.Src.Items[0].Path, "/")))
	if err := j.plan(req, dstFull); err != nil {
		logger.Printf("rcopy failed %s -> %s by %s: %v", desc, dstClean, ip, err)
		j.t.finish(err)
		return
	}
	var total int64
	for _, f := range j.files {
		total += f.size
	}
	j.t.total.Store(total)
	logger.Printf("rcopy start %s -> [%d]%s by %s: %d file(s), %s", desc, req.Dst.Root, dstClean, ip, len(j.files), fmtBytes(uint64(total)))

	var err error
	for _, d := range j.dirs {
		if e := os.MkdirAll(d, 0o755); e != nil {
			j.fail(filepath.Base(d), e)
		}
	}
	for _, f := range j.files {
		if j.t.cancel.Load() {
			err = errCancelled
			break
		}
		j.copyOne(f, req.Policy == "skip")
	}
	if err == nil && j.failed > 0 {
		err = fmt.Errorf("%d failed (%s)", j.failed, j.firstErr)
	}
	res := "done"
	if err != nil {
		res = "failed"
		if errors.Is(err, errCancelled) {
			res = "cancelled"
		}
	}
	extra := ""
	if j.denied > 0 {
		extra = fmt.Sprintf(", %d not readable on source", j.denied)
	}
	logger.Printf("rcopy %s %s -> [%d]%s: copied %d, skipped %d, failed %d%s, %s in %s",
		res, desc, req.Dst.Root, dstClean, j.copied, j.skipped, j.failed, extra,
		fmtBytes(uint64(j.t.done.Load())), time.Since(start).Round(time.Second))
	if err == nil && j.denied > 0 {
		err = fmt.Errorf("%d item(s) not readable on the source server", j.denied)
	}
	j.t.done.Store(min(j.t.done.Load(), total))
	if err == nil {
		j.t.done.Store(total)
	}
	j.t.finish(err)
}

func (j *rcopyJob) fail(name string, err error) {
	j.failed++
	if j.firstErr == "" {
		j.firstErr = name + ": " + errText(err)
	}
}

// ---- source listing

type remoteEntry struct {
	Name   string `json:"name"`
	Dir    bool   `json:"dir"`
	Size   int64  `json:"size"`
	MTime  int64  `json:"mtime"`
	Denied bool   `json:"denied"`
}

func (j *rcopyJob) get(ctx context.Context, p string, hdr map[string]string) (*http.Response, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, j.base+p, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("User-Agent", "tinyHFS")
	if j.token != "" {
		req.Header.Set(xTokenHeader, j.token)
	}
	for k, v := range hdr {
		req.Header.Set(k, v)
	}
	return j.client.Do(req)
}

// netErr: the source server could not be reached at all (no route, timeout, refused).
type netErr struct{ error }

func (e netErr) Unwrap() error { return e.error }

// originBase validates a client supplied server address and returns "scheme://host[:port]".
func originBase(o string) (string, bool) {
	u, err := url.Parse(o)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" || u.User != nil ||
		(u.Path != "" && u.Path != "/") || u.RawQuery != "" || u.Fragment != "" {
		return "", false
	}
	return u.Scheme + "://" + u.Host, true
}

// probe lists dir on every address at the same time and returns the first address (in the
// given order) that answers like tinyHFS. Only network failures everywhere give a netErr.
func (j *rcopyJob) probe(ctx context.Context, bases []string, dir string) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	type result struct {
		i   int
		err error
	}
	ch := make(chan result, len(bases)) // buffered: late probes never block after we return
	for i, b := range bases {
		go func() {
			jj := *j
			jj.base = b
			_, err := jj.listCtx(ctx, dir)
			ch <- result{i, err}
		}()
	}
	errs := make([]error, len(bases))
	done := make([]bool, len(bases))
	var grace <-chan time.Time // started when a less preferred address has answered
	for left := len(bases); left > 0; {
		select {
		case r := <-ch:
			left--
			done[r.i], errs[r.i] = true, r.err
			if r.err == nil && grace == nil {
				grace = time.After(time.Second)
			}
		case <-grace: // preferred addresses are still silent: take the best one that answered
			for i := range bases {
				if done[i] && errs[i] == nil {
					return bases[i], nil
				}
			}
		}
		// the preferred address wins as soon as every address before it has failed
		for i := range bases {
			if !done[i] {
				break
			}
			if errs[i] == nil {
				return bases[i], nil
			}
		}
	}
	var other error
	for i, err := range errs {
		if err == nil {
			return bases[i], nil
		}
		var ne netErr
		if !errors.As(err, &ne) && other == nil {
			other = err
		}
	}
	if other != nil {
		return "", other // reachable, but refused / not tinyHFS / ...
	}
	return "", errs[0]
}

func (j *rcopyJob) list(dir string) ([]remoteEntry, error) {
	ctx, cancel := context.WithTimeout(j.ctx, 2*time.Minute)
	defer cancel()
	return j.listCtx(ctx, dir)
}

func (j *rcopyJob) listCtx(ctx context.Context, dir string) ([]remoteEntry, error) {
	resp, err := j.get(ctx, fmt.Sprintf("/api/list?root=%d&path=%s", j.root, url.QueryEscape(dir)), nil)
	if err != nil {
		var ue *url.Error
		if errors.As(err, &ue) {
			err = ue.Err
		}
		return nil, netErr{fmt.Errorf("cannot reach source server: %v", err)}
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 256<<20))
	if err != nil {
		return nil, fmt.Errorf("source server: %v", err)
	}
	var d struct {
		Entries []remoteEntry `json:"entries"`
		Error   string        `json:"error"`
	}
	jerr := json.Unmarshal(body, &d)
	ct := resp.Header.Get("Content-Type")
	switch {
	case resp.StatusCode == http.StatusUnauthorized:
		return nil, errors.New("source server refused access (link expired? reload the source page and drag again)")
	case !strings.HasPrefix(ct, "application/json") || jerr != nil:
		return nil, errors.New("source is not a tinyHFS server")
	case resp.StatusCode != 200:
		return nil, fmt.Errorf("source server: %s", d.Error)
	case d.Entries == nil:
		return nil, errors.New("source is not a tinyHFS server")
	}
	return d.Entries, nil
}

// plan collects the files and folders to copy (folders recursively).
func (j *rcopyJob) plan(req rcopyReq, dstFull string) error {
	cache := map[string][]remoteEntry{}
	for _, it := range req.Src.Items {
		p := "/" + strings.Trim(it.Path, "/")
		parent, name := path.Dir(p), path.Base(p)
		ents, ok := cache[parent]
		if !ok {
			var err error
			if ents, err = j.list(parent); err != nil {
				return err
			}
			cache[parent] = ents
		}
		var e *remoteEntry
		for i := range ents {
			if ents[i].Name == name {
				e = &ents[i]
				break
			}
		}
		if e == nil {
			j.fail(name, errors.New("not found on source"))
			continue
		}
		if err := j.add(*e, p, filepath.Join(dstFull, name), 0); err != nil {
			return err
		}
	}
	return nil
}

func (j *rcopyJob) add(e remoteEntry, src, dst string, depth int) error {
	if j.t.cancel.Load() {
		return errCancelled
	}
	if e.Denied || e.Size < 0 && !e.Dir {
		j.denied++
		return nil
	}
	if !validName(e.Name) {
		j.fail(e.Name, errors.New("invalid name"))
		return nil
	}
	if !e.Dir {
		if len(j.files) >= rcopyMaxFiles {
			return fmt.Errorf("too many files (max %d per copy)", rcopyMaxFiles)
		}
		j.files = append(j.files, rcopyFile{src: src, dst: dst, size: e.Size, mtime: e.MTime})
		return nil
	}
	if depth > 64 {
		return errors.New("folders nested too deep")
	}
	j.dirs = append(j.dirs, dst)
	ents, err := j.list(src)
	if err != nil {
		if strings.Contains(err.Error(), "permission") || strings.Contains(err.Error(), "denied") {
			j.denied++
			return nil
		}
		return err
	}
	for _, c := range ents {
		if err := j.add(c, path.Join(src, c.Name), filepath.Join(dst, c.Name), depth+1); err != nil {
			return err
		}
	}
	return nil
}

// ---- file download

// permErr marks errors that retrying does not fix (HTTP 4xx, disk full, ...).
type permErr struct{ error }

func (e permErr) Unwrap() error { return e.error }

func (j *rcopyJob) copyOne(f rcopyFile, skipExisting bool) {
	name := path.Base(f.src)
	if fi, err := os.Stat(f.dst); err == nil {
		if fi.IsDir() {
			j.fail(name, errors.New("a folder with this name exists"))
			j.t.done.Add(f.size)
			return
		}
		if skipExisting {
			j.skipped++
			j.t.done.Add(f.size)
			return
		}
	}
	part := f.dst + rcopyPartInfix + partSuffix
	base := j.t.done.Load()
	err := j.download(f, part, base)
	if err == nil {
		if f.mtime > 0 {
			os.Chtimes(part, time.Now(), time.Unix(f.mtime, 0))
		}
		err = os.Rename(part, f.dst)
	}
	if err != nil {
		os.Remove(part)
		if !errors.Is(err, errCancelled) {
			j.fail(name, err)
			j.t.done.Store(base + f.size) // keep the progress bar consistent
		}
		return
	}
	j.copied++
}

func (j *rcopyJob) download(f rcopyFile, part string, base int64) error {
	out, err := os.OpenFile(part, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o644)
	if err != nil {
		return err
	}
	defer out.Close()
	var off int64
	etag := ""
	buf := make([]byte, 1<<20)
	for attempt := 0; ; attempt++ {
		err := j.fetch(f, out, &off, &etag, base, buf)
		var pe permErr
		if errors.As(err, &pe) {
			return pe.error
		}
		if err == nil || errors.Is(err, errCancelled) || attempt >= rcopyRetries {
			return err
		}
		time.Sleep(time.Duration(attempt+1) * 2 * time.Second)
		if j.t.cancel.Load() {
			return errCancelled
		}
	}
}

// fetch downloads from offset *off (Range + If-Range on retries) and appends to out.
func (j *rcopyJob) fetch(f rcopyFile, out *os.File, off *int64, etag *string, base int64, buf []byte) error {
	hdr := map[string]string{}
	if *off > 0 {
		hdr["Range"] = fmt.Sprintf("bytes=%d-", *off)
		if *etag != "" {
			hdr["If-Range"] = *etag
		}
	}
	resp, err := j.get(j.ctx, "/dl/"+strconv.Itoa(j.root)+escapeSegments(f.src), hdr)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	switch {
	case resp.StatusCode == http.StatusOK:
		if *off > 0 { // range not honoured (file changed): start over
			if _, err := out.Seek(0, io.SeekStart); err != nil {
				return err
			}
			if err := out.Truncate(0); err != nil {
				return err
			}
			*off = 0
		}
	case resp.StatusCode == http.StatusPartialContent && *off > 0:
	case resp.StatusCode == http.StatusUnauthorized:
		return permErr{errors.New("source server refused access")}
	default:
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		var d struct {
			Error string `json:"error"`
		}
		if json.Unmarshal(b, &d) == nil && d.Error != "" {
			return permErr{errors.New(d.Error)}
		}
		return fmt.Errorf("source server: HTTP %d %s", resp.StatusCode, bytes.TrimSpace(b))
	}
	if e := resp.Header.Get("ETag"); e != "" {
		*etag = e
	}
	for {
		if j.t.cancel.Load() {
			return errCancelled
		}
		n, rerr := resp.Body.Read(buf)
		if n > 0 {
			if _, err := out.Write(buf[:n]); err != nil {
				return permErr{err} // disk full etc.: retrying does not help
			}
			*off += int64(n)
			j.t.done.Store(base + *off)
		}
		if rerr == io.EOF {
			return nil
		}
		if rerr != nil {
			return rerr
		}
	}
}
