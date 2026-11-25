#!/bin/sh
# Release files for tag $1: static arm64 binary, complete source with vendored
# Go modules (builds offline with -mod=vendor), SHA256SUMS. Used by ci.yml.
set -eu
tag="$1"
src="dongie-ims-$tag"
work=$(mktemp -d)
mkdir -p dist "$work/$src"
CGO_ENABLED=0 GOOS=linux GOARCH=arm64 go build -trimpath -ldflags='-s -w' -o "dist/dongie-ims-$tag-aarch64-linux" .
git archive HEAD | tar -x -C "$work/$src"
(cd "$work/$src" && go mod vendor)
tar -C "$work" --sort=name --owner=0 --group=0 --numeric-owner -cJf "dist/$src.tar.xz" "$src"
rm -rf "$work"
(cd dist && sha256sum ./* > SHA256SUMS && cat SHA256SUMS)
