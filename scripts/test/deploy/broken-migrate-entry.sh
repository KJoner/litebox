#!/bin/sh
for a in "$@"; do
    if [ "$a" = migrate ]; then echo "注入:迁移失败" >&2; exit 1; fi
done
exec /usr/local/bin/litebox "$@"
