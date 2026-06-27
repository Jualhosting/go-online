#!/bin/sh
set -e

OS="$(uname -s | tr '[:upper:]' '[:lower:]')"

if [ "$OS" = "darwin" ]; then
    URL="https://goinstant.my.id/downloads/goinstant-darwin"
elif [ "$OS" = "linux" ]; then
    URL="https://goinstant.my.id/downloads/goinstant-linux"
else
    echo "Unsupported OS: $OS"
    exit 1
fi

DEST_DIR="/usr/local/bin"
if [ ! -w "$DEST_DIR" ]; then
    DEST_DIR="$HOME/.local/bin"
fi
mkdir -p "$DEST_DIR"

echo "Downloading goinstant from $URL..."
curl -fsSL "$URL" -o "$DEST_DIR/goinstant"
chmod +x "$DEST_DIR/goinstant"

echo ""
echo "goinstant installed successfully at $DEST_DIR/goinstant!"
echo "You can now run:"
echo "  goinstant expose --port 8080"
echo "  goinstant deploy --dir ./folder-kamu"
echo ""
if ! echo "$PATH" | grep -q "$DEST_DIR"; then
    echo "WARNING: $DEST_DIR is not in your PATH. Please add it to your shell config (e.g. .bashrc or .zshrc) to run 'goinstant' from anywhere."
fi
