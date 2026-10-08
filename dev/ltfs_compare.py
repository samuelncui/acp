#!/usr/bin/env python3
"""Compare committed ACP pipelines with one LTFS model and explicit historical I/O adapters."""

import argparse
import difflib
import json
import os
from pathlib import Path
import platform
import re
import tempfile

from release import (commit, digest, go_environment, harness_files, private_output,
                     require, run, save, snapshot)


BASELINES = {
    "4d5dbd0b8c06c5d53f369820ac9cff20e779821f",  # Published v0.2.3.
}
MODEL_FILES = ("ltfs_mock_test.go", "tape_drive_test.go", "tape_speed_test.go", "ltfs_benchmark_test.go")
SELECTOR = "^BenchmarkLTFSPipeline$/^(native47|stress100)$/^(small|large|mixed|small-delayed-source)$/^pipeline$"


def replace_once(text, old, new):
    require(text.count(old) == 1, "Historical I/O adapter anchor changed.")
    return text.replace(old, new)


def adapt(source, adapter):
    """Change only native I/O dispatch; retain historical scheduling and completion."""
    changes = {
        "acp.go": [("type StreamCopyer struct {", "type StreamCopyer struct {\n\tfilesystem transferFilesystem")],
        "prepare.go": [("openSourceContent(job.path, mode, job.stat.info)", "c.fs().Open(job.path, mode, job.stat.info)")],
        "copy.go": [("c.prepareTarget(job, target)", "c.fs().Create(job, target)")],
        "target.go": [
            ("target targetSpec, out *fileio.Output, chunks", "target targetSpec, out transferOutput, chunks"),
            ("writeChunk(out.File, chunk)", "writeChunk(out, chunk)"),
            ("c.refreshCacheEntry(out.File, target.name, job.baseJob, true)", "out.Cache(job.baseJob, c.toDevice.linear)"),
            ("restoreTarget(out.Temporary, job.stat)", "out.Restore(job.stat)"),
            ("out.File.Sync()", "out.Sync()"),
            ("commitTarget(out, c.createFlag&os.O_TRUNC != 0)", "out.Commit(c.createFlag&os.O_TRUNC != 0)"),
        ],
    }
    patch = []
    for name, replacements in changes.items():
        path = source / name
        before = path.read_text()
        after = before
        for old, new in replacements:
            after = replace_once(after, old, new)
        path.write_text(after)
        patch.extend(difflib.unified_diff(before.splitlines(True), after.splitlines(True), "a/" + name, "b/" + name))
    # Historical targets still own an open descriptor at Cache, including linear outputs.
    adapter = re.sub(r"(?ms)^func \(o \*nativeTransferOutput\) Cache\(.*?^}\n", """func (o *nativeTransferOutput) Cache(job *baseJob, _ bool) {
\to.copyer.refreshCacheEntry(o.output.File, o.target.name, job, true)
}
""", adapter)
    require("refreshCachePath" not in adapter, "Adapter must preserve historical descriptor cache writes.")
    (source / "filesystem.go").write_text(adapter)
    patch.extend(difflib.unified_diff([], adapter.splitlines(True), "/dev/null", "b/filesystem.go"))
    return "".join(patch)


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--baseline", action="append", required=True)
    parser.add_argument("--candidate", required=True)
    parser.add_argument("--harness", required=True)
    parser.add_argument("--out", required=True)
    parser.add_argument("--idle", action="store_true")
    parser.add_argument("--scenario", choices=("ordinary", "read-delay"), default="ordinary")
    args = parser.parse_args()
    selector, expected = SELECTOR, 7
    if args.scenario == "read-delay":
        selector, expected = "^BenchmarkLTFSPipeline$/^stress100$/^medium-delayed-read$/^pipeline$", 1
    require(args.idle, "Stop other builds and benchmarks before measuring.")
    require(set(args.baseline) <= BASELINES, "Historical adapter is reviewed only for the listed exact revisions.")
    repo = Path.cwd()
    for revision in [*args.baseline, args.candidate, args.harness]:
        commit(repo, revision)
    output = private_output(repo, args.out)
    files = {name: run(["git", "show", args.candidate + ":" + name], cwd=repo) for name in MODEL_FILES}
    adapter = run(["git", "show", args.candidate + ":filesystem.go"], cwd=repo).decode()
    native = harness_files(repo, args.harness)
    record = {"complete": False, "candidate": args.candidate, "baselines": args.baseline,
              "harness": args.harness, "harness_files": {name: digest(data) for name, data in native.items()},
              "scenario": args.scenario, "model_files": {name: digest(data) for name, data in files.items()},
              "collector_sha256": digest(Path(__file__).read_bytes()), "runs": [],
              "note": "Historical snapshots use recorded I/O-only adapters. Scheduling and completion remain historical. Model gains are not hardware gains."}
    save(output / "comparison.json", record)
    with tempfile.TemporaryDirectory(prefix=".ltfs-compare-", dir=output.parent) as directory:
        work = Path(directory)
        temporary = work / "tmp"
        temporary.mkdir()
        for revision in [*args.baseline, args.candidate]:
            label = revision[:12]
            source = work / label
            tree = snapshot(repo, revision, source)
            for path in source.glob("*_test.go"):
                path.unlink()
            patch = ""
            if revision in BASELINES:
                patch = adapt(source, adapter)
                (output / (label + ".adapter.patch")).write_text(patch)
            for name, data in {**native, **files}.items():
                (source / name).write_bytes(data)
            go, env, info = go_environment(source, temporary, cpu=4)
            env["ACP_MOCK_REPORT_DIR"] = str(output / label)
            # Native fallback exercises the mechanical adapter before timed model comparisons.
            smoke = [go, "test", "-run=^$", "-bench=^BenchmarkCopyWorkload$/^SmallOneTarget$", "-benchtime=1x", "-count=1", "-cpu=4", "."]
            run(smoke, cwd=source, env=env, log=output / (label + ".native.log"))
            command = [go, "test", "-run=^$", "-bench=" + selector, "-benchtime=1x", "-count=1", "-benchmem", "-cpu=4", "-p=1", "-timeout=15m", "."]
            entry = {"revision": revision, "tree": tree, "adapter_sha256": digest(patch.encode()),
                     "command": list(map(str, command)), "go": info, "host": platform.platform(),
                     "filesystem_device": temporary.stat().st_dev, "load": os.getloadavg(), "complete": False}
            record["runs"].append(entry)
            save(output / "comparison.json", record)
            data = run(command, cwd=source, env=env, log=output / (label + ".bench"))
            names = re.findall(r"^BenchmarkLTFSPipeline/([^\s]+)\s+1\s+", data.decode(), re.M)
            require(len(names) == expected and len(set(names)) == expected and "\nPASS\n" in data.decode(), "Incomplete model benchmark coverage.")
            entry.update(complete=True, output_sha256=digest(data))
            save(output / "comparison.json", record)
    record["complete"] = True
    save(output / "comparison.json", record)
    print(json.dumps({"complete": True, "out": str(output)}))


if __name__ == "__main__":
    main()
