"""Summarise a `go test -json` log for a reader who only needs the failures.

Usage: gotest_summary.py LOG STDERR EXIT_CODE SECONDS

Prints one line when everything passed. Otherwise prints build errors, then each
failing leaf test with its own output, capped so a runaway test can't flood the
reader. The full log stays on disk; the last line says where.
"""

import json
import sys
from collections import defaultdict

PER_TEST_LINES = 30
TOTAL_LINES = 150

NOISE_PREFIXES = ("=== RUN", "=== PAUSE", "=== CONT", "=== NAME", "--- FAIL", "--- PASS")
GOROOT_FRAMES = ("/src/runtime/", "/src/testing/")


def drop_goroot_frames(lines):
    """Remove stack frames inside the Go runtime and testing package.

    A frame is two lines: the function, then a tab-indented file:line. Only the
    frames in the user's own code say anything about the failure.
    """
    kept = []
    for line in lines:
        if line.startswith("\t") and any(g in line for g in GOROOT_FRAMES):
            if kept and not kept[-1].startswith("\t") and not kept[-1].startswith("goroutine"):
                kept.pop()
            continue
        kept.append(line)
    return kept


def plural(n: int, word: str) -> str:
    return f"{n} {word}" + ("" if n == 1 else "s")


def main() -> int:
    log_path, err_path, code, secs = sys.argv[1], sys.argv[2], int(sys.argv[3]), sys.argv[4]

    test_out = defaultdict(list)   # (pkg, test) -> output lines
    pkg_out = defaultdict(list)    # pkg -> package-level output lines
    build_out = defaultdict(list)  # import path -> build output lines
    result = {}                    # (pkg, test) -> pass/fail/skip
    pkg_result = {}                # pkg -> pass/fail/skip
    no_tests = set()

    with open(log_path, encoding="utf-8", errors="replace") as f:
        for raw in f:
            raw = raw.strip()
            if not raw.startswith("{"):
                continue
            try:
                ev = json.loads(raw)
            except json.JSONDecodeError:
                continue
            action = ev.get("Action")
            pkg = ev.get("Package", "")
            test = ev.get("Test")
            out = ev.get("Output", "")
            if action == "build-output":
                build_out[ev.get("ImportPath", pkg)].append(out.rstrip("\n"))
            elif action == "output":
                line = out.rstrip("\n")
                if test:
                    if not line.lstrip().startswith(NOISE_PREFIXES):
                        test_out[(pkg, test)].append(line)
                else:
                    if "[no test files]" in line:
                        no_tests.add(pkg)
                    pkg_out[pkg].append(line)
            elif action in ("pass", "fail", "skip"):
                if test:
                    result[(pkg, test)] = action
                else:
                    pkg_result[pkg] = action

    passed = sum(1 for r in result.values() if r == "pass")
    skipped = sum(1 for r in result.values() if r == "skip")
    failed = [k for k, r in result.items() if r == "fail"]
    # Report only leaf failures: a parent test fails whenever a subtest does.
    leaves = [
        (p, t) for (p, t) in failed
        if not any(p2 == p and t2.startswith(t + "/") for (p2, t2) in failed)
    ]
    failed_pkgs = [p for p, r in pkg_result.items() if r == "fail"]
    tested_pkgs = len([p for p in pkg_result if p not in no_tests])

    if code == 0 and not failed and not failed_pkgs:
        print(f"test: ok — {passed} passed, {skipped} skipped, "
              f"{plural(tested_pkgs, 'package')} ({secs}s)")
        return 0

    budget = [TOTAL_LINES]

    def emit(line: str) -> bool:
        if budget[0] <= 0:
            return False
        print(line)
        budget[0] -= 1
        return True

    print(f"test: FAILED — {plural(len(leaves), 'failing test')}, {passed} passed, "
          f"{plural(len(failed_pkgs), 'failing package')} ({secs}s)")

    for path, lines in build_out.items():
        emit(f"\n## build failed: {path}")
        for line in lines[:PER_TEST_LINES]:
            emit(line)

    for pkg, test in leaves:
        emit(f"\n## {test}  ({pkg})")
        lines = drop_goroot_frames([l for l in test_out[(pkg, test)] if l.strip()])
        for line in lines[:PER_TEST_LINES]:
            emit(line)
        if len(lines) > PER_TEST_LINES:
            emit(f"   … {len(lines) - PER_TEST_LINES} more lines")

    # Packages that failed without a failing test: panic in init/TestMain, timeout, vet.
    for pkg in failed_pkgs:
        built_badly = any(path == pkg or path.startswith(pkg + " ") for path in build_out)
        if any(p == pkg for p, _ in leaves) or built_badly:
            continue
        lines = drop_goroot_frames(
            [l for l in pkg_out[pkg] if l.strip() and not l.startswith(("FAIL", "ok "))])
        emit(f"\n## package failed: {pkg}")
        for line in lines[-PER_TEST_LINES:]:
            emit(line)

    # Nothing parsed but go test still failed: show its stderr (bad flags, no packages).
    if not failed and not failed_pkgs and not build_out:
        with open(err_path, encoding="utf-8", errors="replace") as f:
            for line in f.read().splitlines()[-PER_TEST_LINES:]:
                emit(line)

    if budget[0] <= 0:
        print("… output capped")
    print(f"\nfull log: {log_path} (grep it; don't cat it)")
    return 1


if __name__ == "__main__":
    sys.exit(main())
