#!/bin/sh
# Creates the audit owner and writer roles (docs/internals/audit-storage.md#roles)
# and the audit schema owned by audit_owner. Postgres runs this once, on an empty
# data directory.
set -eu
psql -v ON_ERROR_STOP=1 --username "$POSTGRES_USER" --dbname "$POSTGRES_DB" \
  -v owner_password="$AUDIT_OWNER_PASSWORD" -v writer_password="$AUDIT_WRITER_PASSWORD" <<'SQL'
CREATE ROLE audit_owner LOGIN PASSWORD :'owner_password';
CREATE ROLE audit_writer LOGIN PASSWORD :'writer_password';
CREATE SCHEMA audit AUTHORIZATION audit_owner;
SQL
