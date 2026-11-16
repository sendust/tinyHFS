# tinyHFS by sendust

**English** | [한국어](README.ko.md)

Tiny HTTP file server — a single executable for Windows and Linux with no dependencies (Go standard library only).

![tinyHFS web UI](tinyHFS_wwwUI.png)

## Download

Prebuilt binaries: **[Latest release](https://github.com/sendust/tinyHFS/releases/latest)**

| Platform | File |
|---|---|
| Windows x64 | [tinyhfs-windows-amd64.exe](https://github.com/sendust/tinyHFS/releases/latest/download/tinyhfs-windows-amd64.exe) |
| Linux x64 | [tinyhfs-linux-amd64](https://github.com/sendust/tinyHFS/releases/latest/download/tinyhfs-linux-amd64) |
| Linux ARM64 | [tinyhfs-linux-arm64](https://github.com/sendust/tinyHFS/releases/latest/download/tinyhfs-linux-arm64) |
| Checksums | [SHA256SUMS.txt](https://github.com/sendust/tinyHFS/releases/latest/download/SHA256SUMS.txt) |

On Linux:

```
wget https://github.com/sendust/tinyHFS/releases/latest/download/tinyhfs-linux-amd64
chmod +x tinyhfs-linux-amd64
./tinyhfs-linux-amd64
```

## Usage

```
tinyhfs[.exe] [-auth password] [root_path1 root_path2 "path N" ...] [port]
```

- `-auth password`: require a password (see [Authentication](#authentication)). Without it anyone on the network can access.
- Default root: the executable's folder. Default port: `10002`.
- Several root paths can be shared at once; switch between them with the buttons at the top of the page.
- If the port is busy, port+1, +2 … is tried.
- Serves on every NIC that is up and has an IPv4 address; the access URLs are printed to stdout.
- Logs are written to `./log/tinyhfs_YYYYMMDD.log`.

Examples:

```
tinyhfs                          # share the executable's folder on port 10002
tinyhfs /mnt /home/sendust 8080  # share two folders on port 8080
tinyhfs.exe D:\share "E:\My Files"
tinyhfs -auth MyPass123 /mnt     # password protected
```

## Features (web UI)

- Folder browsing under each root; free space of the current root shown at the top
- Upload with buttons (files / whole folder) or drag & drop (files and folders)
- **Resumable upload**: files are sent in 8 MB chunks. A broken upload leaves `name.tinyhfs-part`; upload the same file again (or press ▶) and it continues where it stopped
- Download by click, button, or dragging a row to the desktop (Chrome, Edge)
- **Resumable download**: HTTP Range / If-Range supported → resume from the browser's download manager, or with `curl -C - -O URL` / `wget -c URL`
- Folder zip download (selection or whole folder); compression: store / **fast (default)** / normal / best
- Create folders
- **File search**: searches the current folder and all subfolders by name. Case-insensitive; space separated words must all match (`report 2026`); wildcards `*` `?` match the whole name (`*.mp4`). Up to 1000 results / 20 s per search. Results can be downloaded, zipped (folder structure kept) or opened
- **Image preview**: clicking an image (jpg, png, gif, webp, bmp, svg, avif, ico) opens it in a right-side pane, scaled to fit, instead of downloading. ◀ ▶ / arrow keys browse images in the folder, Esc closes, drag the pane edge to resize
- **Password authentication** (`-auth`)
- **WebDAV server** at `/webdav/` — connect as a network drive (see [WebDAV](#webdav))
- Transfer panel: size, %, speed, remaining time, cancel / resume

## Authentication

```
tinyhfs -auth MyPass123 /mnt
```

- Browsers get a login page; after login a session cookie is kept for 7 days (until the server restarts). A **Logout** button appears at the top right.
- Command line tools use HTTP Basic auth with any user name:
  ```
  curl -u :MyPass123 -C - -O http://host:10002/dl/0/path/file
  wget --user=x --password=MyPass123 -c http://host:10002/dl/0/path/file
  ```
- 5 wrong passwords from one IP → that IP is blocked for 5 minutes. Login results are written to the log.
- The password on the command line is visible to other users of the host (`ps`). To avoid that, set the environment variable `TINYHFS_AUTH` instead of `-auth`.
- Traffic is plain HTTP (not encrypted). Use it on trusted networks or behind a VPN / HTTPS reverse proxy.

## WebDAV

Every shared folder is also served over WebDAV:

```
http://<address>:10002/webdav/            top folder (read-only), one sub folder per root
http://<address>:10002/webdav/<root name>/ files of that root (read/write)
```

Opening `/webdav/` in a web browser shows a **connection guide** (English / 한국어) with the real server address filled in:
how to map a network drive on Windows, macOS and Linux, and the limits of the built-in Windows WebDAV client with workarounds.
The 🔗 WebDAV button in the web UI opens the same page.

Windows quick start:

```
net use Z: http://<address>:10002/webdav/ /persistent:yes
```

Built-in Windows client limits (details and a fix script on the guide page):

| Limit | Workaround |
|---|---|
| `WebClient` service must run | `sc config WebClient start= auto` + `net start WebClient` |
| Password over plain HTTP refused (with `-auth`) | registry `BasicAuthLevel = 2` |
| Files > 50 MB fail | registry `FileSizeLimitInBytes = 0xFFFFFFFF` (max 4 GB; larger files: use the web UI or RaiDrive / WinSCP / rclone) |
| Big folders fail to list | registry `FileAttributesLimitInBytes = 50000000` |
| Slow | Internet Options → LAN settings → uncheck "Automatically detect settings" |

With `-auth`, WebDAV clients log in with any user name and the server password.
Moving files between different roots is not supported, and the roots themselves cannot be deleted or renamed.

## Build

Requires Go ≥ 1.26 (https://go.dev/dl/). Dependency: `golang.org/x/net/webdav` (fetched automatically by `go build`).

```
./build.sh        # Linux/macOS → dist/tinyhfs, dist/tinyhfs.exe, dist/tinyhfs-arm64
build.bat         # Windows     → dist\tinyhfs.exe, dist\tinyhfs
```

The page title is `tinyHFS by sendust <commit date/time>`. The date comes from the last git commit when built inside a git repository, otherwise the build time is used.

## HTTP API

| Method | Path | Description |
|---|---|---|
| GET | `/api/roots` | Roots with free/total bytes |
| GET | `/api/list?root=&path=` | Folder listing |
| GET | `/api/search?root=&path=&q=[&limit=]` | Recursive name search |
| POST | `/api/mkdir?root=&path=&name=` | Create folder |
| GET | `/api/upload/status?root=&path=&name=&size=` | Resume offset of a partial upload |
| PUT | `/api/upload?root=&path=&name=&size=&offset=[&mtime=]` | Upload one chunk (request body) |
| GET | `/dl/<root>/<path>` | Download (Range supported) |
| GET | `/zip/<root>/<path>[?level=0-9&n=name...]` | Zip download |
| GET | `/api/progress?ids=` | Download progress |
| POST | `/api/cancel?tid=` | Cancel a download |
| POST | `/api/login` (`{"password":"..."}`) / `/api/logout` | Login / logout (when `-auth` is used) |
| WebDAV | `/webdav/...` | WebDAV class 1/2 (PROPFIND, PUT, MKCOL, COPY, MOVE, DELETE, LOCK …) |

## Notes

- Without `-auth` there is **no authentication**: anyone who can reach the port can browse, download and upload.
- Drag-out download works only in Chromium browsers (Chrome, Edge).
- Zip downloads are generated on the fly, so they cannot be resumed.
