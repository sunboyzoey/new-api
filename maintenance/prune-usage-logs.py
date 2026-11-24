#!/usr/bin/python3
"""Retain rolling 24h of consume/error usage logs; leave all other records intact."""
import json
import subprocess

def query(sql):
    return subprocess.check_output([
        '/usr/bin/docker', 'exec', 'new-api-postgres', 'psql',
        '-U', 'newapi', '-d', 'newapi', '-v', 'ON_ERROR_STOP=1', '-Atc', sql,
    ], text=True).strip()

cutoff = int(query("SELECT floor(extract(epoch FROM clock_timestamp() - interval '24 hours'))::bigint"))
print(json.dumps({'cutoff_epoch': cutoff, 'cutoff_utc':query(f"SELECT to_timestamp({cutoff}) AT TIME ZONE 'UTC'"), 'types':[2,5]}),flush=True)
total = 0
for batch in range(200):
    count = int(query(f"""
        SET lock_timeout = '3s';
        SET statement_timeout = '60s';
        WITH doomed AS (
            SELECT id FROM logs WHERE type IN (2,5) AND created_at < {cutoff}
            ORDER BY created_at, id LIMIT 10000
        ), removed AS (
            DELETE FROM logs USING doomed WHERE logs.id = doomed.id
            AND logs.type IN (2,5) AND logs.created_at < {cutoff}
            RETURNING logs.id
        ) SELECT count(*) FROM removed;
    """).splitlines()[-1])
    total += count
    if batch % 10 == 0 or count < 10000:
        print(json.dumps({'batch':batch+1,'deleted':total}),flush=True)
    if count < 10000:
        break
else:
    raise RuntimeError('Batch limit reached; next scheduled run will continue cleanup')
remaining=int(query(f"SELECT count(*) FROM logs WHERE type IN (2,5) AND created_at < {cutoff}"))
print(json.dumps({'deleted':total,'expired_remaining':remaining}),flush=True)
if remaining:
    raise RuntimeError('Expired usage logs remain')
