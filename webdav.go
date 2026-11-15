package main

// WebDAV server under /webdav (golang.org/x/net/webdav).
//
//   /webdav/                 virtual top folder listing every root (read-only)
//   /webdav/<root name>/...  files of that root (read/write)
//
// A browser GET on /webdav shows a setup guide page; a GET on a sub folder
// redirects to the normal web UI at that folder.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"html"
	"io"
	"io/fs"
	"net/http"
	"os"
	"path"
	"strings"
	"time"

	"golang.org/x/net/webdav"
)

const davPrefix = "/webdav"

var (
	davNames   []string // unique folder name of each root under /webdav/
	davHandler *webdav.Handler
	davHTML    string // guide page template
)

func initWebDAV(host string) error {
	used := map[string]int{}
	davNames = davNames[:0]
	for _, r := range roots {
		n := r.Name
		if n == "" || strings.ContainsAny(n, `/\:`) {
			n = "root"
		}
		used[n]++
		if used[n] > 1 {
			n = fmt.Sprintf("%s_%d", n, used[n])
		}
		davNames = append(davNames, n)
	}
	davHandler = &webdav.Handler{
		Prefix:     davPrefix,
		FileSystem: multiFS{},
		LockSystem: webdav.NewMemLS(),
		Logger: func(r *http.Request, err error) {
			if err != nil && !errors.Is(err, fs.ErrNotExist) {
				flog.Printf("webdav %s %s: %v", r.Method, r.URL.Path, err)
			}
		},
	}
	b, err := webFS.ReadFile("web/webdav.html")
	if err != nil {
		return err
	}
	shares, _ := json.Marshal(davNames) // json escapes <, >, & -> safe inside <script>
	davHTML = strings.NewReplacer(
		"{{VERSION}}", html.EscapeString(version),
		"{{HOSTNAME}}", html.EscapeString(host),
		"{{AUTH}}", fmt.Sprint(authEnabled),
		"{{SHARES}}", string(shares),
	).Replace(string(b))
	return nil
}

// isDavGuidePath: the guide page is public (no secrets in it).
func isDavGuidePath(r *http.Request) bool {
	return (r.Method == http.MethodGet || r.Method == http.MethodHead) &&
		(r.URL.Path == davPrefix || r.URL.Path == davPrefix+"/")
}

func handleWebDAV(w http.ResponseWriter, r *http.Request) {
	if r.Method == http.MethodGet || r.Method == http.MethodHead {
		name := strings.TrimPrefix(r.URL.Path, davPrefix)
		if fi, err := (multiFS{}).Stat(r.Context(), name); err == nil && fi.IsDir() {
			idx, rest, top := davSplit(name)
			if top || idx < 0 {
				w.Header().Set("Content-Type", "text/html; charset=utf-8")
				w.Header().Set("Cache-Control", "no-store")
				io.WriteString(w, davHTML)
				return
			}
			// browser opened a WebDAV folder URL -> show it in the web UI
			http.Redirect(w, r, "/#/"+fmt.Sprint(idx)+escapeSegments(rest), http.StatusFound)
			return
		}
	}
	davHandler.ServeHTTP(w, r)
}

func escapeSegments(p string) string {
	segs := strings.Split(p, "/")
	for i, seg := range segs {
		segs[i] = rfc5987(seg) // percent-encode everything except unreserved chars
	}
	return strings.Join(segs, "/")
}

// davSplit maps "/name/rest" to (root index, "/rest"); top=true for "/".
func davSplit(name string) (idx int, rest string, top bool) {
	name = path.Clean("/" + name)
	if name == "/" {
		return -1, "/", true
	}
	first, r, _ := strings.Cut(name[1:], "/")
	for i, n := range davNames {
		if n == first {
			return i, "/" + r, false
		}
	}
	return -1, "", false
}

// ---------------------------------------------------------------- file system

type multiFS struct{}

func (multiFS) resolve(name string) (webdav.Dir, string, bool, error) {
	idx, rest, top := davSplit(name)
	if top {
		return "", "/", true, nil
	}
	if idx < 0 {
		return "", "", false, os.ErrNotExist
	}
	return webdav.Dir(roots[idx].Path), rest, false, nil
}

func (m multiFS) Mkdir(ctx context.Context, name string, perm os.FileMode) error {
	d, rest, top, err := m.resolve(name)
	if err != nil {
		return err
	}
	if top || rest == "/" {
		return os.ErrExist
	}
	return d.Mkdir(ctx, rest, perm)
}

func (m multiFS) OpenFile(ctx context.Context, name string, flag int, perm os.FileMode) (webdav.File, error) {
	d, rest, top, err := m.resolve(name)
	if err != nil {
		return nil, err
	}
	if top {
		if flag&(os.O_WRONLY|os.O_RDWR|os.O_CREATE|os.O_TRUNC) != 0 {
			return nil, os.ErrPermission
		}
		return &topDir{}, nil
	}
	return d.OpenFile(ctx, rest, flag, perm)
}

func (m multiFS) RemoveAll(ctx context.Context, name string) error {
	d, rest, top, err := m.resolve(name)
	if err != nil {
		return err
	}
	if top || rest == "/" {
		return os.ErrPermission // never delete a shared root itself
	}
	return d.RemoveAll(ctx, rest)
}

func (m multiFS) Rename(ctx context.Context, oldName, newName string) error {
	oi, orest, otop := davSplit(oldName)
	ni, nrest, ntop := davSplit(newName)
	if otop || ntop || oi < 0 || ni < 0 || orest == "/" || nrest == "/" {
		return os.ErrPermission
	}
	if oi != ni {
		return os.ErrPermission // moving between roots is not supported
	}
	return webdav.Dir(roots[oi].Path).Rename(ctx, orest, nrest)
}

func (m multiFS) Stat(ctx context.Context, name string) (os.FileInfo, error) {
	d, rest, top, err := m.resolve(name)
	if err != nil {
		return nil, err
	}
	if top {
		return dirInfo{name: "/", mt: time.Now()}, nil
	}
	return d.Stat(ctx, rest)
}

// virtual top folder: one sub folder per root
type topDir struct{ pos int }

func (t *topDir) Close() error                   { return nil }
func (t *topDir) Read([]byte) (int, error)       { return 0, errors.New("is a directory") }
func (t *topDir) Seek(int64, int) (int64, error) { return 0, nil }
func (t *topDir) Write([]byte) (int, error)      { return 0, os.ErrPermission }
func (t *topDir) Stat() (fs.FileInfo, error)     { return dirInfo{name: "/", mt: time.Now()}, nil }
func (t *topDir) Readdir(count int) ([]fs.FileInfo, error) {
	all := make([]fs.FileInfo, 0, len(davNames))
	for i, n := range davNames {
		mt := time.Now()
		if fi, err := os.Stat(roots[i].Path); err == nil {
			mt = fi.ModTime()
		}
		all = append(all, dirInfo{name: n, mt: mt})
	}
	if count <= 0 {
		rest := all[min(t.pos, len(all)):]
		t.pos = len(all)
		return rest, nil
	}
	if t.pos >= len(all) {
		return nil, io.EOF
	}
	end := min(t.pos+count, len(all))
	out := all[t.pos:end]
	t.pos = end
	return out, nil
}

type dirInfo struct {
	name string
	mt   time.Time
}

func (d dirInfo) Name() string       { return d.name }
func (d dirInfo) Size() int64        { return 0 }
func (d dirInfo) Mode() fs.FileMode  { return fs.ModeDir | 0o755 }
func (d dirInfo) ModTime() time.Time { return d.mt }
func (d dirInfo) IsDir() bool        { return true }
func (d dirInfo) Sys() any           { return nil }
