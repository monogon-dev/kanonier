#!/usr/bin/env bash
set -euo pipefail
DIR="$( cd "$( dirname "${BASH_SOURCE[0]}" )" >/dev/null 2>&1 && pwd )"

mutagen sync create "${DIR}" eq-ams-dev1.prod.monogon.dev:kanonier \
  --watch-polling-interval-alpha=1 \
  --default-file-mode-beta=0664 \
  --default-directory-mode-beta=0775

mutagen sync create "${DIR}" eq-ams-dev2.prod.monogon.dev:kanonier \
  --watch-polling-interval-alpha=1 \
  --default-file-mode-beta=0664 \
  --default-directory-mode-beta=0775
