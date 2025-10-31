#!/bin/sh

set -eu

# Use "latest" if not specified, otherwise accept explicit tag like v0.3.1
VERSION=${VERSION:-latest}

os=$(uname -s)
arch=$(uname -m)
OS=${OS:-"${os}"}
ARCH=${ARCH:-"${arch}"}
OS_ARCH=""
BIN="crossplane-diff"

unsupported_arch() {
  local os=$1
  local arch=$2
  echo "❌ crossplane-diff does not support $os / $arch at this time."
  exit 1
}

# Detect OS/Architecture
case $OS in
  CYGWIN* | MINGW64* | Windows*)
    if [ "$ARCH" = "x86_64" ]; then
      OS_ARCH="windows_amd64.exe"
      BIN="crossplane-diff.exe"
    else
      unsupported_arch "$OS" "$ARCH"
    fi
    ;;
  Darwin)
    case $ARCH in
      x86_64|amd64)
        OS_ARCH="darwin_amd64"
        ;;
      arm64)
        OS_ARCH="darwin_arm64"
        ;;
      *)
        unsupported_arch "$OS" "$ARCH"
        ;;
    esac
    ;;
  Linux)
    case $ARCH in
      x86_64|amd64)
        OS_ARCH="linux_amd64"
        ;;
      arm64|aarch64)
        OS_ARCH="linux_arm64"
        ;;
      arm)
        OS_ARCH="linux_arm"
        ;;
      ppc64le)
        OS_ARCH="linux_ppc64le"
        ;;
      *)
        unsupported_arch "$OS" "$ARCH"
        ;;
    esac
    ;;
  *)
    unsupported_arch "$OS" "$ARCH"
    ;;
esac

# Choose correct URL pattern
if [ "$VERSION" = "latest" ]; then
  url="https://github.com/crossplane-contrib/crossplane-diff/releases/latest/download/crossplane-diff_${OS_ARCH}"
else
  url="https://github.com/crossplane-contrib/crossplane-diff/releases/download/${VERSION}/crossplane-diff_${OS_ARCH}"
fi

echo "📦 Downloading crossplane-diff (${VERSION}) for ${OS_ARCH}..."
echo "➡️  ${url}"
echo

# Download the binary
if ! curl -sfLo "${BIN}" "${url}"; then
  echo "❌ Failed to download crossplane-diff version '${VERSION}'."
  echo "Please check available versions at:"
  echo "  https://github.com/crossplane-contrib/crossplane-diff/releases"
  exit 1
fi

chmod +x "${BIN}"

echo
echo "✅ crossplane-diff downloaded successfully!"
echo
echo "To finish installation, run:"
echo "  sudo mv ${BIN} /usr/local/bin/"
echo "  crossplane-diff --help"
echo
echo "Visit https://github.com/crossplane-contrib/crossplane-diff for more info 🚀"
