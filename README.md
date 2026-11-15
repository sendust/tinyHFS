# tinyHFS by sendust

**English** | [한국어](README.ko.md)

Tiny HTTP file server — a single executable for Windows and Linux with no dependencies (Go standard library only).

![tinyHFS web UI](tinyHFS_wwwUI.png)

## Usage

```
tinyhfs[.exe] [root_path1 root_path2 "path N" ...] [port]
```

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
```

## Features (web UI)

- Folder browsing under each root; free space of the current root shown at the top
- Upload with buttons (files / whole folder) or drag & drop (files and folders)
- **Resumable upload**: files are sent in 8 MB chunks. A broken upload leaves `name.tinyhfs-part`; upload the same file again (or press ▶) and it continues where it stopped
- Download by click, button, or dragging a row to the desktop (Chrome, Edge)
- **Resumable download**: HTTP Range / If-Range supported → resume from the browser's download manager, or with `curl -C - -O URL` / `wget -c URL`
- Folder zip download (selection or whole folder); compression: store / **fast (default)** / normal / best
- Create folders
- Transfer panel: size, %, speed, remaining time, cancel / resume

## Build

Requires Go ≥ 1.21 (https://go.dev/dl/).

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
| POST | `/api/mkdir?root=&path=&name=` | Create folder |
| GET | `/api/upload/status?root=&path=&name=&size=` | Resume offset of a partial upload |
| PUT | `/api/upload?root=&path=&name=&size=&offset=[&mtime=]` | Upload one chunk (request body) |
| GET | `/dl/<root>/<path>` | Download (Range supported) |
| GET | `/zip/<root>/<path>[?level=0-9&n=name...]` | Zip download |
| GET | `/api/progress?ids=` | Download progress |
| POST | `/api/cancel?tid=` | Cancel a download |

## Notes

- There is **no authentication**: anyone who can reach the port can browse, download and upload. Use it on trusted networks only.
- Drag-out download works only in Chromium browsers (Chrome, Edge).
- Zip downloads are generated on the fly, so they cannot be resumed.
