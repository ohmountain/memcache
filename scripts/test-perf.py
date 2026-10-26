#!/usr/bin/env python3
"""Exercise performance CLI modes and quick suites with real Go processes."""

import json
import os
from pathlib import Path
import shutil
import subprocess
import sys
import tempfile
import unittest


ROOT = Path(__file__).resolve().parents[1]
# macOS ships Bash 3.2; do not let a newer Bash on PATH hide compatibility bugs.
BASH = "/bin/bash" if sys.platform == "darwin" else "bash"


class PerformanceToolsTest(unittest.TestCase):
    @classmethod
    def setUpClass(cls):
        cls.temp = tempfile.TemporaryDirectory(prefix="memcache-perf-test-")
        cls.addClassCleanup(cls.temp.cleanup)
        cls.directory = Path(cls.temp.name)
        cls.binary = cls.directory / "memcache.test"
        cls.env = dict(os.environ, GOMAXPROCS="4")
        cls.run_command(["go", "test", "-c", "-o", str(cls.binary), "."])

    @classmethod
    def run_command(cls, args, cwd=ROOT, success=True):
        result = subprocess.run(
            args, cwd=cwd, env=cls.env, text=True,
            stdout=subprocess.PIPE, stderr=subprocess.STDOUT, timeout=120,
        )
        if (result.returncode == 0) != success:
            raise AssertionError(f"{args!r} exited {result.returncode}:\n{result.stdout}")
        return result.stdout

    def check_load(self, record):
        load = record["load"]
        self.assertEqual(load["attempted"], load["planned"])
        self.assertEqual(load["completed"] + load["dropped"], load["planned"])
        self.assertEqual(load["unissued"], 0)
        self.assertEqual(load["all"]["samples"], load["completed"])

    def test_expiry_measurement_modes(self):
        # Skipping the wrong fixture changes registered_caches; leaking the
        # monitored load into a latency record changes its evidence fields.
        for mode, caches, reclaim in [("latency", 1, False), ("reclaim", 1, True), ("both", 2, True)]:
            with self.subTest(mode=mode):
                output = self.directory / f"expiry-{mode}.json"
                args = [
                    str(self.binary), "-test.run=^TestCachePerformance$",
                    "-cache-perf-case=expiry-burst", "-cache-perf-rate=1000",
                    "-cache-perf-duration=20ms", f"-cache-perf-out={output}",
                ]
                # An omitted flag must keep the original two-run behavior.
                if mode != "both":
                    args.append(f"-cache-perf-expiry-measure={mode}")
                self.run_command(args)
                record = json.loads(output.read_text())
                self.check_load(record)
                self.assertEqual(record["load"]["planned"], 0 if mode == "reclaim" else 20)
                self.assertEqual(record.get("expiry_measurement", "both"), mode)
                self.assertEqual(record["resources"]["registered_caches"], caches)
                self.assertEqual("reclaim_duration_ns" in record["resources"], reclaim)
                if mode == "both":
                    self.assertNotIn("expiry_measurement", record)
                if mode == "reclaim":
                    # Monitor latencies include snapshot interference and must
                    # not masquerade as the independent latency measurement.
                    self.assertEqual(record["load"]["all"]["samples"], 0)
                    self.assertEqual(record["load"]["all"]["p99"], 0)

        for case, mode in [("expiry-burst", "invalid"), ("steady", "reclaim")]:
            output = self.directory / f"invalid-{case}.json"
            self.run_command([
                str(self.binary), "-test.run=^TestCachePerformance$",
                f"-cache-perf-case={case}", f"-cache-perf-expiry-measure={mode}",
                f"-cache-perf-out={output}",
            ], success=False)
            self.assertFalse(output.exists())

    def check_quick_records(self, directory, expected):
        records = [json.loads(path.read_text()) for path in directory.glob("*.json")]
        self.assertEqual(len(records), expected)
        self.assertEqual({record["case"] for record in records}, {"noop", "steady", "expiry-burst"})
        for record in records:
            self.check_load(record)
            self.assertEqual(record["config"]["rate"], 10000)
            self.assertEqual(record["config"]["duration"], 1000000000)
            self.assertEqual(record["load"]["planned"], 10000)
            if record["case"] == "expiry-burst":
                self.assertEqual(record["expiry_measurement"], "latency")
                self.assertEqual(record["resources"]["registered_caches"], 1)
                self.assertNotIn("reclaim_duration_ns", record["resources"])

    def test_baseline_quick(self):
        output = self.directory / "quick"
        args = [BASH, "scripts/perf-baseline.sh", "--suite", "quick", "--out", str(output)]
        self.run_command(args)
        self.check_quick_records(output / "perf", 9)
        manifest = (output / "manifest.txt").read_text()
        self.assertIn("suite=quick\n", manifest)
        self.assertIn("repetitions=3\n", manifest)
        self.assertIn("acceptance_evidence=no\n", manifest)
        self.assertNotIn("command[profile-", manifest)
        self.run_command(args, success=False)
        self.assertEqual((output / "manifest.txt").read_text(), manifest)

    def test_compare_quick(self):
        # A temporary git repository tests the commit-export contract without
        # committing changes or creating refs in the developer's repository.
        fixture = self.directory / "repository"
        fixture.mkdir()
        for path in ROOT.glob("*.go"):
            shutil.copy2(path, fixture / path.name)
        shutil.copy2(ROOT / "go.mod", fixture / "go.mod")
        (fixture / "scripts").mkdir()
        shutil.copy2(ROOT / "scripts/perf-compare.sh", fixture / "scripts/perf-compare.sh")
        self.run_command(["git", "init", "-q"], cwd=fixture)
        self.run_command(["git", "add", "."], cwd=fixture)
        self.run_command([
            "git", "-c", "user.name=Performance Test", "-c", "user.email=perf@example.invalid",
            "-c", "core.hooksPath=/dev/null", "-c", "commit.gpgsign=false",
            "commit", "-qm", "test fixture",
        ], cwd=fixture)
        output = self.directory / "comparison"
        self.run_command([
            BASH, "scripts/perf-compare.sh", "--suite", "quick",
            "--control", "HEAD", "--candidate", "HEAD", "--harness", "HEAD",
            "--comparison-role", "incremental", "--out", str(output),
        ], cwd=fixture)
        for side in ["control", "candidate"]:
            self.check_quick_records(output / "results" / side, 9)
        manifest = (output / "manifest.txt").read_text()
        self.assertIn("repetitions=3\n", manifest)
        self.assertIn("acceptance_evidence=no\n", manifest)
        self.assertNotIn("command[profile-", manifest)
        commands = [line for line in manifest.splitlines() if line.startswith("command[perf-")]
        # Each group of three pairs must rotate AB, BA, AB.
        self.assertEqual(len(commands), 18)
        for index, line in enumerate(commands):
            expected = ["control", "candidate", "candidate", "control", "control", "candidate"][index % 6]
            self.assertIn(f"-{expected}]=", line)


if __name__ == "__main__":
    unittest.main(verbosity=2)
