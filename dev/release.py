#!/usr/bin/env python3
"""Local ACP release gates. No command publishes or changes repository history."""

import argparse
import hashlib
import io
import json
import math
import os
from pathlib import Path, PurePosixPath
import platform
import re
import subprocess
import sys
import tarfile
import tempfile
import zipfile


MODULE = "github.com/samuelncui/acp"
SCANNER_VERSION = "8.30.1"
SCANNER_ARCHIVES = {
    ("Linux", "x86_64"): "551f6fc83ea457d62a0d98237cbad105af8d557003051f41f3e7ca7b3f2470eb",
    ("Darwin", "arm64"): "b40ab0ae55c505963e365f271a8d3846efbc170aa17f2607f13df610a9aeb6a5",
    ("Darwin", "x86_64"): "dfe101a4db2255fc85120ac7f3d25e4342c3c20cf749f2c20a18081af1952709",
}
DEFAULT_RULES_SHA256 = "e163e53b9e7e8a8511e77271e2b323ed057759542a6d988258afe3a1fa329caf"
CONTENT_RULES_SHA256 = "36ca2e3e9793dfa830e7c4fc813ca3eef92dd1b846da516a495e86132eb2f475"
BENCHMARKS = (
    "BenchmarkReadBuffered", "BenchmarkReadMapped",
    "BenchmarkRefreshSignatureUnchanged", "BenchmarkRefreshSignatureChanged",
    "BenchmarkCopyWorkload/LargeOneTarget", "BenchmarkCopyWorkload/LargeThreeTargets",
    "BenchmarkCopyWorkload/SmallOneTarget", "BenchmarkCopyWorkload/SmallThreeTargets",
    "BenchmarkCopyWorkload/LinearSmallBuffered", "BenchmarkCopyWorkload/LinearSmallMapped",
    "BenchmarkCopyWorkload/LinearLargeBuffered", "BenchmarkCopyWorkload/LinearLargeMapped",
    "BenchmarkCopyWorkload/LinearMixedBuffered", "BenchmarkCopyWorkload/LinearMixedMapped",
)
METRICS = ("ns/op", "B/op", "allocs/op")
SIDES = ("baseline", "candidate")


class GateError(Exception):
    """A diagnostic safe to print without source, scanner matches or local paths."""


def require(condition, message):
    if not condition:
        raise GateError(message)


def digest(data):
    return hashlib.sha256(data).hexdigest()


def save(path, value):
    path.write_text(json.dumps(value, indent=2, sort_keys=True) + "\n")


def run(args, cwd=None, env=None, log=None):
    result = subprocess.run([str(arg) for arg in args], cwd=cwd, env=env,
                            stdout=subprocess.PIPE, stderr=subprocess.PIPE)
    if log:
        diagnostics = re.sub(rb"/(?:Users|home)/[^/\s]+", b"<home>", result.stdout + result.stderr)
        diagnostics = re.sub(rb"/(?:private/)?var/folders/[^\s]+", b"<private-temp>", diagnostics)
        log.write_bytes(diagnostics)
    require(result.returncode == 0, "A required command failed; inspect its private log.")
    return result.stdout


def git(repo, *args):
    return run(["git", "-C", repo, *args]).decode().strip()


def commit(repo, value):
    require(isinstance(value, str) and re.fullmatch(r"[a-f0-9]{40}", value),
            "Every source, baseline, harness and history input requires a full commit SHA.")
    try:
        resolved = git(repo, "rev-parse", value + "^{commit}")
    except GateError:
        raise GateError("Required exact commit is unavailable locally; fetch the reviewed source first.") from None
    require(resolved == value, "Commit is unavailable locally.")
    return value


def safe_name(name):
    require(name and not name.startswith("/") and "\\" not in name
            and all(part not in ("", ".", "..", ".git") for part in name.split("/")),
            "Archive or source has an unsafe member path.")
    return name


def archive_files(data, kind):
    """Read ordinary members only, without ever extracting archive paths."""
    files = {}
    if kind == "zip":
        with zipfile.ZipFile(io.BytesIO(data)) as archive:
            for entry in archive.infolist():
                safe_name(entry.filename.rstrip("/"))
                require((entry.external_attr >> 16) & 0o170000 in (0, 0o100000, 0o040000),
                        "Archive links and special files are not accepted.")
                if entry.is_dir():
                    continue
                require(entry.filename not in files, "Duplicate archive member.")
                files[entry.filename] = archive.read(entry)
    else:
        with tarfile.open(fileobj=io.BytesIO(data), mode="r:*") as archive:
            for entry in archive:
                safe_name(entry.name.rstrip("/"))
                require(entry.isfile() or entry.isdir(), "Archive links and special files are not accepted.")
                if entry.isdir():
                    continue
                require(entry.name not in files, "Duplicate archive member.")
                files[entry.name] = archive.extractfile(entry).read()
    return files


def snapshot(repo, revision, destination):
    """Check archive bytes against the actual Git tree, including export-ignored files."""
    commit(repo, revision)
    files = archive_files(run(["git", "-C", repo, "archive", "--format=tar", revision]), "tar")
    entries = git(repo, "ls-tree", "-rz", revision).split("\0")
    expected = {}
    for entry in filter(None, entries):
        metadata, name = entry.split("\t", 1)
        mode, kind, oid = metadata.split()
        require(kind == "blob" and mode in ("100644", "100755"),
                "Release source must contain ordinary files, not links or submodules.")
        expected[safe_name(name)] = (mode, oid)
    require(set(files) == set(expected), "Source archive differs from the complete committed tree.")
    destination.mkdir()
    for name, data in files.items():
        mode, oid = expected[name]
        actual = hashlib.sha1(b"blob " + str(len(data)).encode() + b"\0" + data).hexdigest()
        require(actual == oid, "Source archive changed a committed file.")
        target = destination / name
        target.parent.mkdir(parents=True, exist_ok=True)
        target.write_bytes(data)
        target.chmod(int(mode, 8) & 0o777)
    return git(repo, "rev-parse", revision + "^{tree}")


def private_output(repo, value):
    output = Path(value).resolve()
    try:
        relative = output.relative_to(repo.resolve())
    except ValueError:
        relative = None
    if relative is not None:
        require(relative.parts and relative.parts[0] == "output",
                "Reports inside the repository must be under ignored output/.")
        require(git(repo, "check-ignore", str(relative)) == str(relative), "Report output is not ignored.")
    require(not output.exists(), "Use a new output directory; existing evidence is never overwritten.")
    output.mkdir(parents=True, mode=0o700)
    return output


def module_text(source):
    module = (source / "go.mod").read_text()
    require(re.search(r"^module " + re.escape(MODULE) + r"$", module, re.M), "Unexpected Go module identity.")
    require(not re.search(r"^replace\b", module, re.M), "Release inputs may not depend on local module replacements.")
    return module


def go_environment(source, temporary, cpu=4, cgo="0"):
    module = module_text(source)
    match = re.search(r"^go (\d+\.\d+\.\d+)$", module, re.M)
    require(match, "go.mod must pin a complete Go patch version.")
    env = {key: os.environ[key] for key in ("PATH", "HOME", "SYSTEMROOT", "SSL_CERT_FILE", "SSL_CERT_DIR",
                                          "GITLEAKS_ARCHIVE", "GITLEAKS_RULES")
           if key in os.environ}
    env.update(GOTOOLCHAIN="go" + match[1], GOENV="off", GOWORK="off", GOFLAGS="-mod=readonly",
               CGO_ENABLED=cgo, GOMAXPROCS=str(cpu), GOGC="100", GOMEMLIMIT="off", GODEBUG="",
               TMPDIR=str(temporary), PYTHONDONTWRITEBYTECODE="1")
    info = json.loads(run(["go", "env", "-json", "GOVERSION", "GOOS", "GOARCH", "GOROOT"], cwd=source, env=env))
    require(info["GOVERSION"] == "go" + match[1], "Go toolchain does not match the pinned patch version.")
    executable = Path(info.pop("GOROOT")) / "bin" / "go"
    env["PATH"] = str(executable.parent) + os.pathsep + env.get("PATH", "")
    env["GOTOOLCHAIN"] = "local"
    return executable, env, info


class Scanner:
    def __init__(self, temporary):
        expected = SCANNER_ARCHIVES.get((platform.system(), platform.machine()))
        require(expected, "No pinned Gitleaks archive for this host.")
        archive = os.environ.get("GITLEAKS_ARCHIVE")
        rules = os.environ.get("GITLEAKS_RULES")
        require(archive and rules, "Set GITLEAKS_ARCHIVE and GITLEAKS_RULES to the pinned 8.30.1 inputs.")
        data = Path(archive).read_bytes()
        require(digest(data) == expected, "Gitleaks archive checksum mismatch.")
        defaults = Path(rules).read_bytes()
        require(digest(defaults) == DEFAULT_RULES_SHA256, "Gitleaks default rules checksum mismatch.")
        overrides = Path(__file__).with_name("content-rules.toml").read_bytes()
        require(digest(overrides) == CONTENT_RULES_SHA256, "ACP content rules checksum mismatch.")
        self.directory = temporary
        self.executable = temporary / "gitleaks"
        with tarfile.open(fileobj=io.BytesIO(data), mode="r:gz") as packed:
            entry = packed.getmember("gitleaks")
            require(entry.isfile(), "Gitleaks executable is not an ordinary archive member.")
            self.executable.write_bytes(packed.extractfile(entry).read())
        self.executable.chmod(0o700)
        require(run([self.executable, "version"]).decode().strip() == SCANNER_VERSION,
                "Gitleaks version mismatch.")
        # Remove upstream global vendor/generated exclusions; retain its credential rules.
        text = defaults.decode()
        start, end = text.find("\n[allowlist]\n"), text.find("\n[[rules]]\n")
        require(0 <= start < end, "Pinned default rule structure changed.")
        base = temporary / "defaults.toml"
        base.write_text(text[:start] + text[end:])
        self.config = temporary / "rules.toml"
        self.config.write_text(overrides.decode().replace("useDefault = true", "path = " + json.dumps(str(base))))
        self.no_ignores = temporary / "empty-ignores"
        self.no_ignores.write_text("")

    def scan(self, args, ignores=None):
        report = self.directory / "scan.json"
        result = subprocess.run([str(self.executable), *map(str, args), "--config", str(self.config),
                                 "--redact=100", "--no-banner", "--no-color", "--ignore-gitleaks-allow",
                                 "--gitleaks-ignore-path", str(ignores or self.no_ignores),
                                 "--max-archive-depth=4", "--max-decode-depth=5",
                                 "--report-format=json", "--report-path", str(report)],
                                stdout=subprocess.PIPE, stderr=subprocess.PIPE)
        require(result.returncode == 0, "Content scan failed; findings and scanner diagnostics are withheld.")

    def directory_scan(self, directory):
        self.scan(["dir", directory])
        # Gitleaks skips binary data. Scan printable strings as well, including UTF-16 paths.
        with tempfile.TemporaryDirectory(dir=self.directory) as location:
            strings = Path(location)
            for index, source in enumerate(sorted(directory.rglob("*"))):
                require(not source.is_symlink(), "Content directory contains a link.")
                if not source.is_file():
                    continue
                data = source.read_bytes()
                if b"\0" not in data:
                    continue
                values = re.findall(rb"[\x20-\x7e]{4,}", data)
                values += [value.replace(b"\0", b"") for value in re.findall(rb"(?:[\x20-\x7e]\x00){4,}", data)]
                (strings / (str(index) + ".txt")).write_bytes(b"\n".join(values))
            self.scan(["dir", strings])


def history_ignores(repo, source, public_base, destination):
    fingerprints = []
    path = source / ".gitleaksignore"
    for line in path.read_text().splitlines() if path.exists() else []:
        if not line.strip() or line.startswith("#"):
            continue
        match = re.fullmatch(r"([a-f0-9]{40}):([^:\r\n]+):([a-z0-9][a-z0-9-]*):([1-9][0-9]*)", line)
        require(match and not re.search(r"[\\*?\[\]]", match[2]),
                "History exceptions require exact commit:file:rule:line fingerprints.")
        safe_name(match[2])
        git(repo, "merge-base", "--is-ancestor", match[1], public_base)
        fingerprints.append(line)
    destination.write_text("\n".join(fingerprints) + "\n")
    return destination


def scan_history(repo, tip, public_base, source, scanner):
    require(git(repo, "rev-parse", "--is-shallow-repository") == "false", "A complete Git history is required.")
    git(repo, "merge-base", "--is-ancestor", public_base, tip)
    ignores = history_ignores(repo, source, public_base, scanner.directory / "history-ignores")
    scanner.scan(["git", repo, "--log-opts=--full-history --root -m " + tip], ignores)
    # Commit messages are separate public content and never inherit file/line exceptions.
    messages = scanner.directory / "messages"
    messages.mkdir(exist_ok=True)
    (messages / "commits.txt").write_bytes(run(["git", "-C", repo, "log", "--format=%H%n%B", tip]))
    scanner.directory_scan(messages)


def harness_files(repo, revision):
    files = {name: run(["git", "-C", repo, "show", revision + ":" + name])
             for name in ("bench_test.go", "workload_bench_test.go")}
    # The benchmarks share this existing lifecycle helper with item tests. Copy only the helper,
    # so unrelated candidate unit tests cannot change what the baseline must compile.
    source = run(["git", "-C", repo, "show", revision + ":item_test.go"])
    helper = re.search(rb"(?ms)^func runStream\(.*?^}\n", source)
    require(helper, "Reviewed benchmark harness lacks the runStream helper.")
    files["release_helper_test.go"] = b'package acp\n\nimport "context"\n\n' + helper[0]
    return files


def benchmark_args(cpu):
    selector = "^(" + "|".join(dict.fromkeys(name.split("/")[0] for name in BENCHMARKS)) + ")$"
    return ["test", "-run=^$", "-bench=" + selector, "-benchtime=1s", "-count=1", "-benchmem",
            "-cpu=" + str(cpu), "-p=1", "-timeout=30m", "."]


def parse_benchmarks(text, cpu):
    values = {}
    names = {name if cpu == 1 else f"{name}-{cpu}": name for name in BENCHMARKS}
    headers = {}
    parts = re.split(r"^PASS$", text, flags=re.M)
    require(len(parts) == 2 and re.fullmatch(r"\s*ok[ \t]+" + re.escape(MODULE) +
            r"[ \t]+[0-9]+(?:\.[0-9]+)?s\s*", parts[-1]) and not re.search(r"\b(?:FAIL|SKIP)\b", text),
            "Benchmark run failed, skipped or did not finish.")
    for line in parts[0].splitlines():
        header = re.fullmatch(r"(goos|goarch|cpu|pkg): (.+)", line)
        if header:
            require(header[1] not in headers or headers[header[1]] == header[2], "Benchmark environment changed.")
            headers[header[1]] = header[2]
        if not line.startswith("Benchmark"):
            continue
        columns = line.split()
        name = names.get(columns[0])
        require(name is not None, "Unexpected benchmark or CPU setting.")
        require(name not in values, "Repeated benchmark row.")
        require(len(columns) >= 8 and len(columns) % 2 == 0 and columns[1].isdigit() and int(columns[1]) > 0,
                "Benchmark row is malformed or has no completed iterations.")
        metrics = dict(zip(columns[3::2], columns[2::2]))
        require(len(metrics) == len(columns[3::2]) and all(unit in metrics for unit in METRICS),
                "Required performance metric is missing or repeated.")
        try:
            values[name] = {unit: float(metrics[unit]) for unit in METRICS}
        except ValueError:
            raise GateError("Invalid performance value.") from None
        require(all(math.isfinite(value) and value >= 0 for value in values[name].values()),
                "Invalid performance value.")
    require(set(headers) == {"goos", "goarch", "cpu", "pkg"} and headers["pkg"] == MODULE,
            "Missing or unexpected benchmark environment.")
    require(set(values) == set(BENCHMARKS), "Incomplete benchmark inventory.")
    return headers, values


def performance_check(repo, directory, baseline, candidate, harness):
    require(baseline != candidate, "Choose a distinct explicitly accepted performance baseline.")
    require(directory.is_dir() and (directory / "pair.json").is_file(), "Missing paired performance evidence.")
    record = json.loads((directory / "pair.json").read_text())
    require(isinstance(record, dict), "Performance metadata must be a JSON object.")
    require(record.get("complete") is True and record.get("idle_confirmed") is True,
            "Performance collection is incomplete or lacks idle-host confirmation.")
    for key, expected in (("baseline", baseline), ("harness", harness)):
        commit(repo, expected)
        require(record.get(key) == expected, "Performance evidence has a different source or harness commit.")
    measured = commit(repo, record.get("candidate"))
    commit(repo, candidate)
    # These files are not inputs to the direct Go benchmark command. Keep the measured identity,
    # rather than timing identical Go code again after a documentation or checker-only correction.
    changed = set(filter(None, run(["git", "-C", repo, "diff", "--name-only", "-z", measured, candidate, "--"]).decode().split("\0")))
    require(changed <= {"AGENTS.md", "README.md", "TESTING.md", "LICENSE", "mmap/LICENSE",
                        "dev/release.py", "dev/test_release.py"},
            "Performance inputs differ from the measured candidate; collect matching evidence.")
    collector = record.get("collector_sha256")
    if collector != digest(Path(__file__).read_bytes()):
        require(collector == digest(run(["git", "-C", repo, "show", measured + ":dev/release.py"])),
                "Performance collector differs from both the current and measured source tooling.")
    expected_files = {name: digest(data) for name, data in harness_files(repo, harness).items()}
    require(record.get("harness_files") == expected_files, "Performance harness bytes differ from the reviewed commit.")
    cpu = record.get("cpu")
    require(type(cpu) is int and cpu > 0, "Invalid CPU setting.")
    require(record.get("command") == benchmark_args(cpu), "Recorded benchmark command differs.")
    environment = record.get("environment", {})
    require(isinstance(environment, dict) and environment.get("label") and environment.get("filesystem")
            and isinstance(environment.get("go"), dict),
            "Missing performance environment or filesystem identity.")
    module = run(["git", "-C", repo, "show", harness + ":go.mod"]).decode()
    version = re.search(r"^go (\d+\.\d+\.\d+)$", module, re.M)
    require(version and environment["go"].get("GOVERSION") == "go" + version[1]
            and environment.get("cgo") == "0" and environment.get("GOGC") == "100"
            and environment.get("GOMEMLIMIT") == "off", "Recorded performance runtime settings differ.")
    parsed = {}
    for side in SIDES:
        require(record.get("trees", {}).get(side) == git(repo, "rev-parse", record[side] + "^{tree}"),
                "Performance source tree differs from the required commit.")
        data = (directory / (side + ".bench")).read_bytes()
        require(digest(data) == record.get("outputs", {}).get(side), "Performance output checksum mismatch.")
        parsed[side] = parse_benchmarks(data.decode(), cpu)
    require(parsed["baseline"][0] == parsed["candidate"][0], "Paired benchmark environments differ.")
    for key in ("goos", "goarch"):
        require(parsed["baseline"][0][key] == environment["go"].get(key.upper()), "Recorded Go environment differs.")
    rows = []
    for name in BENCHMARKS:
        for unit in METRICS:
            before = parsed["baseline"][1][name][unit]
            after = parsed["candidate"][1][name][unit]
            delta = None
            if before:
                delta = (after - before) / before * 100
            elif after == 0:
                delta = 0
            rows.append({"benchmark": name, "metric": unit, "baseline": before, "candidate": after,
                         "delta_percent": delta, "flagged": after > before * 1.10})
    result = {"baseline": baseline, "measured_candidate": measured, "candidate": candidate,
              "harness": harness, "flagged": any(row["flagged"] for row in rows), "metrics": rows}
    save(directory / "comparison.json", result)
    report_performance(result)
    return result


def report_performance(comparison):
    print("Benchmark\tMetric\tBaseline\tCandidate\tDelta\tFlag")
    for row in comparison["metrics"]:
        delta = "n/a (zero baseline)" if row["delta_percent"] is None else f"{row['delta_percent']:+.2f}%"
        flag = "investigate this benchmark" if row["flagged"] else ""
        print(f"{row['benchmark']}\t{row['metric']}\t{row['baseline']}\t{row['candidate']}\t{delta}\t{flag}")
    if comparison["flagged"]:
        print("Performance comparison complete; flagged values require review and focused investigation. "
              "Fix confirmed regressions before acceptance.")
    else:
        print("Performance comparison complete; no deltas above 10%.")


def collect(repo, args):
    require(args.idle, "Collection requires --idle after all other builds, tests and agents have stopped.")
    require(args.cpu > 0, "Require a positive CPU count.")
    initial_load = os.getloadavg()
    require(initial_load[0] <= max(0.5, (os.cpu_count() or 1) / 4),
            "Host load is too high; repeat both sides later on an idle host.")
    require(args.baseline != args.candidate, "Choose a distinct explicitly accepted performance baseline.")
    for value in (args.baseline, args.candidate, args.harness):
        commit(repo, value)
    files = harness_files(repo, args.harness)
    output = private_output(repo, args.out)
    record = {"complete": False, "idle_confirmed": True, "baseline": args.baseline, "candidate": args.candidate,
              "harness": args.harness, "harness_files": {name: digest(data) for name, data in files.items()},
              "collector_sha256": digest(Path(__file__).read_bytes()), "cpu": args.cpu,
              "command": benchmark_args(args.cpu), "initial_load": initial_load,
              "trees": {}, "outputs": {}, "runs": []}
    save(output / "pair.json", record)
    with tempfile.TemporaryDirectory(prefix=".acp-performance-", dir=output.parent) as location:
        work = Path(location)
        temporary = work / "tmp"
        temporary.mkdir()
        for side in SIDES:
            source = work / side
            record["trees"][side] = snapshot(repo, record[side], source)
            module_text(source)
            for path in source.glob("*_test.go"):
                path.unlink()
            for name, data in files.items():
                (source / name).write_bytes(data)
        # One exact toolchain and filesystem serve both sources.
        toolchain_source = work / "harness"
        snapshot(repo, args.harness, toolchain_source)
        go, env, info = go_environment(toolchain_source, temporary, args.cpu)
        fs = os.statvfs(temporary)
        record["environment"] = {"label": args.environment, "go": info, "host": platform.platform(),
                                 "filesystem": {"device": temporary.stat().st_dev, "block_size": fs.f_bsize},
                                 "cgo": "0", "GOGC": "100", "GOMEMLIMIT": "off", "cache": "warm/uncontrolled OS cache"}
        save(output / "pair.json", record)
        for side in SIDES:
            # Load includes our own work; retain it as evidence, not an external-work detector.
            load = os.getloadavg()
            data = run([go, *record["command"]], cwd=work / side, env=env, log=output / (side + ".log"))
            (output / (side + ".bench")).write_bytes(data)
            parse_benchmarks(data.decode(), args.cpu)
            record["runs"].append({"side": side, "load": load})
            save(output / "pair.json", record)
        record["outputs"] = {side: digest((output / (side + ".bench")).read_bytes()) for side in SIDES}
        record["complete"] = True
        save(output / "pair.json", record)
    performance_check(repo, output, args.baseline, args.candidate, args.harness)


def check_artifacts(artifacts, source, candidate, scanner, go, env):
    require(isinstance(artifacts, list), "Declare artifacts explicitly; [] means a Go-module-only release.")
    accepted = []
    for index, item in enumerate(artifacts):
        require(isinstance(item, dict), "Each artifact requires a JSON object with its exact inventory.")
        path = Path(item["path"])
        data = path.read_bytes()
        require(digest(data) == item.get("sha256"), "Distributed archive checksum mismatch.")
        require(path.name.endswith((".tar.gz", ".tgz", ".zip")), "Artifacts must be tar.gz, tgz or zip archives.")
        files = archive_files(data, "zip" if path.suffix == ".zip" else "tar")
        inventory = {name: digest(value) for name, value in files.items()}
        require(inventory and inventory == item.get("members"), "Archive differs from its complete reviewed member inventory.")
        root = item.get("root", "")
        prefix = safe_name(root) + "/" if root else ""
        require(all(name.startswith(prefix) for name in files), "Archive members are outside the declared root.")
        contents = {name[len(prefix):]: value for name, value in files.items()}
        require(item.get("kind") in ("source", "commands"), "Declare each archive as source or commands.")
        if item["kind"] == "source":
            expected = {str(path.relative_to(source)): path.read_bytes() for path in source.rglob("*") if path.is_file()}
            require(contents == expected, "Source archive is not the complete exact candidate tree.")
        else:
            require((item.get("goos"), item.get("goarch")) in
                    (("linux", "amd64"), ("windows", "amd64"), ("darwin", "arm64"), ("freebsd", "amd64")),
                    "Command archive requires a supported explicit target platform.")
        for name in ("LICENSE", "README.md"):
            require(any(PurePosixPath(member).name == name and value == (source / name).read_bytes()
                        for member, value in files.items()), "Archive is missing exact source README or LICENSE.")
        directory = scanner.directory / ("artifact-" + str(index))
        directory.mkdir()
        commands, dependencies = set(), set()
        for name, value in files.items():
            require(not name.endswith((".tar", ".tar.gz", ".tgz", ".zip", ".gz", ".xz", ".bz2", ".7z")),
                    "Nested archives need explicit unpacked review; they are not accepted silently.")
            target = directory / name
            target.parent.mkdir(parents=True, exist_ok=True)
            target.write_bytes(value)
            if value[:4] in (b"\x7fELF", b"\xcf\xfa\xed\xfe", b"\xce\xfa\xed\xfe", b"\xfe\xed\xfa\xcf",
                             b"\xfe\xed\xfa\xce", b"\xca\xfe\xba\xbe", b"\xbe\xba\xfe\xca") or value[:2] == b"MZ":
                metadata = run([go, "version", "-m", target], env=env).decode()
                settings = dict(re.findall(r"^\s+build\s+([^=]+)=(.*)$", metadata, re.M))
                command = re.search(r"^\s+path\s+(\S+)$", metadata, re.M)
                require(item["kind"] == "commands" and command and command[1] in
                        (MODULE + "/cmd/acp", MODULE + "/cmd/acp-rewrite")
                        and settings.get("vcs.revision") == candidate and settings.get("vcs.modified") == "false"
                        and settings.get("-trimpath") == "true" and settings.get("GOOS") == item["goos"]
                        and settings.get("GOARCH") == item["goarch"],
                        "Distributed executable lacks exact clean ACP source identity or trimpath.")
                commands.add(command[1])
                dependencies.update(name + "@" + version for name, version in
                                    re.findall(r"^\s+dep\s+(\S+)\s+(\S+)", metadata, re.M))
        if item["kind"] == "commands":
            require(commands == {MODULE + "/cmd/acp", MODULE + "/cmd/acp-rewrite"},
                    "Command archive must include both ACP executables.")
            licenses = item.get("licenses", {})
            require(isinstance(licenses, dict) and set(licenses) == dependencies
                    and all(isinstance(paths, list) and paths and all(files.get(path) for path in paths)
                            for paths in licenses.values()),
                    "Every shipped module dependency needs explicit reviewed license members.")
        scanner.directory_scan(directory)
        accepted.append({"sha256": item["sha256"], "kind": item["kind"], "members": inventory})
    return accepted


def release_inputs(repo, config):
    require(isinstance(config, dict), "Release inputs must be a JSON object.")
    for key in ("candidate", "public_base", "history_tip", "baseline", "harness"):
        require(key in config, "Release input missing: " + key)
        commit(repo, config[key])
    require(re.fullmatch(r"v[01]\.[0-9]+\.[0-9]+(?:-[0-9A-Za-z.-]+)?", config.get("version", "")),
            "A new explicit release version matching this Go module path is required.")
    require(isinstance(config.get("artifacts"), list) and config.get("performance"),
            "Exact artifact inventory and performance evidence are required.")
    require(git(repo, "rev-parse", "HEAD") == config["candidate"] and not git(repo, "status", "--porcelain"),
            "Release candidate must be the exact clean current commit.")
    require(git(repo, "rev-parse", "--is-shallow-repository") == "false", "A complete Git history is required.")
    parents = git(repo, "show", "-s", "--format=%P", config["candidate"]).split()
    require(parents == [config["public_base"]],
            "Squash the reviewed unpublished range first: candidate must have only the rechecked public tip as parent.")
    git(repo, "merge-base", "--is-ancestor", config["public_base"], config["history_tip"])
    require(git(repo, "rev-parse", config["history_tip"] + "^{tree}") ==
            git(repo, "rev-parse", config["candidate"] + "^{tree}"),
            "Pre-squash history and final candidate trees differ; review the final source again.")
    if config["history_tip"] != config["candidate"]:
        backup = config.get("backup_ref", "")
        require(backup.startswith("refs/heads/") and git(repo, "rev-parse", backup) == config["history_tip"],
                "Retain a local backup branch at the exact pre-squash tip.")


def public_tip(repo, config):
    require(git(repo, "rev-parse", "refs/remotes/origin/main") == config["public_base"],
            "origin/main differs from the explicitly reviewed public base.")
    remote = git(repo, "ls-remote", "--exit-code", "origin", "refs/heads/main").split()
    require(remote == [config["public_base"], "refs/heads/main"], "Public main moved; reconcile and review again.")
    require(not git(repo, "tag", "--list", config["version"]), "Version already has a local tag.")
    require(not git(repo, "ls-remote", "origin", "refs/tags/" + config["version"]), "Version is already public.")


def release_check(repo, inputs, output_value):
    require(inputs, "RELEASE_INPUTS is required; see TESTING.md for the explicit release manifest.")
    config = json.loads(Path(inputs).read_text())
    release_inputs(repo, config)
    # Check evidence and observed deltas before source gates; this never runs timings.
    comparison = performance_check(repo, Path(config["performance"]), config["baseline"], config["candidate"], config["harness"])
    public_tip(repo, config)
    output = private_output(repo, output_value)
    with tempfile.TemporaryDirectory(prefix="acp-release-") as location:
        work = Path(location)
        source = work / "source"
        snapshot(repo, config["candidate"], source)
        scanner = Scanner(work)
        scanner.directory_scan(source)
        scan_history(repo, config["history_tip"], config["public_base"], source, scanner)
        if config["history_tip"] != config["candidate"]:
            scan_history(repo, config["candidate"], config["public_base"], source, scanner)
        go, env, info = go_environment(source, work, cgo="1")
        for target in ("check", "race"):
            print("Running exact-source " + target + " (private log).", flush=True)
            run(["make", target, "GO=" + str(go)], cwd=source, env=env, log=output / (target + ".log"))
        artifacts = check_artifacts(config["artifacts"], source, config["candidate"], scanner, go, env)
        # Nothing may be silently changed while acceptance is in progress.
        release_inputs(repo, config)
        public_tip(repo, config)
        save(output / "acceptance.json", {"candidate": config["candidate"], "version": config["version"],
             "public_base": config["public_base"], "history_tip": config["history_tip"],
             "scanner": SCANNER_VERSION, "default_rules_sha256": DEFAULT_RULES_SHA256,
             "content_rules_sha256": CONTENT_RULES_SHA256, "go": info, "artifacts": artifacts,
             "performance": comparison, "publication_approved": False})
    if comparison["flagged"]:
        print("Release checks complete; flagged performance values in acceptance.json require review. "
              "Fix confirmed regressions before acceptance. "
              "Exact source/version/artifact publication approval is still required.")
    else:
        print("Release checks passed. Exact source/version/artifact publication approval is still required.")


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    sub = parser.add_subparsers(dest="command", required=True)
    check = sub.add_parser("check")
    check.add_argument("--inputs", default=os.environ.get("RELEASE_INPUTS"))
    check.add_argument("--out", default=os.environ.get("RELEASE_OUTPUT", "output/release-acceptance"))
    history = sub.add_parser("history")
    history.add_argument("--tip", required=True)
    history.add_argument("--public-base", required=True)
    history.add_argument("--out", required=True)
    for name in ("collect", "performance"):
        command = sub.add_parser(name)
        for key in ("baseline", "candidate", "harness"):
            command.add_argument("--" + key, required=True)
        if name == "performance":
            command.add_argument("--evidence", required=True)
            continue
        command.add_argument("--out", required=True)
        command.add_argument("--environment", required=True)
        command.add_argument("--idle", action="store_true")
        command.add_argument("--cpu", type=int, default=4)
    args = parser.parse_args()
    os.umask(0o077)
    repo = Path.cwd()
    try:
        if args.command == "check":
            release_check(repo, args.inputs, args.out)
        elif args.command == "collect":
            collect(repo, args)
        elif args.command == "performance":
            performance_check(repo, Path(args.evidence), args.baseline, args.candidate, args.harness)
        else:
            commit(repo, args.tip)
            commit(repo, args.public_base)
            output = private_output(repo, args.out)
            with tempfile.TemporaryDirectory(prefix="acp-history-") as location:
                work = Path(location)
                source = work / "source"
                tree = snapshot(repo, args.tip, source)
                scanner = Scanner(work)
                scanner.directory_scan(source)
                scan_history(repo, args.tip, args.public_base, source, scanner)
                save(output / "history.json", {"tip": args.tip, "tree": tree, "public_base": args.public_base,
                     "scanner": SCANNER_VERSION, "content_rules_sha256": CONTENT_RULES_SHA256})
            print("Source and complete pre-squash history scans passed; retain the reviewed backup before squashing.")
    except GateError as error:
        print(str(error), file=sys.stderr)
        return 1
    except (OSError, ValueError, KeyError, TypeError, tarfile.TarError, zipfile.BadZipFile):
        print("Required local input is missing or malformed; no private diagnostics were emitted.", file=sys.stderr)
        return 1
    return 0


if __name__ == "__main__":
    sys.exit(main())
