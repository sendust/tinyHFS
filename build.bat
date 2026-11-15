@echo off
rem Build single executable on Windows. Requires Go >= 1.21.
rem Title date = git commit time (embedded automatically when built inside a git repo),
rem otherwise the executable's modification time.
cd /d "%~dp0"
if not exist dist mkdir dist
set CGO_ENABLED=0
set GOOS=windows& set GOARCH=amd64& go build -trimpath -ldflags "-s -w" -o dist\tinyhfs.exe . || exit /b 1
set GOOS=linux& set GOARCH=amd64& go build -trimpath -ldflags "-s -w" -o dist\tinyhfs . || exit /b 1
dir dist
