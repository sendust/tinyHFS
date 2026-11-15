package main

// Password authentication (enabled with -auth <password> or env TINYHFS_AUTH).
//
//   - browsers: login page -> HttpOnly session cookie (SameSite=Lax)
//   - CLI tools: HTTP Basic auth with any user name, e.g. curl -u :PASSWORD
//   - brute force protection: 5 failures from one IP -> blocked for 5 minutes

import (
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"sync"
	"time"
)

const (
	sessionCookie = "tinyhfs_session"
	sessionTTL    = 7 * 24 * time.Hour
	maxFails      = 5
	blockTime     = 5 * time.Minute
)

var (
	authEnabled bool
	authHash    [32]byte
	loginHTML   []byte

	sessMu   sync.Mutex
	sessions = map[string]time.Time{} // token -> expiry

	failMu sync.Mutex
	fails  = map[string]*failInfo{} // ip -> failures
)

type failInfo struct {
	n     int
	until time.Time
	last  time.Time
}

// extractAuth removes "-auth <pw>" / "--auth <pw>" / "-auth=<pw>" from args.
func extractAuth(args []string) ([]string, string, error) {
	var out []string
	pw := ""
	for i := 0; i < len(args); i++ {
		a := args[i]
		switch {
		case a == "-auth" || a == "--auth":
			if i+1 >= len(args) || args[i+1] == "" {
				return nil, "", errors.New("-auth requires a password")
			}
			pw = args[i+1]
			i++
		case strings.HasPrefix(a, "-auth=") || strings.HasPrefix(a, "--auth="):
			pw = a[strings.Index(a, "=")+1:]
			if pw == "" {
				return nil, "", errors.New("-auth requires a password")
			}
		default:
			out = append(out, a)
		}
	}
	return out, pw, nil
}

func setPassword(pw string) {
	authEnabled = pw != ""
	authHash = sha256.Sum256([]byte(pw))
}

func ipBlocked(ip string) (bool, time.Duration) {
	failMu.Lock()
	defer failMu.Unlock()
	if f := fails[ip]; f != nil && time.Now().Before(f.until) {
		return true, time.Until(f.until)
	}
	return false, 0
}

// checkPassword compares in constant time and counts failures per IP.
func checkPassword(ip, pw string) bool {
	if b, _ := ipBlocked(ip); b {
		return false
	}
	h := sha256.Sum256([]byte(pw))
	ok := subtle.ConstantTimeCompare(h[:], authHash[:]) == 1
	failMu.Lock()
	defer failMu.Unlock()
	if ok {
		delete(fails, ip)
		return true
	}
	f := fails[ip]
	if f == nil {
		f = &failInfo{}
		fails[ip] = f
	}
	f.n++
	f.last = time.Now()
	if f.n >= maxFails {
		f.n = 0
		f.until = time.Now().Add(blockTime)
		logger.Printf("auth: %s blocked for %v after %d failed attempts", ip, blockTime, maxFails)
	} else {
		logger.Printf("auth: failed password from %s (%d/%d)", ip, f.n, maxFails)
	}
	return false
}

func newSession() string {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		panic(err)
	}
	tok := hex.EncodeToString(b)
	sessMu.Lock()
	sessions[tok] = time.Now().Add(sessionTTL)
	sessMu.Unlock()
	return tok
}

func validSession(tok string) bool {
	if tok == "" {
		return false
	}
	sessMu.Lock()
	defer sessMu.Unlock()
	exp, ok := sessions[tok]
	if ok && time.Now().After(exp) {
		delete(sessions, tok)
		return false
	}
	return ok
}

func authJanitor() {
	for range time.Tick(10 * time.Minute) {
		now := time.Now()
		sessMu.Lock()
		for k, exp := range sessions {
			if now.After(exp) {
				delete(sessions, k)
			}
		}
		sessMu.Unlock()
		failMu.Lock()
		for k, f := range fails {
			if now.After(f.until) && now.Sub(f.last) > time.Hour {
				delete(fails, k)
			}
		}
		failMu.Unlock()
	}
}

func isAuthed(r *http.Request) bool {
	if !authEnabled {
		return true
	}
	if c, err := r.Cookie(sessionCookie); err == nil && validSession(c.Value) {
		return true
	}
	if _, pw, ok := r.BasicAuth(); ok && checkPassword(clientIP(r), pw) {
		return true
	}
	return false
}

func authMW(next http.Handler) http.Handler {
	if !authEnabled {
		return next
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		p := r.URL.Path
		if p == "/login" || p == "/api/login" || p == "/api/logout" || p == "/favicon.ico" {
			next.ServeHTTP(w, r)
			return
		}
		if isAuthed(r) {
			next.ServeHTTP(w, r)
			return
		}
		switch {
		case p == "/":
			http.Redirect(w, r, "/login", http.StatusFound)
		case strings.HasPrefix(p, "/api/"):
			jsonErr(w, http.StatusUnauthorized, "login required")
		default: // /dl/, /zip/ : let curl/wget authenticate with Basic auth
			w.Header().Set("WWW-Authenticate", `Basic realm="tinyHFS", charset="UTF-8"`)
			http.Error(w, "401 unauthorized", http.StatusUnauthorized)
		}
	})
}

func handleLoginPage(w http.ResponseWriter, r *http.Request) {
	if !authEnabled || isAuthed(r) {
		http.Redirect(w, r, "/", http.StatusFound)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.Write(loginHTML)
}

func handleLogin(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		jsonErr(w, http.StatusMethodNotAllowed, "POST only")
		return
	}
	if !authEnabled {
		writeJSON(w, 200, map[string]any{"ok": true})
		return
	}
	ip := clientIP(r)
	if b, left := ipBlocked(ip); b {
		jsonErr(w, http.StatusTooManyRequests, "too many failed attempts, try again in "+left.Round(time.Second).String())
		return
	}
	var body struct {
		Password string `json:"password"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 4096)).Decode(&body); err != nil {
		jsonErr(w, http.StatusBadRequest, "bad request")
		return
	}
	if !checkPassword(ip, body.Password) {
		time.Sleep(700 * time.Millisecond) // slow down guessing
		jsonErr(w, http.StatusUnauthorized, "wrong password")
		return
	}
	http.SetCookie(w, &http.Cookie{
		Name: sessionCookie, Value: newSession(), Path: "/",
		MaxAge: int(sessionTTL / time.Second), HttpOnly: true, SameSite: http.SameSiteLaxMode,
	})
	logger.Printf("auth: login ok from %s", ip)
	writeJSON(w, 200, map[string]any{"ok": true})
}

func handleLogout(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		jsonErr(w, http.StatusMethodNotAllowed, "POST only")
		return
	}
	if c, err := r.Cookie(sessionCookie); err == nil {
		sessMu.Lock()
		delete(sessions, c.Value)
		sessMu.Unlock()
	}
	http.SetCookie(w, &http.Cookie{Name: sessionCookie, Value: "", Path: "/", MaxAge: -1, HttpOnly: true, SameSite: http.SameSiteLaxMode})
	writeJSON(w, 200, map[string]any{"ok": true})
}
