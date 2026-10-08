import contextlib
import io
import json
from pathlib import Path
import tempfile
import unittest
from unittest import mock

import ltfs_compare
from release import GateError, digest


class LTFSCompareTest(unittest.TestCase):
    def setUp(self):
        temporary = tempfile.TemporaryDirectory(prefix="acp-ltfs-compare-test-")
        self.addCleanup(temporary.cleanup)
        self.root = Path(temporary.name)
        self.baseline = "4d5dbd0b8c06c5d53f369820ac9cff20e779821f"
        self.candidate = "a" * 40
        self.harness = "b" * 40
        self.adapter = (Path(__file__).resolve().parents[1] / "filesystem.go").read_text()
        self.arguments = ["ltfs_compare.py", "--baseline", self.baseline, "--candidate", self.candidate,
                          "--harness", self.harness, "--out", str(self.root / "out"), "--idle"]

    def snapshot(self, repo, revision, source):
        source.mkdir()
        # Only the reviewed historical dispatch boundaries are needed for this adapter fixture.
        files = {"acp.go": "type StreamCopyer struct {\n}\n",
                 "prepare.go": "openSourceContent(job.path, mode, job.stat.info)\n",
                 "copy.go": "c.prepareTarget(job, target)\n",
                 "target.go": "target targetSpec, out *fileio.Output, chunks\n"
                              "writeChunk(out.File, chunk)\n"
                              "c.refreshCacheEntry(out.File, target.name, job.baseJob, true)\n"
                              "restoreTarget(out.Temporary, job.stat)\n"
                              "out.File.Sync()\n"
                              "commitTarget(out, c.createFlag&os.O_TRUNC != 0)\n"}
        for name, text in files.items():
            (source / name).write_text(text)
        return revision

    def test_harness_is_required_before_any_collection(self):
        arguments = self.arguments.copy()
        start = arguments.index("--harness")
        del arguments[start:start + 2]
        with mock.patch("sys.argv", arguments), \
                mock.patch.object(ltfs_compare, "private_output") as output, contextlib.redirect_stderr(io.StringIO()):
            with self.assertRaises(SystemExit) as raised:
                ltfs_compare.main()
        self.assertEqual(raised.exception.code, 2)
        output.assert_not_called()

    def test_unreviewed_baseline_stops_before_source_access(self):
        arguments = self.arguments.copy()
        arguments[2] = "c" * 40
        with mock.patch("sys.argv", arguments), mock.patch.object(ltfs_compare, "commit") as commit:
            with self.assertRaisesRegex(GateError, "reviewed"):
                ltfs_compare.main()
        commit.assert_not_called()

    def test_adapter_refuses_missing_or_ambiguous_anchors(self):
        for replacement in ("", "type StreamCopyer struct {\ntype StreamCopyer struct {\n"):
            with self.subTest(replacement=replacement):
                source = self.root / str(len(replacement))
                self.snapshot(None, self.baseline, source)
                (source / "acp.go").write_text(replacement)
                with self.assertRaisesRegex(GateError, "anchor changed"):
                    ltfs_compare.adapt(source, self.adapter)

    def test_collection_uses_explicit_harness_and_scenario(self):
        native = {"bench_test.go": b"package acp\n", "release_helper_test.go": b"package acp\n"}
        ordinary = [profile + "/" + case + "/pipeline-4"
                    for profile in ("native47", "stress100") for case in ("small", "large", "mixed")]
        ordinary.append("stress100/small-delayed-source/pipeline-4")
        for scenario, names, selector in (
                ("ordinary", ordinary, ltfs_compare.SELECTOR),
                ("read-delay", ["stress100/medium-delayed-read/pipeline-4"],
                 "^BenchmarkLTFSPipeline$/^stress100$/^medium-delayed-read$/^pipeline$")):
            with self.subTest(scenario=scenario):
                output = self.root / scenario
                output.mkdir()
                arguments = [*self.arguments, "--scenario", scenario]
                commands = []

                def execute(command, **kwargs):
                    if command[:2] == ["git", "show"]:
                        return self.adapter.encode() if command[2].endswith(":filesystem.go") else b"package acp\n"
                    commands.append(command)
                    if "-bench=" + selector in command:
                        return ("\n".join("BenchmarkLTFSPipeline/" + name + " 1 100 ns/op" for name in names)
                                + "\nPASS\n").encode()
                    return b"PASS\n"

                with mock.patch("sys.argv", arguments), mock.patch.object(ltfs_compare, "commit") as commit, \
                        mock.patch.object(ltfs_compare, "private_output", return_value=output), \
                        mock.patch.object(ltfs_compare, "harness_files", return_value=native) as harness, \
                        mock.patch.object(ltfs_compare, "snapshot", side_effect=self.snapshot), \
                        mock.patch.object(ltfs_compare, "go_environment", side_effect=lambda *a, **k: ("go", {}, {})), \
                        mock.patch.object(ltfs_compare, "run", side_effect=execute), contextlib.redirect_stdout(io.StringIO()):
                    ltfs_compare.main()
                self.assertEqual([call.args[1] for call in commit.call_args_list],
                                 [self.baseline, self.candidate, self.harness])
                self.assertEqual(harness.call_args.args[1], self.harness)
                record = json.loads((output / "comparison.json").read_text())
                self.assertTrue(record["complete"])
                self.assertEqual(record["harness"], self.harness)
                self.assertEqual(record["harness_files"], {name: digest(data) for name, data in native.items()})
                self.assertEqual(len(record["runs"]), 2)
                self.assertEqual(sum("-bench=" + selector in command for command in commands), 2)
                patch = (output / (self.baseline[:12] + ".adapter.patch")).read_text()
                self.assertIn("out.Cache(job.baseJob, c.toDevice.linear)", patch)
                self.assertIn("o.copyer.refreshCacheEntry(o.output.File, o.target.name, job, true)", patch)
                self.assertNotIn("refreshCachePath", patch)
