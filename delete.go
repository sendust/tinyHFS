package main

// Delete files / folders. Only available when password authentication is on
// (-auth / TINYHFS_AUTH): without a password anyone on the network could wipe the shares.
//
//   POST /api/delete   {"root":0, "path":"/dir", "names":["a.txt","sub","x/y.mp4"]}
//   names are relative to path (search results may contain '/'); folders are removed recursively.

import (
	"encoding/json"
	"errors"
	"io/fs"
	"mime"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
)

func handleDelete(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		jsonErr(w, http.StatusMethodNotAllowed, "POST only")
		return
	}
	if !authEnabled {
		jsonErr(w, http.StatusForbidden, "delete is only available when the server runs with -auth")
		return
	}
	// JSON body only: a cross-site HTML form cannot send it (CSRF), on top of SameSite cookies
	if mt, _, _ := mime.ParseMediaType(r.Header.Get("Content-Type")); mt != "application/json" {
		jsonErr(w, http.StatusUnsupportedMediaType, "application/json required")
		return
	}
	var req struct {
		Root  int      `json:"root"`
		Path  string   `json:"path"`
		Names []string `json:"names"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 4<<20)).Decode(&req); err != nil || len(req.Names) == 0 {
		jsonErr(w, http.StatusBadRequest, "bad request")
		return
	}
	full, _, clean, err := resolve(strconv.Itoa(req.Root), req.Path)
	if err != nil {
		pathErr(w, err)
		return
	}
	ip := clientIP(r)
	type failed struct {
		Name  string `json:"name"`
		Error string `json:"error"`
	}
	deleted := 0
	fails := []failed{}
	for _, n := range req.Names {
		if !validRelPath(n) { // no "", ".", "..", absolute paths: a root itself can never be deleted
			fails = append(fails, failed{n, "invalid name"})
			continue
		}
		target := filepath.Join(full, filepath.FromSlash(n))
		fi, err := os.Lstat(target)
		if err != nil {
			msg := "not found"
			if !errors.Is(err, fs.ErrNotExist) {
				msg = errText(err)
			}
			fails = append(fails, failed{n, msg})
			continue
		}
		kind := "file"
		if fi.IsDir() {
			kind = "folder"
			err = os.RemoveAll(target)
		} else {
			err = os.Remove(target)
			os.Remove(target + partSuffix) // unfinished upload of the same file, if any
		}
		if err != nil {
			logger.Printf("delete %s %s by %s FAILED: %v", kind, target, ip, err)
			fails = append(fails, failed{n, errText(err)})
			continue
		}
		deleted++
		logger.Printf("delete %s %s by %s", kind, target, ip)
	}
	status := http.StatusOK
	if deleted == 0 {
		status = http.StatusConflict
	}
	writeJSON(w, status, map[string]any{"path": clean, "deleted": deleted, "failed": fails,
		"error": func() string {
			if deleted == 0 && len(fails) > 0 {
				return fails[0].Name + ": " + fails[0].Error
			}
			return ""
		}()})
}

// errText: short message without the server's absolute path
func errText(err error) string {
	var pe *fs.PathError
	if errors.As(err, &pe) {
		return pe.Err.Error()
	}
	return err.Error()
}
