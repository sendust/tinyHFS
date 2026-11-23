package main

// tinyhfs -update : replace this executable with the latest GitHub release.
//
// The release asset for this OS/arch is downloaded next to the executable, checked against
// SHA256SUMS.txt of the same release and then swapped in:
//   - Linux/macOS: renamed over the executable (a running server keeps its old copy)
//   - Windows:     a running .exe cannot be overwritten but can be renamed, so the old one
//                  becomes tinyhfs.exe.old (deleted on the next start)

import (
	"bufio"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"time"
)

// releaseTag is set by build.sh: -ldflags "-X main.releaseTag=v1.7.0" (git describe --tags).
var releaseTag = ""

const (
	updateAPI      = "https://api.github.com/repos/sendust/tinyHFS/releases/latest"
	updateMaxBytes = 200 << 20
)

var updateClient = &http.Client{Timeout: 15 * time.Minute}

func releaseAsset() (string, error) {
	switch runtime.GOOS + "/" + runtime.GOARCH {
	case "linux/amd64":
		return "tinyhfs-linux-amd64", nil
	case "linux/arm64":
		return "tinyhfs-linux-arm64", nil
	case "windows/amd64":
		return "tinyhfs-windows-amd64.exe", nil
	}
	return "", fmt.Errorf("no release build for %s/%s", runtime.GOOS, runtime.GOARCH)
}

// semver parses "v1.2.3" or "v1.2.3-4-gabcdef" (git describe: 4 commits after v1.2.3).
func semver(tag string) (v [3]int, after, ok bool) {
	t := strings.TrimPrefix(strings.TrimSpace(tag), "v")
	base, rest, _ := strings.Cut(t, "-")
	parts := strings.Split(base, ".")
	if len(parts) != 3 {
		return v, false, false
	}
	for i, p := range parts {
		n, err := strconv.Atoi(p)
		if err != nil {
			return v, false, false
		}
		v[i] = n
	}
	return v, rest != "", true
}

func cmpVer(a, b [3]int) int {
	for i := range a {
		if a[i] != b[i] {
			if a[i] < b[i] {
				return -1
			}
			return 1
		}
	}
	return 0
}

func httpGet(url string) (*http.Response, error) {
	req, err := http.NewRequest(http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("User-Agent", "tinyHFS-updater")
	resp, err := updateClient.Do(req)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode != 200 {
		resp.Body.Close()
		return nil, fmt.Errorf("%s: HTTP %d", url, resp.StatusCode)
	}
	return resp, nil
}

func selfUpdate() int {
	if err := doUpdate(); err != nil {
		fmt.Fprintln(os.Stderr, "update failed:", err)
		return 1
	}
	return 0
}

func doUpdate() error {
	asset, err := releaseAsset()
	if err != nil {
		return err
	}
	exe, err := os.Executable()
	if err != nil {
		return err
	}
	if p, err := filepath.EvalSymlinks(exe); err == nil {
		exe = p
	}
	cur := releaseTag
	if cur == "" {
		cur = "unknown"
	}
	fmt.Printf("current: %s  (%s)\n", cur, exe)

	resp, err := httpGet(updateAPI)
	if err != nil {
		return fmt.Errorf("cannot read the latest release: %v", err)
	}
	var rel struct {
		Tag    string `json:"tag_name"`
		URL    string `json:"html_url"`
		Assets []struct {
			Name string `json:"name"`
			URL  string `json:"browser_download_url"`
			Size int64  `json:"size"`
		} `json:"assets"`
	}
	err = json.NewDecoder(io.LimitReader(resp.Body, 4<<20)).Decode(&rel)
	resp.Body.Close()
	if err != nil {
		return fmt.Errorf("bad release info: %v", err)
	}
	fmt.Printf("latest:  %s  (%s)\n", rel.Tag, rel.URL)

	lv, _, lok := semver(rel.Tag)
	if cv, _, ok := semver(releaseTag); ok && lok && cmpVer(cv, lv) >= 0 {
		fmt.Println("already up to date") // same release, or a build made after it
		return nil
	}

	var binURL, sumURL string
	var binSize int64
	for _, a := range rel.Assets {
		switch a.Name {
		case asset:
			binURL, binSize = a.URL, a.Size
		case "SHA256SUMS.txt":
			sumURL = a.URL
		}
	}
	if binURL == "" {
		return fmt.Errorf("release %s has no %s", rel.Tag, asset)
	}
	if sumURL == "" {
		return fmt.Errorf("release %s has no SHA256SUMS.txt (refusing an unverified update)", rel.Tag)
	}
	want, err := releaseSum(sumURL, asset)
	if err != nil {
		return err
	}

	// download next to the executable so the final rename stays on the same file system
	mode := fs.FileMode(0o755)
	if fi, err := os.Stat(exe); err == nil {
		mode = fi.Mode().Perm() | 0o100
	}
	tmp := exe + ".new"
	f, err := os.OpenFile(tmp, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, mode)
	if err != nil {
		if errors.Is(err, fs.ErrPermission) {
			return fmt.Errorf("no permission to write in %s (run as a user that can, e.g. with sudo)", filepath.Dir(exe))
		}
		return err
	}
	ok := false
	defer func() {
		if !ok {
			os.Remove(tmp)
		}
	}()
	fmt.Printf("downloading %s (%s) ...\n", asset, fmtBytes(uint64(max(binSize, 0))))
	resp, err = httpGet(binURL)
	if err != nil {
		f.Close()
		return err
	}
	h := sha256.New()
	n, err := io.Copy(io.MultiWriter(f, h), io.LimitReader(resp.Body, updateMaxBytes))
	resp.Body.Close()
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		return fmt.Errorf("download: %v", err)
	}
	if binSize > 0 && n != binSize {
		return fmt.Errorf("download incomplete (%d of %d bytes)", n, binSize)
	}
	if got := hex.EncodeToString(h.Sum(nil)); got != want {
		return fmt.Errorf("SHA256 mismatch: got %s, expected %s", got, want)
	}
	fmt.Println("SHA256 ok")

	if runtime.GOOS == "windows" {
		old := exe + ".old"
		os.Remove(old)
		if err := os.Rename(exe, old); err != nil {
			return fmt.Errorf("cannot move the old executable aside: %v", err)
		}
		if err := os.Rename(tmp, exe); err != nil {
			os.Rename(old, exe)
			return fmt.Errorf("cannot install the new executable: %v", err)
		}
	} else if err := os.Rename(tmp, exe); err != nil {
		return fmt.Errorf("cannot replace the executable: %v", err)
	}
	ok = true
	fmt.Printf("updated %s -> %s. Restart tinyHFS to use the new version.\n", cur, rel.Tag)
	return nil
}

// releaseSum returns the expected sha256 of asset from SHA256SUMS.txt.
func releaseSum(url, asset string) (string, error) {
	resp, err := httpGet(url)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	sc := bufio.NewScanner(io.LimitReader(resp.Body, 1<<20))
	for sc.Scan() {
		f := strings.Fields(sc.Text())
		if len(f) == 2 && strings.TrimPrefix(f[1], "*") == asset && len(f[0]) == 64 {
			return strings.ToLower(f[0]), nil
		}
	}
	return "", fmt.Errorf("%s is not listed in SHA256SUMS.txt", asset)
}

// cleanupOldExe removes tinyhfs.exe.old left by an update on Windows.
func cleanupOldExe() {
	if runtime.GOOS != "windows" {
		return
	}
	if exe, err := os.Executable(); err == nil {
		os.Remove(exe + ".old")
	}
}
