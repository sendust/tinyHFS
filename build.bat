@echo off
rem Build single executable on Windows. Requires Go >= 1.21.
rem Title date = git commit time (embedded automatically when built inside a git repo),
rem otherwise the executable's modification time.
rem Icon: if go-winres is installed it regenerates rsrc_windows_*.syso from winres\icon.png,
rem otherwise the committed .syso files are used.
cd /d "%~dp0"
if not exist dist mkdir dist
where go-winres >nul 2>nul && go-winres simply --arch amd64,arm64 --icon winres\icon.png --manifest cli --product-name tinyHFS --file-description "tinyHFS - tiny HTTP file server" --original-filename tinyhfs.exe --copyright sendust
set CGO_ENABLED=0
set GOOS=windows& set GOARCH=amd64& go build -trimpath -ldflags "-s -w" -o dist\tinyhfs.exe . || exit /b 1
set GOOS=linux& set GOARCH=amd64& go build -trimpath -ldflags "-s -w" -o dist\tinyhfs . || exit /b 1
dir dist
