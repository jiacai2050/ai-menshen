#!/usr/bin/env python3
"""Add the request_logs.provider column to a database created by menshen v1.3.x or earlier.

Idempotent: safe to run more than once.

Usage: scripts/migrate_provider_column.py <path-to-sqlite-db>
"""

import sqlite3
import sys
from pathlib import Path


def main() -> int:
    if len(sys.argv) != 2:
        print(f"usage: {Path(sys.argv[0]).name} <path-to-sqlite-db>", file=sys.stderr)
        return 2

    db_path = Path(sys.argv[1])
    if not db_path.is_file():
        print(f"error: database not found: {db_path}", file=sys.stderr)
        return 1

    with sqlite3.connect(db_path) as conn:
        columns = {row[1] for row in conn.execute("PRAGMA table_info(request_logs)")}
        if not columns:
            print(f"error: no request_logs table in {db_path}", file=sys.stderr)
            return 1
        if "provider" in columns:
            print(f"request_logs.provider already exists in {db_path}, nothing to do")
            return 0
        conn.execute("ALTER TABLE request_logs ADD COLUMN provider TEXT NOT NULL DEFAULT ''")
        conn.commit()

    print(f"added request_logs.provider to {db_path}")
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
