#!/bin/sh
# SPDX-License-Identifier: GPL-3.0-only
# go mod vendor + apply patches/<module-path-with-dashes>-*.patch to the
# vendored modules. Build afterwards with: go build -mod=vendor ...
# (vendor/ is not committed.)
set -eu
cd "$(dirname "$0")/.."
rm -rf vendor
go mod vendor
for p in patches/*.patch; do
  [ -e "$p" ] || continue
  case "$(basename "$p")" in
    ims-go-*) dir=vendor/github.com/damonto/ims-go ;;
    wwan-go-*) dir=vendor/github.com/damonto/wwan-go ;;
    *) echo "vendor.sh: don't know which module $p is for" >&2; exit 1 ;;
  esac
  chmod -R u+w "$dir"
  patch -s -p1 -d "$dir" < "$p"
  echo "applied $p"
done
