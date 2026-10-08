import contextlib
import io
import json
import os
from pathlib import Path
import subprocess
import tarfile
import tempfile
import unittest
from unittest import mock
from types import SimpleNamespace

import release


def benchmark_text(time=100, memory=100, allocations=10, cpu=4, iterations=100,
                   benchmarks=release.BENCHMARKS):
    text = "goos: linux\ngoarch: amd64\npkg: " + release.MODULE + "\ncpu: Fixture CPU\n"
    suffix = "" if cpu == 1 else f"-{cpu}"
    for name in benchmarks:
        throughput = " 640.00 MB/s" if "Read" in name or "Copy" in name else ""
        text += f"{name}{suffix}\t{iterations}\t{time} ns/op{throughput}\t{memory} B/op\t{allocations} allocs/op\n"
    return text + "PASS\nok  \t" + release.MODULE + "\t12.345s\n"


def archive_bytes(files):
    data = io.BytesIO()
    with tarfile.open(fileobj=data, mode="w:gz") as archive:
        for name, value in files.items():
            entry = tarfile.TarInfo(name)
            entry.size = len(value)
            archive.addfile(entry, io.BytesIO(value))
    return data.getvalue()


class RepositoryFixture:
    def setUp(self):
        self.temporary = tempfile.TemporaryDirectory(prefix="acp-release-test-")
        self.addCleanup(self.temporary.cleanup)
        self.root = Path(self.temporary.name)
        self.repo = self.root / "repo"
        self.repo.mkdir()
        self.git("init", "-b", "main")
        self.git("config", "user.name", "Release Fixture")
        self.git("config", "user.email", "release@example.invalid")
        self.git("config", "commit.gpgsign", "false")
        self.write("go.mod", "module " + release.MODULE + "\n\ngo 1.26.8\n")
        self.write("README.md", "Fixture\n")
        self.write("LICENSE", "Fixture license\n")
        self.write("bench_test.go", "package acp\n")
        self.write("workload_bench_test.go", "package acp\n")
        self.write("item_test.go", "package acp\nfunc runStream() {\n}\n")
        (self.repo / "dev").mkdir()
        self.write("dev/release.py", "# Reviewed collector fixture\n")
        self.base = self.commit()
        self.write("source.go", "package acp\n")
        self.candidate = self.commit()
        self.config = {"candidate": self.candidate, "baseline": self.base, "harness": self.base,
                       "public_base": self.base, "history_tip": self.candidate, "version": "v0.2.3",
                       "artifacts": [], "performance": str(self.root / "pair")}

    def git(self, *args):
        return release.git(self.repo, *args)

    def write(self, name, text):
        (self.repo / name).write_text(text)

    def commit(self, message="Fixture commit"):
        self.git("add", ".")
        self.git("commit", "--allow-empty", "-m", message)
        return self.git("rev-parse", "HEAD")

    def evidence(self, candidate=None, cpu=4, baseline=None):
        directory = self.root / "pair"
        directory.mkdir(exist_ok=True)
        (directory / "baseline.bench").write_text(benchmark_text(cpu=cpu) if baseline is None else baseline)
        (directory / "candidate.bench").write_text(benchmark_text(cpu=cpu) if candidate is None else candidate)
        record = {"complete": True, "idle_confirmed": True, "baseline": self.base, "candidate": self.candidate,
                  "harness": self.base, "collector_sha256": release.digest(Path(release.__file__).read_bytes()),
                  "harness_files": {name: release.digest(data) for name, data in release.harness_files(self.repo, self.base).items()},
                  "cpu": cpu, "command": release.benchmark_args(cpu),
                  "environment": {"label": "synthetic test only", "filesystem": {"device": 1}, "cgo": "0",
                                  "GOGC": "100", "GOMEMLIMIT": "off",
                                  "go": {"GOVERSION": "go1.26.8", "GOOS": "linux", "GOARCH": "amd64"}},
                  "trees": {side: self.git("rev-parse", self.config[side] + "^{tree}") for side in release.SIDES},
                  "outputs": {side: release.digest((directory / (side + ".bench")).read_bytes()) for side in release.SIDES}}
        release.save(directory / "pair.json", record)
        return directory, record


class RepositoryTest(RepositoryFixture, unittest.TestCase):
    def test_release_requires_clean_exact_single_unpublished_commit(self):
        release.release_inputs(self.repo, self.config)
        self.write("untracked", "not a release input")
        with self.assertRaisesRegex(release.GateError, "clean"):
            release.release_inputs(self.repo, self.config)
        (self.repo / "untracked").unlink()
        self.write("second", "next unpublished change")
        self.config["candidate"] = self.config["history_tip"] = self.commit()
        with self.assertRaisesRegex(release.GateError, "Squash"):
            release.release_inputs(self.repo, self.config)

    def test_release_requires_explicit_baseline_and_artifact_declaration(self):
        for key in ("baseline", "harness", "artifacts", "performance"):
            config = dict(self.config)
            del config[key]
            with self.subTest(key=key), self.assertRaises(release.GateError):
                release.release_inputs(self.repo, config)

    def test_performance_sources_cannot_use_workstation_module_replacements(self):
        release.module_text(self.repo)
        self.write("go.mod", "module " + release.MODULE + "\n\ngo 1.26.8\nreplace example.org/dep => ../local\n")
        with self.assertRaisesRegex(release.GateError, "replacements"):
            release.module_text(self.repo)

    def test_history_tree_must_match_candidate(self):
        self.config["history_tip"] = self.base
        with self.assertRaisesRegex(release.GateError, "trees differ"):
            release.release_inputs(self.repo, self.config)

    def test_retained_history_needs_its_exact_local_backup(self):
        tree = self.git("rev-parse", self.candidate + "^{tree}")
        history = self.git("commit-tree", tree, "-p", self.base, "-m", "Retained fixture history")
        self.config["history_tip"] = history
        with self.assertRaisesRegex(release.GateError, "backup branch"):
            release.release_inputs(self.repo, self.config)
        self.config["backup_ref"] = "refs/heads/local-backup"
        self.git("update-ref", self.config["backup_ref"], history)
        release.release_inputs(self.repo, self.config)

    def test_snapshot_ignores_workstation_files_but_rejects_export_omissions(self):
        self.write(".gitignore", "workstation\n")
        self.commit()
        self.write("workstation", "private local data")
        release.snapshot(self.repo, self.candidate, self.root / "snapshot")
        self.assertFalse((self.root / "snapshot" / "workstation").exists())
        self.write(".gitattributes", "source.go export-ignore\n")
        revision = self.commit()
        with self.assertRaisesRegex(release.GateError, "complete committed tree"):
            release.snapshot(self.repo, revision, self.root / "omitted")

    def test_snapshot_rejects_links(self):
        (self.repo / "link").symlink_to("source.go")
        revision = self.commit()
        with self.assertRaises(release.GateError):
            release.snapshot(self.repo, revision, self.root / "snapshot")

    def test_explicit_public_tip_is_rechecked_and_tags_are_never_reused(self):
        remote = self.root / "remote.git"
        release.run(["git", "clone", "--bare", self.repo, remote])
        self.git("remote", "add", "origin", str(remote))
        release.git(remote, "update-ref", "refs/heads/main", self.base)
        self.git("update-ref", "refs/remotes/origin/main", self.base)
        release.public_tip(self.repo, self.config)
        release.git(remote, "update-ref", "refs/heads/main", self.candidate)
        with self.assertRaisesRegex(release.GateError, "Public main moved"):
            release.public_tip(self.repo, self.config)
        release.git(remote, "update-ref", "refs/heads/main", self.base)
        release.git(remote, "update-ref", "refs/tags/v0.2.3", self.base)
        with self.assertRaisesRegex(release.GateError, "already public"):
            release.public_tip(self.repo, self.config)

    def test_performance_reports_raw_values_and_flags_observed_increases(self):
        for changes, flagged in (({"time": 90}, False), ({"time": 110}, False), ({"time": 111}, True),
                                 ({"memory": 111}, True), ({"allocations": 12}, True)):
            with self.subTest(changes=changes), contextlib.redirect_stdout(io.StringIO()) as stdout:
                directory, _ = self.evidence(benchmark_text(**changes))
                comparison = release.performance_check(self.repo, directory, self.base, self.candidate, self.base)
                result = json.loads((directory / "comparison.json").read_text())
                self.assertEqual(comparison, result)
                self.assertEqual(result["flagged"], flagged)
                self.assertEqual(len(result["metrics"]), len(release.BENCHMARKS) * len(release.METRICS))
                row = result["metrics"][0]
                self.assertEqual(row["baseline"], 100)
                self.assertEqual(row["candidate"], changes.get("time", 100))
                self.assertAlmostEqual(row["delta_percent"], changes.get("time", 100) - 100)
                self.assertEqual(sum(row["flagged"] for row in result["metrics"]), len(release.BENCHMARKS) if flagged else 0)
                for row in result["metrics"]:
                    self.assertIn(row["benchmark"] + "\t" + row["metric"], stdout.getvalue())

    def test_zero_baseline_reports_undefined_percent_for_an_increase(self):
        for value in (0, 1):
            with self.subTest(allocations=value), contextlib.redirect_stdout(io.StringIO()) as stdout:
                directory, _ = self.evidence(benchmark_text(allocations=value), baseline=benchmark_text(allocations=0))
                release.performance_check(self.repo, directory, self.base, self.candidate, self.base)
                if value:
                    self.assertIn("n/a (zero baseline)", stdout.getvalue())
                result = json.loads((directory / "comparison.json").read_text())
                row = result["metrics"][2]
                self.assertEqual(row["baseline"], 0)
                self.assertEqual(row["candidate"], value)
                self.assertEqual(row["delta_percent"], None if value else 0)
                self.assertEqual(row["flagged"], bool(value))

    def test_performance_requires_bound_source_harness_environment_and_outputs(self):
        changes = {"baseline": self.candidate, "candidate": self.base, "harness": self.candidate,
                   "harness_files": {}, "complete": False, "idle_confirmed": False,
                   "outputs": {}, "trees": {}, "collector_sha256": "0" * 64, "cpu": 0,
                   "command": ["test", "-bench=.", "-benchtime=1x"], "environment": {}}
        for key, value in changes.items():
            with self.subTest(key=key):
                directory, record = self.evidence()
                record[key] = value
                release.save(directory / "pair.json", record)
                with self.assertRaises(release.GateError):
                    release.performance_check(self.repo, directory, self.base, self.candidate, self.base)
        for side in release.SIDES:
            directory, _ = self.evidence()
            path = directory / (side + ".bench")
            path.write_text(path.read_text().replace("100 ns/op", "99 ns/op", 1))
            with self.subTest(side=side), self.assertRaisesRegex(release.GateError, "checksum mismatch"):
                release.performance_check(self.repo, directory, self.base, self.candidate, self.base)
        directory, _ = self.evidence(benchmark_text().replace("Fixture CPU", "Different CPU"))
        with self.assertRaisesRegex(release.GateError, "environments differ"):
            release.performance_check(self.repo, directory, self.base, self.candidate, self.base)

    def test_go_calibrates_iterations_independently(self):
        for cpu in (1, 4):
            with self.subTest(cpu=cpu), contextlib.redirect_stdout(io.StringIO()):
                directory, _ = self.evidence(benchmark_text(cpu=cpu, iterations=1234), cpu=cpu)
                result = release.performance_check(self.repo, directory, self.base, self.candidate, self.base)
                self.assertFalse(result["flagged"])
                self.assertEqual(len(result["metrics"]), len(release.BENCHMARKS) * len(release.METRICS))

    def test_documentation_and_checker_changes_retain_original_measurements_and_source_identity(self):
        directory, record = self.evidence()
        record["collector_sha256"] = release.digest((self.repo / "dev/release.py").read_bytes())
        release.save(directory / "pair.json", record)
        original = (directory / "pair.json").read_bytes()
        self.write("dev/release.py", "# Updated comparison reporting\n")
        self.write("TESTING.md", "Review measurements once.\n")
        self.write("README.md", "Install the published version.\n")
        self.write("LICENSE", "Project license\n")
        (self.repo / "mmap").mkdir()
        self.write("mmap/LICENSE", "Upstream license\n")
        candidate = self.commit()
        with contextlib.redirect_stdout(io.StringIO()):
            result = release.performance_check(self.repo, directory, self.base, candidate, self.base)
        self.assertEqual(result["candidate"], candidate)
        self.assertEqual(result["measured_candidate"], self.candidate)
        self.assertEqual((directory / "pair.json").read_bytes(), original)
        self.write("go.mod", (self.repo / "go.mod").read_text() + "\nrequire example.invalid/module v1.0.0\n")
        changed = self.commit()
        with self.assertRaisesRegex(release.GateError, "Performance inputs differ"):
            release.performance_check(self.repo, directory, self.base, changed, self.base)

    def test_collector_runs_go_test_once_per_source_and_keeps_complete_flagged_evidence(self):
        for time in (100, 111):
            args = SimpleNamespace(idle=True, cpu=1, baseline=self.base, candidate=self.candidate,
                                   harness=self.base, out=str(self.root / f"collected-{time}"),
                                   environment="synthetic collector test")
            execute = release.run
            calls = []
            outputs = {"baseline": benchmark_text(cpu=1, iterations=100).encode(),
                       "candidate": benchmark_text(cpu=1, time=time, iterations=1234).encode()}

            def command(arguments, **kwargs):
                if arguments[0] != "fixture-go":
                    return execute(arguments, **kwargs)
                self.assertEqual(arguments[1], "test")
                for flag in ("-run=^$", "-benchtime=1s", "-count=1", "-benchmem", "-cpu=1"):
                    self.assertIn(flag, arguments)
                selector = next(arg for arg in arguments if arg.startswith("-bench="))
                self.assertEqual(selector, "-bench=^(BenchmarkReadBuffered|BenchmarkReadMapped|"
                                 "BenchmarkRefreshSignatureUnchanged|BenchmarkRefreshSignatureChanged|BenchmarkCopyWorkload)$")
                side = kwargs["cwd"].name
                calls.append(side)
                return outputs[side]

            info = {"GOVERSION": "go1.26.8", "GOOS": "linux", "GOARCH": "amd64"}
            with self.subTest(time=time), mock.patch.object(release, "go_environment", return_value=("fixture-go", {}, info)), \
                    mock.patch.object(release, "run", side_effect=command), \
                    mock.patch.object(release.os, "getloadavg", side_effect=[(0, 0, 0), (100, 100, 100), (100, 100, 100)]), \
                    contextlib.redirect_stdout(io.StringIO()):
                release.collect(self.repo, args)
            self.assertEqual(calls, ["baseline", "candidate"])
            record = json.loads((Path(args.out) / "pair.json").read_text())
            self.assertTrue(record["complete"])
            self.assertEqual(record["baseline"], self.base)
            self.assertEqual(record["initial_load"], [0, 0, 0])
            self.assertEqual(record["runs"], [{"side": side, "load": [100, 100, 100]} for side in release.SIDES])
            for side in release.SIDES:
                self.assertEqual((Path(args.out) / (side + ".bench")).read_bytes(), outputs[side])
            result = json.loads((Path(args.out) / "comparison.json").read_text())
            self.assertEqual(result["flagged"], time == 111)
            self.assertEqual(len(result["metrics"]), len(release.BENCHMARKS) * len(release.METRICS))

    def test_collector_keeps_incomplete_output_unaccepted(self):
        args = SimpleNamespace(idle=True, cpu=4, baseline=self.base, candidate=self.candidate,
                               harness=self.base, out=str(self.root / "collected"), environment="synthetic test")
        execute = release.run

        def command(arguments, **kwargs):
            if arguments[0] == "fixture-go":
                return benchmark_text(benchmarks=release.BENCHMARKS[:-1]).encode()
            return execute(arguments, **kwargs)

        info = {"GOVERSION": "go1.26.8", "GOOS": "linux", "GOARCH": "amd64"}
        with mock.patch.object(release, "go_environment", return_value=("fixture-go", {}, info)), \
                mock.patch.object(release, "run", side_effect=command), \
                mock.patch.object(release.os, "getloadavg", return_value=(0, 0, 0)):
            with self.assertRaisesRegex(release.GateError, "Incomplete benchmark inventory"):
                release.collect(self.repo, args)
        record = json.loads((Path(args.out) / "pair.json").read_text())
        self.assertFalse(record["complete"])
        self.assertTrue((Path(args.out) / "baseline.bench").is_file())
        self.assertFalse((Path(args.out) / "comparison.json").exists())

    def test_collector_refuses_busy_host_before_preparing_sources(self):
        args = SimpleNamespace(idle=True, cpu=1, baseline=self.base, candidate=self.candidate,
                               harness=self.base, out=str(self.root / "collected"), environment="synthetic test")
        with mock.patch.object(release.os, "getloadavg", return_value=(1000, 1000, 1000)), \
                mock.patch.object(release, "snapshot", side_effect=AssertionError("must refuse before preparing sources")):
            with self.assertRaisesRegex(release.GateError, "Host load is too high"):
                release.collect(self.repo, args)
        self.assertFalse(Path(args.out).exists())

    def test_comparison_command_completes_with_visible_flags_for_review(self):
        for time in (100, 111):
            directory, _ = self.evidence(benchmark_text(time=time))
            arguments = ["release.py", "performance", "--baseline", self.base, "--candidate", self.candidate,
                         "--harness", self.base, "--evidence", str(directory)]
            with self.subTest(time=time), mock.patch.object(release.sys, "argv", arguments), \
                    mock.patch.object(release.Path, "cwd", return_value=self.repo), mock.patch.object(release.os, "umask"), \
                    contextlib.redirect_stdout(io.StringIO()) as stdout, contextlib.redirect_stderr(io.StringIO()) as stderr:
                self.assertEqual(release.main(), 0)
            self.assertEqual(sum(line.startswith(release.BENCHMARKS) for line in stdout.getvalue().splitlines()), len(release.BENCHMARKS) * len(release.METRICS))
            self.assertEqual(stderr.getvalue(), "")
            if time == 111:
                self.assertIn("investigate this benchmark", stdout.getvalue())
                self.assertIn("flagged values require review", stdout.getvalue())
                self.assertIn("Fix confirmed regressions before acceptance", stdout.getvalue())
            else:
                self.assertIn("no deltas above 10%", stdout.getvalue())

    def test_release_precheck_stops_on_failed_benchmark_before_source_gates(self):
        directory, _ = self.evidence(benchmark_text(time=111).replace("PASS", "FAIL"))
        inputs = self.root / "inputs.json"
        release.save(inputs, self.config)
        with mock.patch.object(release, "public_tip", side_effect=AssertionError("must stop before remote checks")), \
                mock.patch.object(release, "collect", side_effect=AssertionError("must not benchmark")), \
                contextlib.redirect_stdout(io.StringIO()):
            with self.assertRaisesRegex(release.GateError, "Benchmark run failed"):
                release.release_check(self.repo, str(inputs), str(self.root / "acceptance"))
        self.assertFalse((directory / "comparison.json").exists())
        self.assertFalse((self.root / "acceptance").exists())

    def test_performance_missing_input_fails_without_running_go(self):
        with mock.patch.object(release, "run", side_effect=AssertionError("must not execute")):
            with self.assertRaisesRegex(release.GateError, "Missing paired"):
                release.performance_check(self.repo, self.root / "absent", self.base, self.candidate, self.base)
        with mock.patch.object(release, "collect", side_effect=AssertionError("must not benchmark")):
            with self.assertRaisesRegex(release.GateError, "Missing paired"):
                inputs = self.root / "inputs.json"
                release.save(inputs, self.config)
                release.release_check(self.repo, str(inputs), str(self.root / "acceptance"))

    def test_release_entrypoint_runs_source_gates_and_never_collects_timings(self):
        inputs = self.root / "inputs.json"
        release.save(inputs, self.config)
        calls = []
        execute = release.run

        def command(args, **kwargs):
            if args[0] == "make":
                calls.append(args[1])
                self.assertNotEqual(kwargs["cwd"], self.repo)
                self.assertEqual((kwargs["cwd"] / "source.go").read_text(), "package acp\n")
                return b"passed\n"
            return execute(args, **kwargs)

        def scanner(work):
            return SimpleNamespace(directory=work, directory_scan=mock.Mock(), scan=mock.Mock())

        for time in (100, 111):
            directory, _ = self.evidence(benchmark_text(time=time))
            output = self.root / f"acceptance-{time}"
            calls.clear()
            with self.subTest(time=time), mock.patch.object(release, "Scanner", side_effect=scanner), \
                    mock.patch.object(release, "public_tip") as remote, \
                    mock.patch.object(release, "go_environment", return_value=("go", {}, {})) as toolchain, \
                    mock.patch.object(release, "run", side_effect=command), \
                    mock.patch.object(release, "collect", side_effect=AssertionError("must not benchmark")), \
                    contextlib.redirect_stdout(io.StringIO()) as stdout:
                release.release_check(self.repo, str(inputs), str(output))
            self.assertEqual(calls, ["check", "race"])
            self.assertEqual(toolchain.call_args.kwargs["cgo"], "1")
            self.assertEqual(remote.call_count, 2)
            report = json.loads((output / "acceptance.json").read_text())
            self.assertEqual(report["performance"], json.loads((directory / "comparison.json").read_text()))
            self.assertEqual(report["performance"]["flagged"], time == 111)
            self.assertEqual(len(report["performance"]["metrics"]), len(release.BENCHMARKS) * len(release.METRICS))
            self.assertFalse(report["publication_approved"])
            final = stdout.getvalue().splitlines()[-1]
            self.assertIn("publication approval is still required", final)
            if time == 111:
                self.assertIn("flagged performance values in acceptance.json require review", final)
                self.assertIn("Fix confirmed regressions before acceptance", final)
                self.assertNotIn("Release checks passed", final)
            else:
                self.assertIn("Release checks passed", final)

    def test_history_exceptions_are_exact_and_published_only(self):
        source = self.root / "source"
        source.mkdir()
        target = self.root / "ignores"
        fingerprint = self.base + ":README.md:personal-build-path:1"
        (source / ".gitleaksignore").write_text(fingerprint + "\n")
        release.history_ignores(self.repo, source, self.base, target)
        self.assertEqual(target.read_text().strip(), fingerprint)
        for invalid in (fingerprint.replace("README.md", "*.md"), fingerprint.replace(":1", ":0"),
                        fingerprint.replace(self.base, self.candidate), fingerprint.replace(self.base, "*"),
                        fingerprint.replace("README.md", "../README.md")):
            (source / ".gitleaksignore").write_text(invalid + "\n")
            with self.subTest(fingerprint=invalid), self.assertRaises(release.GateError):
                release.history_ignores(self.repo, source, self.base, target)

    def test_artifact_requires_exact_bytes_inventory_and_source_tree(self):
        source = self.root / "source"
        release.snapshot(self.repo, self.candidate, source)
        files = {str(path.relative_to(source)): path.read_bytes() for path in source.rglob("*") if path.is_file()}
        archive = self.root / "source.tar.gz"
        archive.write_bytes(archive_bytes(files))
        item = {"path": str(archive), "sha256": release.digest(archive.read_bytes()), "kind": "source",
                "root": "", "members": {name: release.digest(value) for name, value in files.items()}}
        directory = self.root / "scan"
        directory.mkdir()
        scanner = SimpleNamespace(directory=directory, directory_scan=mock.Mock())
        release.check_artifacts([item], source, self.candidate, scanner, "go", {})
        scanner.directory_scan.assert_called_once()
        item["sha256"] = "0" * 64
        with self.assertRaisesRegex(release.GateError, "checksum mismatch"):
            release.check_artifacts([item], source, self.candidate, scanner, "go", {})
        item["sha256"] = release.digest(archive.read_bytes())
        item["members"].pop("source.go")
        with self.assertRaisesRegex(release.GateError, "inventory"):
            release.check_artifacts([item], source, self.candidate, scanner, "go", {})
        files["source.go"] = b"unreviewed source"
        archive.write_bytes(archive_bytes(files))
        item["sha256"] = release.digest(archive.read_bytes())
        item["members"] = {name: release.digest(value) for name, value in files.items()}
        with self.assertRaisesRegex(release.GateError, "exact candidate tree"):
            release.check_artifacts([item], source, self.candidate, scanner, "go", {})

    def test_command_archive_requires_source_platform_both_commands_and_licenses(self):
        files = {"README.md": b"Fixture\n", "LICENSE": b"Fixture license\n",
                 "acp": b"\x7fELFfixture", "acp-rewrite": b"\x7fELFfixture",
                 "licenses/dependency.txt": b"Dependency license"}
        archive = self.root / "commands.tar.gz"
        archive.write_bytes(archive_bytes(files))
        item = {"path": str(archive), "sha256": release.digest(archive.read_bytes()), "kind": "commands",
                "root": "", "members": {name: release.digest(value) for name, value in files.items()},
                "goos": "linux", "goarch": "amd64", "licenses": {"example.org/dep@v1.0.0": ["licenses/dependency.txt"]}}
        metadata = ("\tpath\t" + release.MODULE + "/cmd/{command}\n"
                    "\tdep\texample.org/dep\tv1.0.0\th1:fixture\n"
                    "\tbuild\t-trimpath=true\n\tbuild\tvcs.modified=false\n"
                    "\tbuild\tvcs.revision=" + self.candidate + "\n\tbuild\tGOOS=linux\n\tbuild\tGOARCH=amd64\n")
        for index, (text, licenses, passed) in enumerate((
                (metadata, item["licenses"], True),
                (metadata.replace(self.candidate, self.base), item["licenses"], False),
                (metadata.replace("modified=false", "modified=true"), item["licenses"], False),
                (metadata.replace("GOOS=linux", "GOOS=windows"), item["licenses"], False),
                (metadata.replace("/cmd/{command}", "/cmd/acp"), item["licenses"], False),
                (metadata, {}, False))):
            scanner = SimpleNamespace(directory=self.root / ("scan-" + str(index)), directory_scan=mock.Mock())
            scanner.directory.mkdir()
            with self.subTest(index=index), mock.patch.object(release, "run", side_effect=lambda args, **kwargs:
                                                             text.format(command=Path(args[-1]).name).encode()):
                if passed:
                    release.check_artifacts([{**item, "licenses": licenses}], self.repo, self.candidate, scanner, "go", {})
                else:
                    with self.assertRaises(release.GateError):
                        release.check_artifacts([{**item, "licenses": licenses}], self.repo, self.candidate, scanner, "go", {})


class PureGateTest(unittest.TestCase):
    def test_cpu_one_uses_unsuffixed_go_names_and_mismatches_fail(self):
        for cpu in (1, 4):
            _, values = release.parse_benchmarks(benchmark_text(cpu=cpu), cpu)
            self.assertEqual(set(values), set(release.BENCHMARKS))
            self.assertEqual(sum(len(row) for row in values.values()), len(release.BENCHMARKS) * len(release.METRICS))
            self.assertEqual(values[release.BENCHMARKS[0]], {"ns/op": 100, "B/op": 100, "allocs/op": 10})
        for text, cpu in ((benchmark_text(cpu=4), 1), (benchmark_text(cpu=1), 4),
                          (benchmark_text(cpu=2), 4), (benchmark_text().replace("-4\t", "-1\t"), 1)):
            with self.subTest(cpu=cpu), self.assertRaisesRegex(release.GateError, "CPU setting"):
                release.parse_benchmarks(text, cpu)

    def test_command_logs_redact_workstation_paths(self):
        with tempfile.TemporaryDirectory() as location:
            log = Path(location) / "command.log"
            private_path = b"/" + b"home" + b"/release-fixture/file.go"
            with mock.patch.object(release.subprocess, "run", return_value=subprocess.CompletedProcess([], 0, private_path, b"")):
                release.run(["fixture"], log=log)
            self.assertNotIn(private_path, log.read_bytes())
            self.assertIn(b"<home>/file.go", log.read_bytes())

    def test_benchmark_inventory_and_go_test_completion_are_required(self):
        for text in (benchmark_text(benchmarks=release.BENCHMARKS[:8]),
                     benchmark_text(benchmarks=release.BENCHMARKS[:-1]),
                     benchmark_text(benchmarks=release.BENCHMARKS + (release.BENCHMARKS[0],)),
                     benchmark_text() * 2, benchmark_text().replace("PASS", "SKIP", 1),
                     benchmark_text().replace("PASS\n", ""), benchmark_text().split("ok  \t")[0],
                     benchmark_text().replace("ok  \t" + release.MODULE, "ok  \tunexpected/package"),
                     benchmark_text().replace("PASS", "FAIL", 1),
                     benchmark_text().replace(" B/op", " missing/op"),
                     benchmark_text().replace("10 allocs/op", "10 allocs/op 20 allocs/op", 1),
                     benchmark_text(time="nan"), benchmark_text(time="inf"), benchmark_text(time=-1),
                     benchmark_text(time="invalid"), benchmark_text(iterations=0), benchmark_text(iterations="invalid"),
                     benchmark_text().replace(release.BENCHMARKS[0], "BenchmarkUnexpected"),
                     benchmark_text().replace("goarch: amd64\n", "")):
            with self.subTest(text=text[:40]), self.assertRaises(release.GateError):
                release.parse_benchmarks(text, 4)

    def test_archives_reject_escape_links_and_duplicate_names(self):
        for name, kind in (("../outside", tarfile.REGTYPE), ("/outside", tarfile.REGTYPE),
                           ("link", tarfile.SYMTYPE), ("pipe", tarfile.FIFOTYPE)):
            data = io.BytesIO()
            with tarfile.open(fileobj=data, mode="w") as archive:
                entry = tarfile.TarInfo(name)
                entry.type = kind
                archive.addfile(entry)
            with self.subTest(name=name), self.assertRaises(release.GateError):
                release.archive_files(data.getvalue(), "tar")
        data = io.BytesIO()
        with tarfile.open(fileobj=data, mode="w") as archive:
            archive.addfile(tarfile.TarInfo("same"))
            archive.addfile(tarfile.TarInfo("same"))
        with self.assertRaisesRegex(release.GateError, "Duplicate"):
            release.archive_files(data.getvalue(), "tar")

    def test_scanner_disables_inline_and_current_tree_exceptions_and_redacts_diagnostics(self):
        with tempfile.TemporaryDirectory() as location:
            scanner = release.Scanner.__new__(release.Scanner)
            scanner.directory = Path(location)
            scanner.executable = scanner.directory / "gitleaks"
            scanner.config = scanner.directory / "rules"
            scanner.no_ignores = scanner.directory / "empty"
            with mock.patch.object(release.subprocess, "run") as execute:
                execute.return_value = subprocess.CompletedProcess([], 0)
                scanner.scan(["dir", "fixture"])
                command = execute.call_args[0][0]
                self.assertIn("--ignore-gitleaks-allow", command)
                self.assertEqual(command[command.index("--gitleaks-ignore-path") + 1], str(scanner.no_ignores))
                execute.return_value = subprocess.CompletedProcess([], 1, stdout=b"sensitive", stderr=b"sensitive")
                with self.assertRaisesRegex(release.GateError, "withheld") as error:
                    scanner.scan(["dir", "fixture"])
                self.assertNotIn("sensitive", str(error.exception))

    def test_scanner_verifies_archive_before_execution(self):
        with tempfile.TemporaryDirectory() as location:
            path = Path(location)
            archive = path / "untrusted.tar.gz"
            archive.write_bytes(b"not the pinned scanner")
            with mock.patch.dict(os.environ, {"GITLEAKS_ARCHIVE": str(archive), "GITLEAKS_RULES": str(archive)}), \
                    mock.patch.object(release, "run", side_effect=AssertionError("unverified execution")):
                with self.assertRaisesRegex(release.GateError, "checksum mismatch"):
                    release.Scanner(path)


@unittest.skipUnless(os.environ.get("GITLEAKS_ARCHIVE") and os.environ.get("GITLEAKS_RULES"),
                     "pinned Gitleaks archive/rules not supplied")
class ScannerIntegrationTest(RepositoryFixture, unittest.TestCase):
    def test_real_scanner_history_exceptions_never_hide_current_source_or_binary_artifacts(self):
        private_path = "/" + "home" + "/release-fixture/private.txt"
        self.write("fixture.txt", private_path + "\n")
        published = self.commit()
        self.write("fixture.txt", "public fixture\n")
        self.write(".gitleaksignore", published + ":fixture.txt:personal-build-path:1\n")
        final = self.commit()
        work = self.root / "scanner"
        work.mkdir()
        source = work / "source"
        release.snapshot(self.repo, final, source)
        scanner = release.Scanner(work)
        release.scan_history(self.repo, final, published, source, scanner)
        scanner.directory_scan(source)
        (source / "fixture.txt").write_text(private_path + "\n")
        with self.assertRaisesRegex(release.GateError, "Content scan failed"):
            scanner.directory_scan(source)
        (source / "fixture.txt").unlink()
        (source / "binary.dat").write_bytes(b"\0" + private_path.encode() + b"\0")
        with self.assertRaisesRegex(release.GateError, "Content scan failed"):
            scanner.directory_scan(source)

    def test_real_scanner_commit_messages_have_no_file_exceptions(self):
        final = self.commit("public message " + "/" + "home" + "/release-fixture/private.txt")
        work = self.root / "scanner"
        work.mkdir()
        source = work / "source"
        release.snapshot(self.repo, final, source)
        scanner = release.Scanner(work)
        with self.assertRaisesRegex(release.GateError, "Content scan failed"):
            release.scan_history(self.repo, final, self.base, source, scanner)


if __name__ == "__main__":
    unittest.main()
