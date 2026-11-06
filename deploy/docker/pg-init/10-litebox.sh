#!/bin/sh
# 快捷 PG 模式的初始化(只在数据目录为空、第一次启动时跑):建应用账号与应用库。
# 应用账号不是超级用户、没有 CREATEDB;库归它所有,PUBLIC 的连接权限收回。
set -eu
app_pw="$(cat "$LITEBOX_APP_PASSWORD_FILE")"
psql -v ON_ERROR_STOP=1 --username "$POSTGRES_USER" --dbname postgres -v app_pw="$app_pw" <<'SQL'
CREATE ROLE litebox LOGIN PASSWORD :'app_pw' NOSUPERUSER NOCREATEDB NOCREATEROLE;
CREATE DATABASE litebox OWNER litebox;
REVOKE ALL ON DATABASE litebox FROM PUBLIC;
SQL
