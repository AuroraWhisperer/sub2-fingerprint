#!/bin/sh
set -eu
cd -- "$(dirname -- "$0")"
if [ "${1:-}" = "--migrate" ]; then
    shift
    exec /usr/bin/python3 -I -B ./update.py --migrate "$@"
fi
exec /usr/bin/python3 -I ./deploy.py install "$@"
