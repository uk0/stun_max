#!/bin/bash
set -e

VERSION=${1:-"dev"}
BUILD_DIR="build"
rm -rf "$BUILD_DIR"
mkdir -p "$BUILD_DIR"

echo "Building STUN Max $VERSION ..."

# Server
echo "  server (linux/amd64)"
GOOS=linux GOARCH=amd64 CGO_ENABLED=0 go build -ldflags="-s -w" -o "$BUILD_DIR/stun_max-server-linux-amd64" ./server/

# STUN Server
echo "  stunserver (linux/amd64)"
GOOS=linux GOARCH=amd64 CGO_ENABLED=0 go build -ldflags="-s -w" -o "$BUILD_DIR/stun_max-stunserver-linux-amd64" ./tools/stunserver/

# NAT Check
echo "  natcheck (darwin/arm64, windows/amd64, linux/amd64)"
GOOS=darwin GOARCH=arm64 go build -ldflags="-s -w" -o "$BUILD_DIR/natcheck-darwin-arm64" ./tools/natcheck/
GOOS=windows GOARCH=amd64 CGO_ENABLED=0 go build -ldflags="-s -w" -o "$BUILD_DIR/natcheck-windows-amd64.exe" ./tools/natcheck/
GOOS=linux GOARCH=amd64 CGO_ENABLED=0 go build -ldflags="-s -w" -o "$BUILD_DIR/natcheck-linux-amd64" ./tools/natcheck/

# GUI Client
echo "  gui-client (darwin/arm64, windows/amd64)"
GOOS=darwin GOARCH=arm64 go build -ldflags="-s -w" -o "$BUILD_DIR/stun_max-client-darwin-arm64" ./client/
GOOS=windows GOARCH=amd64 CGO_ENABLED=0 go build -ldflags="-s -w -H windowsgui" -o "$BUILD_DIR/stun_max-client-windows-amd64.exe" ./client/

# CLI Client
echo "  cli-client (darwin/arm64, windows/amd64, linux/amd64)"
GOOS=darwin GOARCH=arm64 go build -tags cli -ldflags="-s -w" -o "$BUILD_DIR/stun_max-cli-darwin-arm64" ./client/
GOOS=windows GOARCH=amd64 CGO_ENABLED=0 go build -tags cli -ldflags="-s -w" -o "$BUILD_DIR/stun_max-cli-windows-amd64.exe" ./client/
GOOS=linux GOARCH=amd64 CGO_ENABLED=0 go build -tags cli -ldflags="-s -w" -o "$BUILD_DIR/stun_max-cli-linux-amd64" ./client/

# Web assets
echo "  web assets"
cp -r web "$BUILD_DIR/web"

echo ""
echo "Build complete:"
ls -lh "$BUILD_DIR"/ | grep -v "^total"
