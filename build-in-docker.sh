#!/usr/bin/env bash

set -e
set -x

# Get to workdir
cd "$(realpath "$(dirname "$(realpath "${BASH_SOURCE[0]}")")")"

# 检查系统必须是 Debian 10 amd64
if [[ "$(uname -m)" != "x86_64" ]]; then
  echo "ERROR: requires amd64 (x86_64), got $(uname -m)" >&2
  exit 1
fi
if ! grep -q 'ID=debian' /etc/os-release 2>/dev/null || ! grep -q 'VERSION_ID="10"' /etc/os-release 2>/dev/null; then
  echo "ERROR: requires Debian 10, got $(. /etc/os-release 2>/dev/null && echo "$ID $VERSION_ID")" >&2
  exit 1
fi

(\
  echo "deb http://archive.debian.org/debian          buster         main contrib non-free" && \
  echo "deb http://archive.debian.org/debian          buster-updates main contrib non-free" && \
  echo "deb http://archive.debian.org/debian-security buster/updates main contrib non-free" && \
  true \
) | tee /etc/apt/sources.list

apt update -qq
apt install -qq -y sudo make curl git ca-certificates
git config --global --add safe.directory "$(pwd)"
git status

GO_VERSION="$(grep '^go ' go.mod | awk '{print $2}')"

(\
  cd /opt && \
  curl -fLO "https://go.dev/dl/go${GO_VERSION}.linux-amd64.tar.gz" && \
  tar -zxf "go${GO_VERSION}.linux-amd64.tar.gz" && \
  mv go/ "go${GO_VERSION}/" && \
  true \
)

export GOROOT="/opt/go${GO_VERSION}"
export PATH="${GOROOT}/bin:${PATH}"

make build-prepare-on-debian-amd64
ldd --version
make build-all-platform-linux
