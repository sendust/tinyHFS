package main

// Link tokens: let an external player (mpv, VLC, ...) open a file dragged out of the page
// on a server running with -auth. The player has no login cookie, so the dragged URL
// carries ?xt=<exp>.<sig>:
//   - valid for GET/HEAD of that one file under /dl/ only (no listing, no other files,
//     no writes), signed with the per-process secret (a restart revokes every link)
//   - expires after linkTokenTTL (rounded up to the hour so listings stay cacheable/stable)
//   - masked in the access log
// Listings (/api/list, /api/search) carry "lt" per file only for logged-in clients with -auth.

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"net/http"
	"path"
	"strconv"
	"strings"
	"time"
)

const linkTokenTTL = 6 * time.Hour

func linkSig(root int, exp int64, clean string) string {
	m := hmac.New(sha256.New, xSecret)
	m.Write([]byte("link|" + strconv.Itoa(root) + "|" + strconv.FormatInt(exp, 10) + "|" + clean))
	return base64.RawURLEncoding.EncodeToString(m.Sum(nil)[:12])
}

// addLinkTokens fills entry.LT for the files of a listing of folder base (clean, slash path).
func addLinkTokens(r *http.Request, es []entry, rootIdx string, base string) {
	if !authEnabled || len(es) == 0 || !isAuthed(r) {
		return
	}
	root, err := strconv.Atoi(rootIdx)
	if err != nil {
		return
	}
	exp := (time.Now().Add(linkTokenTTL).Unix()/3600 + 1) * 3600
	pre := strconv.FormatInt(exp, 36) + "."
	for i := range es {
		if !es[i].Dir && !es[i].Denied {
			es[i].LT = pre + linkSig(root, exp, path.Join(base, es[i].Name))
		}
	}
}

// linkTokenAuthorized: GET/HEAD /dl/<root>/<path>?xt=... with a valid token for exactly that file.
func linkTokenAuthorized(r *http.Request) bool {
	if r.Method != http.MethodGet && r.Method != http.MethodHead || !strings.HasPrefix(r.URL.Path, "/dl/") {
		return false
	}
	tok := r.URL.Query().Get("xt")
	e36, sig, ok := strings.Cut(tok, ".")
	if !ok || len(tok) > 64 {
		return false
	}
	exp, err := strconv.ParseInt(e36, 36, 64)
	if err != nil || time.Now().Unix() > exp {
		return false
	}
	idx, rel := splitRootPath("/dl/", r.URL.Path)
	root, err := strconv.Atoi(idx)
	if err != nil {
		return false
	}
	_, _, clean, err := resolve(idx, rel)
	if err != nil {
		return false
	}
	return hmac.Equal([]byte(sig), []byte(linkSig(root, exp, clean)))
}

// maskLinkToken hides the value of xt= in a logged request URI.
func maskLinkToken(uri string) string {
	q := strings.IndexByte(uri, '?')
	if q < 0 {
		return uri
	}
	parts := strings.Split(uri[q+1:], "&")
	for i, p := range parts {
		if strings.HasPrefix(p, "xt=") {
			parts[i] = "xt=***"
		}
	}
	return uri[:q+1] + strings.Join(parts, "&")
}
