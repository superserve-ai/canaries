#!/usr/bin/env python3
"""Publish bounded read-only database inventory evidence for a resident run."""

import argparse
import hashlib
import json
import os
from pathlib import Path
import subprocess
import tempfile
import time
import uuid


QUERY = """
BEGIN READ ONLY;
SET LOCAL statement_timeout = '10s';
WITH owners AS MATERIALIZED (
  SELECT id,status,host_id FROM public.sandbox
  WHERE team_id = :'team_id'::uuid AND destroyed_at IS NULL
    AND metadata @> :'ownership'::jsonb
    AND CASE WHEN metadata->>'resident_slot' ~ '^[0-9]{1,6}$'
         THEN (metadata->>'resident_slot')::integer END < :population
  ORDER BY id LIMIT :row_limit
)
SELECT json_build_object(
  'observed_at',statement_timestamp(),
  'owners',COALESCE(json_agg(json_build_object('id',id,'status',status,'host_id',host_id)), '[]'::json)
) FROM owners;
ROLLBACK;
"""


def make_evidence(payload, preflight, worker, population):
    owners = payload["owners"]
    if not isinstance(owners, list) or len(owners) > population + 1:
        raise ValueError("database inventory exceeds the bounded population")
    ids = sorted(str(uuid.UUID(owner["id"])) for owner in owners)
    if len(set(ids)) != len(ids):
        raise ValueError("database inventory duplicates an owner")
    return {
        "source": "control-plane-sandbox-read",
        "observed_at": payload["observed_at"],
        "run_id": preflight["run_id"],
        "worker_id": worker,
        "team_id": str(uuid.UUID(preflight["team_id"])),
        "target": preflight["target"],
        "expected_host": preflight["expected_host"],
        "population": len(owners),
        "paused": sum(owner["status"] == "paused" for owner in owners),
        "wrong_host_count": sum(owner["host_id"] != preflight["expected_host"] for owner in owners),
        "ids_sha256": hashlib.sha256("\n".join(ids).encode()).hexdigest(),
    }


def write_evidence(path, evidence):
    path = Path(path)
    temporary = None
    try:
        with tempfile.NamedTemporaryFile(mode="w", encoding="utf-8", dir=path.parent, delete=False) as stream:
            temporary = stream.name
            json.dump(evidence, stream, separators=(",", ":"))
            stream.write("\n")
            stream.flush()
            os.fsync(stream.fileno())
        os.replace(temporary, path)
        temporary = None
        directory = os.open(path.parent, os.O_RDONLY)
        try:
            os.fsync(directory)
        finally:
            os.close(directory)
    finally:
        if temporary is not None:
            os.unlink(temporary)


def observe(preflight, worker, population):
    # Use operator-supplied libpq connection configuration. Credentials never
    # enter process arguments or the local evidence file.
    if not os.environ.get("PGSERVICE") and not (os.environ.get("PGHOST") and os.environ.get("PGDATABASE")):
        raise ValueError("set an explicit PGSERVICE or PGHOST and PGDATABASE for the isolated staging database")
    command = ["psql", "-X", "-q", "-t", "-A", "--no-password", "-v", "ON_ERROR_STOP=1"]
    ownership = {
        "managed_by": "superserve-test-runner", "test_type": "loadtest",
        "environment": "staging", "canary_target": preflight["target"],
        "run_id": preflight["run_id"], "worker_id": worker,
    }
    for key, value in {
        "team_id": str(uuid.UUID(preflight["team_id"])),
        "ownership": json.dumps(ownership, separators=(",", ":")),
        "population": str(population), "row_limit": str(population + 1),
    }.items():
        command.extend(["-v", f"{key}={value}"])
    result = subprocess.run(command, input=QUERY, capture_output=True, text=True, timeout=15)
    if result.returncode:
        # libpq errors can include connection details.
        raise RuntimeError("read-only inventory query failed; check the operator connection")
    if len(result.stdout) > 32 << 20:
        raise ValueError("database inventory exceeds response budget")
    return make_evidence(json.loads(result.stdout), preflight, worker, population)


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--preflight", required=True)
    parser.add_argument("--population", required=True, type=int)
    parser.add_argument("--output", required=True)
    parser.add_argument("--worker", default=os.environ.get("LOAD_TEST_WORKER_ID", "worker-0"))
    parser.add_argument("--interval", type=float, default=10,
                        help="seconds between queries (1..15; default: 10)")
    parser.add_argument("--once", action="store_true")
    args = parser.parse_args()
    # Evidence is stamped at query start. Leave room for both queries' bounded
    # runtimes and publishing overhead within the runner's one-minute limit.
    if not 1 <= args.population <= 210_000 or not 1 <= args.interval <= 15:
        parser.error("population must be 1..210000 and interval 1..15 seconds")
    if Path(args.preflight).resolve() == Path(args.output).resolve():
        parser.error("evidence output must differ from the preflight file")
    with open(args.preflight, encoding="utf-8") as stream:
        preflight = json.load(stream)
    if preflight["target"] != "staging-us-central1" or not preflight["expected_host"] or not preflight["run_id"]:
        parser.error("inventory observer requires an explicit isolated staging run")
    uuid.UUID(preflight["team_id"])
    while True:
        write_evidence(args.output, observe(preflight, args.worker, args.population))
        if args.once:
            return
        time.sleep(args.interval)


if __name__ == "__main__":
    main()
