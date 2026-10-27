#!/usr/bin/env python3
"""Split this project's initial snapshot into six logical Git commits.

Preview: python3 scripts/commit-initial.py
Apply:   python3 scripts/commit-initial.py --apply

This is a one-time helper for an empty repository. It preserves normal Git
identity, dates, signing, and hooks, and never resets history or pushes a remote.
"""

import argparse
import hashlib
from pathlib import Path, PurePosixPath
import subprocess
import sys


ROOT = Path(__file__).resolve().parents[1]
MESSAGES = [
    "Define workflow domain rules and initialize Go module",
    "Implement durable PostgreSQL workflows and action delivery",
    "Expose tenant-scoped HTTP API and server lifecycle",
    "Test end-to-end workflows, concurrency, and recovery",
    "Add Docker setup, reproducible demo, and CI checks",
    "Document architecture, tradeoffs, verification, and AI usage",
]


def group_for(name):
    """Allow only project source/document types; never stage whole directories."""
    path = PurePosixPath(name)
    parent = str(path.parent)
    if name in {".gitignore", "go.mod", "go.sum"} or (
            parent == "internal/domain" and path.suffix == ".go"):
        return 0
    if (parent in {"internal/app", "internal/postgres"} and path.suffix == ".go"
            or parent == "internal/postgres/migrations" and path.suffix == ".sql"):
        return 1
    if parent in {"internal/httpapi", "cmd/server"} and path.suffix == ".go":
        return 2
    if (parent == "integration" and path.suffix == ".go"
            or parent == "scripts" and path.name.startswith("e2e_")
            and path.suffix == ".py"):
        return 3
    if name in {".dockerignore", ".env.example", "Dockerfile", "compose.yaml",
                "Makefile", "scripts/demo.py", "scripts/commit-initial.py"} or (
            parent == ".github/workflows" and path.suffix in {".yml", ".yaml"}):
        return 4
    if name == "README.md" or (parent == "docs" and (
            path.suffix == ".md" or path.name.endswith("-results.json"))):
        return 5
    return None


def git(*args, check=True):
    return subprocess.run(
        ["git", *args], cwd=ROOT, check=check,
        stdout=subprocess.PIPE, stderr=subprocess.PIPE,
    )


def text(*args):
    return git(*args).stdout.decode().strip()


def paths(*args):
    return {p.decode("utf-8") for p in git(*args).stdout.split(b"\0") if p}


def require(condition, message):
    if not condition:
        raise RuntimeError(message)


def preflight():
    require(Path(text("rev-parse", "--show-toplevel")).resolve() == ROOT,
            "Run this helper from its original repository.")
    require(git("rev-parse", "--verify", "HEAD", check=False).returncode != 0
            and not text("for-each-ref", "--format=%(refname)"),
            "This helper requires an empty Git history on every ref.")
    require(not git("ls-files", "--cached", "-z").stdout,
            "The index must be empty. Inspect git status before continuing.")
    for identity in ("GIT_AUTHOR_IDENT", "GIT_COMMITTER_IDENT"):
        require(git("var", identity, check=False).returncode == 0,
                "Configure your Git user.name and user.email first.")
    actual = paths("ls-files", "--others", "--exclude-standard", "-z")
    groups = [(message, []) for message in MESSAGES]
    unexpected = []
    for path in sorted(actual):
        group = group_for(path)
        if group is None:
            unexpected.append(path)
        else:
            groups[group][1].append(path)
    require(not unexpected,
            "Unexpected untracked files; update the allowlist explicitly: "
            + ", ".join(unexpected))
    require({".gitignore", "go.mod", "go.sum", "scripts/commit-initial.py"} <= actual,
            "The initial project files or helper are missing/ignored.")
    for message, files in groups:
        require(files, "No project files found for group: " + message)
    for path in actual:
        require((ROOT / path).is_file() and not (ROOT / path).is_symlink(),
                "Missing regular project file: " + path)
    snapshot = {path: hashlib.sha256((ROOT / path).read_bytes()).digest()
                for path in actual}
    blobs = {path: text("hash-object", "--path=" + path, "--", path)
             for path in actual}
    return groups, snapshot, blobs


def unchanged(snapshot):
    require(paths("ls-files", "--cached", "--others", "--exclude-standard", "-z")
            == set(snapshot), "The project file set changed; inspect git status.")
    for path, digest in snapshot.items():
        require((ROOT / path).is_file() and not (ROOT / path).is_symlink()
                and hashlib.sha256((ROOT / path).read_bytes()).digest() == digest,
                "A project file changed during execution: " + path)


def verify_blobs(files, expected, commit=None):
    if commit:
        output = git("ls-tree", "-r", "-z", commit, "--", *files).stdout
    else:
        output = git("ls-files", "--stage", "-z", "--", *files).stdout
    entries = {}
    for entry in output.split(b"\0"):
        if not entry:
            continue
        metadata, raw_path = entry.split(b"\t", 1)
        first, second, third = metadata.decode().split()
        mode, blob = first, third if commit else second
        require(mode in {"100644", "100755"}, "Unexpected Git file mode.")
        if not commit:
            require(third == "0", "Unmerged index entry.")
        entries[raw_path.decode()] = blob
    require(entries == {path: expected[path] for path in files},
            "Staged/committed content differs from the initial snapshot; "
            "a concurrent edit or Git hook changed it.")


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    mode = parser.add_mutually_exclusive_group()
    mode.add_argument("--apply", action="store_true", help="create the six commits")
    mode.add_argument("--dry-run", action="store_true", help="preview only (default)")
    args = parser.parse_args()
    completed = []
    try:
        groups, snapshot, blobs = preflight()
        print("Mode: " + ("apply" if args.apply else "preview; use --apply to commit"),
              flush=True)
        print(f"Project files: {len(snapshot)}", flush=True)
        for label, identity in (("Author", "GIT_AUTHOR_IDENT"),
                                ("Committer", "GIT_COMMITTER_IDENT")):
            print(label + ": " + text("var", identity).rsplit(" ", 2)[0], flush=True)
        for number, (message, files) in enumerate(groups, 1):
            print(f"\n[{number}/{len(groups)}] {message}", flush=True)
            if not args.apply:
                for path in files:
                    print("  " + path)
                continue
            unchanged(snapshot)
            require(not paths("diff", "--name-only", "-z"),
                    "Previously committed files changed; inspect git status.")
            require(not paths("diff", "--cached", "--name-only", "-z"),
                    "Unexpected staged changes; inspect git status.")
            git("add", "--", *files)
            require(paths("diff", "--cached", "--name-only", "-z") == set(files),
                    "The staged paths do not match this commit group.")
            verify_blobs(files, blobs)
            git("diff", "--cached", "--check")
            result = git("commit", "-m", message)
            print(result.stdout.decode(), end="", flush=True)
            commit = text("rev-parse", "--short", "HEAD")
            completed.append(commit)
            require(paths("diff-tree", "--root", "--no-commit-id", "--name-only",
                          "-r", "-z", "HEAD") == set(files),
                    "A Git hook changed the committed file set; inspect the commit.")
            verify_blobs(files, blobs, commit=commit)
        if args.apply:
            require(not text("status", "--porcelain=v1", "--untracked-files=all"),
                    "Commits created, but the working tree is not clean; inspect git status.")
            print("\nCreated six logical commits. No remote was pushed.")
            print(text("log", "--reverse", "--format=%h %s"))
    except (RuntimeError, OSError, UnicodeError, subprocess.CalledProcessError) as exc:
        print("\nStopped: " + str(exc), file=sys.stderr)
        if isinstance(exc, subprocess.CalledProcessError) and exc.stderr:
            print(exc.stderr.decode(errors="replace"), file=sys.stderr)
        if completed:
            print("Completed commits: " + ", ".join(completed), file=sys.stderr)
        print("Existing commits and staged changes were preserved. Inspect git status; "
              "this helper does not roll back or rewrite history.", file=sys.stderr)
        return 1
    return 0


if __name__ == "__main__":
    sys.exit(main())
