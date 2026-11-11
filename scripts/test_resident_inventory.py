import hashlib
import io
import json
import os
from pathlib import Path
import tempfile
from types import SimpleNamespace
import unittest
from unittest.mock import patch

import resident_inventory as inventory


class InventoryEvidenceTest(unittest.TestCase):
    def setUp(self):
        self.preflight = {
            "run_id": "run-with-'quote", "team_id": "10000000-0000-4000-8000-000000000001",
            "target": "staging-us-central1", "expected_host": "isolated-host",
        }
        self.ids = ["00000000-0000-4000-8000-000000000002", "00000000-0000-4000-8000-000000000001"]

    def test_observation_reports_actual_placement_and_count_with_stable_uuid_hash(self):
        payload = {"observed_at": "2026-01-01T00:00:00Z", "owners": [
            {"id": self.ids[0].upper(), "status": "active", "host_id": "wrong-host"},
            {"id": self.ids[1], "status": "paused", "host_id": "isolated-host"},
        ]}
        evidence = inventory.make_evidence(payload, self.preflight, "worker-0", 105_000)
        self.assertEqual((evidence["population"], evidence["paused"], evidence["wrong_host_count"]), (2, 1, 1))
        self.assertEqual(evidence["ids_sha256"], hashlib.sha256("\n".join(sorted(self.ids)).encode()).hexdigest())

    def test_duplicate_or_oversized_database_inventory_is_rejected(self):
        owner = {"id": self.ids[0], "status": "paused", "host_id": "isolated-host"}
        for owners, population in [([owner, owner], 2), ([owner, owner, owner], 1)]:
            with self.assertRaises(ValueError):
                inventory.make_evidence({"observed_at": "now", "owners": owners}, self.preflight, "worker", population)

    def test_psql_uses_read_only_bounded_query_and_structured_ownership_parameter(self):
        payload = json.dumps({"observed_at": "2026-01-01T00:00:00Z", "owners": []})
        with patch.dict(os.environ, {"PGSERVICE": "operator-staging"}), patch.object(inventory.subprocess, "run", return_value=SimpleNamespace(returncode=0, stdout=payload)) as execute:
            evidence = inventory.observe(self.preflight, "worker", 105_000)
        args, kwargs = execute.call_args
        self.assertNotIn("shell", kwargs)
        self.assertEqual(kwargs["timeout"], 15)
        self.assertIn("BEGIN READ ONLY", kwargs["input"])
        self.assertIn("statement_timeout = '10s'", kwargs["input"])
        self.assertIn("team_id = :'team_id'::uuid", kwargs["input"])
        self.assertIn("metadata @> :'ownership'::jsonb", kwargs["input"])
        self.assertIn("row_limit=105001", args[0])
        ownership = next(value for value in args[0] if value.startswith("ownership="))
        self.assertEqual(json.loads(ownership.split("=", 1)[1])["run_id"], self.preflight["run_id"])
        self.assertEqual(evidence["population"], 0)

    def test_query_failure_does_not_publish_or_leak_connection_errors(self):
        with patch.dict(os.environ, {"PGSERVICE": "operator-staging"}), patch.object(inventory.subprocess, "run", return_value=SimpleNamespace(returncode=1, stdout="", stderr="sensitive connection detail")):
            with self.assertRaisesRegex(RuntimeError, "read-only inventory query failed") as raised:
                inventory.observe(self.preflight, "worker", 1)
        self.assertNotIn("sensitive", str(raised.exception))

    def test_atomic_output_replaces_complete_json_and_leaves_no_temporary_files(self):
        with tempfile.TemporaryDirectory() as directory:
            path = Path(directory) / "inventory.json"
            path.write_text("old evidence")
            inventory.write_evidence(path, {"population": 2})
            self.assertEqual(json.loads(path.read_text()), {"population": 2})
            self.assertEqual(list(Path(directory).iterdir()), [path])

    def test_observer_interval_reserves_freshness_headroom_for_query_time(self):
        with tempfile.TemporaryDirectory() as directory:
            preflight_path = Path(directory) / "preflight.json"
            output_path = Path(directory) / "inventory.json"
            preflight_path.write_text(json.dumps(self.preflight))
            command = ["resident_inventory.py", "--preflight", str(preflight_path),
                       "--population", "1", "--output", str(output_path),
                       "--worker", "worker-0", "--once"]
            for interval in ("15.01", "60"):
                with self.subTest(interval=interval), patch("sys.argv", command + ["--interval", interval]), \
                        patch("sys.stderr", new_callable=io.StringIO) as stderr, \
                        patch.object(inventory, "observe") as observe:
                    with self.assertRaises(SystemExit) as raised:
                        inventory.main()
                    self.assertEqual(raised.exception.code, 2)
                    self.assertIn("interval 1..15 seconds", stderr.getvalue())
                    observe.assert_not_called()
            with patch("sys.argv", command + ["--interval", "15"]), \
                    patch.object(inventory, "observe", return_value={"population": 1}) as observe:
                inventory.main()
            observe.assert_called_once_with(self.preflight, "worker-0", 1)
            self.assertEqual(json.loads(output_path.read_text()), {"population": 1})


if __name__ == "__main__":
    unittest.main()
