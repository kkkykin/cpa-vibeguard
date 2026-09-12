#!/usr/bin/env python3
"""Publish verified assets; the workflow serializes all calls to this script."""

from dataclasses import dataclass
import json
import os
from pathlib import Path
import re
import subprocess
import sys
import tempfile
from urllib.error import HTTPError
from urllib.parse import quote
from urllib.request import Request, urlopen


VERSION = re.compile(r"[0-9]+\.[0-9]+\.[0-9]+-build\.([1-9][0-9]*)\.([1-9][0-9]*)\.g([0-9a-f]{7})")


@dataclass(frozen=True)
class Build:
    repo: str
    ref: str
    sha: str
    version: str
    tag: str

    def __post_init__(self):
        match = VERSION.fullmatch(self.version)
        if not re.fullmatch(r"[A-Za-z0-9_.-]+/[A-Za-z0-9_.-]+", self.repo):
            raise ValueError("invalid repository")
        if not re.fullmatch(r"[0-9a-f]{40}", self.sha):
            raise ValueError("invalid commit SHA")
        if match is None or match[3] != self.sha[:7] or self.tag != "v" + self.version:
            raise ValueError("release version, tag and commit do not agree")

    @property
    def order(self):
        match = VERSION.fullmatch(self.version)
        return int(match[1]), int(match[2])

    @classmethod
    def from_environment(cls, env):
        if env["GITHUB_EVENT_NAME"] not in {"push", "workflow_dispatch"}:
            raise ValueError("this event cannot publish releases")
        build = cls(env["GH_REPO"], env["GITHUB_REF"], env["GITHUB_SHA"], env["VERSION"], env["RELEASE_TAG"])
        # A retry of only the publish job reuses the successful verify job's
        # immutable artifact and version, including its original attempt.
        if build.order[0] != int(env["GITHUB_RUN_NUMBER"]) or build.order[1] > int(env["GITHUB_RUN_ATTEMPT"]):
            raise ValueError("release version does not belong to this workflow run")
        return build


class GitHubAPI:
    def __init__(self, token, base="https://api.github.com"):
        self.token = token
        self.base = base.rstrip("/")

    def get(self, path, *, missing_ok=False):
        request = Request(self.base + "/" + path, headers={
            "Accept": "application/vnd.github+json",
            "Authorization": "Bearer " + self.token,
            "X-GitHub-Api-Version": "2022-11-28",
        })
        try:
            with urlopen(request, timeout=30) as response:
                return json.load(response)
        except HTTPError as error:
            if missing_ok and error.code == 404:
                return None
            raise


def release_channel(build, default_branch, head_sha, latest_tag):
    if build.ref != "refs/heads/" + default_branch:
        return False, "branch or tag preview"
    if build.sha != head_sha:
        return False, "default branch has advanced"
    # Automatic releases carry an order even when two runs build the same SHA.
    latest = VERSION.fullmatch(latest_tag.removeprefix("v"))
    if latest and build.order <= (int(latest[1]), int(latest[2])):
        return False, "an equal or newer build is already Latest"
    return True, "current default branch build"


def publish(build, api, run, dist=Path("dist")):
    archive = dist / f"cpa-vibeguard_{build.version}_linux_amd64.zip"
    checksums = dist / "checksums.txt"
    if not archive.is_file() or not checksums.is_file():
        raise ValueError("verified release assets are missing")
    endpoint = "repos/" + build.repo
    existing = api.get(endpoint + "/releases/tags/" + quote(build.tag, safe=""), missing_ok=True)
    if existing:
        if existing["target_commitish"] != build.sha:
            raise ValueError("existing release targets a different commit")
        if not existing["draft"]:
            print("Release already published; preserving its channel.")
            return
        # Repair an interrupted upload only while the matching release is draft.
        run(["gh", "release", "upload", build.tag, str(archive), str(checksums), "--clobber", "--repo", build.repo])
    else:
        with tempfile.TemporaryDirectory(prefix="vibeguard-release-") as directory:
            notes = Path(directory) / "notes.md"
            notes.write_text(
                f"Automated build from commit {build.sha}.\n\n"
                "Platform: Linux/amd64 (glibc), built on Ubuntu 22.04.\n"
                "The ZIP contains cpa-vibeguard.so at its root; checksums.txt contains its SHA-256 checksum.\n",
                encoding="utf-8",
            )
            run([
                "gh", "release", "create", build.tag, str(archive), str(checksums),
                "--repo", build.repo, "--target", build.sha,
                "--title", "cpa-vibeguard " + build.tag, "--notes-file", str(notes),
                "--draft", "--prerelease", "--latest=false",
            ])

    # Read live state AFTER all uploads, immediately before making it public.
    default_branch = api.get(endpoint)["default_branch"]
    stable, reason = False, "branch or tag preview"
    if build.ref == "refs/heads/" + default_branch:
        head = api.get(endpoint + "/branches/" + quote(default_branch, safe=""))["commit"]["sha"]
        latest = api.get(endpoint + "/releases/latest", missing_ok=True)
        stable, reason = release_channel(build, default_branch, head, latest["tag_name"] if latest else "")
    print(f"Publishing {'stable Latest' if stable else 'preview'}: {reason}.")
    run([
        "gh", "release", "edit", build.tag, "--repo", build.repo, "--draft=false",
        "--prerelease=" + str(not stable).lower(), "--latest=" + str(stable).lower(),
    ])


def main():
    build = Build.from_environment(os.environ)
    api = GitHubAPI(os.environ["GH_TOKEN"], os.environ.get("GITHUB_API_URL", "https://api.github.com"))
    publish(build, api, lambda command: subprocess.run(command, check=True))


if __name__ == "__main__":
    try:
        main()
    except (KeyError, ValueError, OSError, subprocess.CalledProcessError) as error:
        sys.exit(f"Release failed: {error}")
