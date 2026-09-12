import contextlib
from dataclasses import replace
import io
from pathlib import Path
import subprocess
import tempfile
import unittest
from unittest.mock import patch
from urllib.error import HTTPError

from scripts.publish_release import Build, GitHubAPI, publish, release_channel


SHA = "a" * 40
BUILD = Build("owner/repo", "refs/heads/main", SHA, "0.1.0-build.42.1.gaaaaaaa", "v0.1.0-build.42.1.gaaaaaaa")


class PublicationTests(unittest.TestCase):
    def setUp(self):
        self.tmp = tempfile.TemporaryDirectory()
        self.addCleanup(self.tmp.cleanup)
        self.dist = Path(self.tmp.name)
        (self.dist / f"cpa-vibeguard_{BUILD.version}_linux_amd64.zip").write_bytes(b"verified artifact")
        (self.dist / "checksums.txt").write_text("verified checksum\n")
        self.responses = {
            "repos/owner/repo/releases/tags/" + BUILD.tag: None,
            "repos/owner/repo": {"default_branch": "main"},
            "repos/owner/repo/branches/main": {"commit": {"sha": SHA}},
            "repos/owner/repo/releases/latest": {"tag_name": "v0.1.0-build.41.1.gbbbbbbb"},
        }
        self.calls = []
        self.timeline = []
        self.failure = None

    def get(self, path, **kwargs):
        self.timeline.append(path)
        result = self.responses[path]
        if isinstance(result, Exception):
            raise result
        return result

    def gh(self, command):
        self.calls.append(command)
        self.timeline.append(command[2])
        if command[2] == self.failure:
            raise subprocess.CalledProcessError(1, command)
        if command[2] == "create":
            notes = Path(command[command.index("--notes-file") + 1])
            self.assertIn(SHA, notes.read_text())

    def invoke(self, build=BUILD):
        with contextlib.redirect_stdout(io.StringIO()):
            publish(build, self, self.gh, self.dist)

    def assert_channel(self, stable):
        edit = self.calls[-1]
        self.assertEqual(edit[:3], ["gh", "release", "edit"])
        self.assertIn("--draft=false", edit)
        self.assertIn("--latest=" + str(stable).lower(), edit)
        self.assertIn("--prerelease=" + str(not stable).lower(), edit)

    def test_current_default_branch_promotes_only_after_upload_and_live_checks(self):
        self.invoke()
        create = self.calls[0]
        self.assertIn("--draft", create)
        self.assertIn("--prerelease", create)
        self.assertIn("--latest=false", create)
        self.assertLess(self.timeline.index("create"), self.timeline.index("repos/owner/repo/branches/main"))
        self.assertLess(self.timeline.index("repos/owner/repo/releases/latest"), self.timeline.index("edit"))
        self.assert_channel(True)

    def test_feature_branch_and_tag_never_promote_even_at_default_head(self):
        for ref in ("refs/heads/feature", "refs/tags/v0.1.0"):
            with self.subTest(ref=ref):
                self.calls.clear()
                self.invoke(replace(BUILD, ref=ref))
                self.assert_channel(False)

    def test_old_commit_finishing_late_is_preview(self):
        self.responses["repos/owner/repo/branches/main"]["commit"]["sha"] = "b" * 40
        self.invoke()
        self.assert_channel(False)

    def test_run_and_attempt_order_prevent_same_commit_regression(self):
        for tag in ("v0.1.0-build.43.1.gaaaaaaa", "v0.1.0-build.42.2.gaaaaaaa", BUILD.tag):
            with self.subTest(latest=tag):
                self.responses["repos/owner/repo/releases/latest"]["tag_name"] = tag
                self.invoke()
                self.assert_channel(False)
        newer = replace(BUILD, version="0.1.0-build.42.3.gaaaaaaa", tag="v0.1.0-build.42.3.gaaaaaaa")
        self.assertTrue(release_channel(newer, "main", SHA, "v0.1.0-build.42.2.gaaaaaaa")[0])

    def test_repository_default_branch_is_queried_instead_of_assuming_main(self):
        self.responses["repos/owner/repo"]["default_branch"] = "stable/branch"
        self.responses["repos/owner/repo/branches/stable%2Fbranch"] = {"commit": {"sha": SHA}}
        self.invoke(replace(BUILD, ref="refs/heads/stable/branch"))
        self.assert_channel(True)

    def test_first_release_without_latest_can_promote(self):
        self.responses["repos/owner/repo/releases/latest"] = None
        self.invoke()
        self.assert_channel(True)

    def test_state_query_failure_leaves_release_draft(self):
        self.responses["repos/owner/repo/releases/latest"] = RuntimeError("API unavailable")
        with self.assertRaises(RuntimeError):
            self.invoke()
        self.assertEqual([command[2] for command in self.calls], ["create"])

    def test_failed_upload_does_not_publish(self):
        self.failure = "create"
        with self.assertRaises(subprocess.CalledProcessError):
            self.invoke()
        self.assertEqual([command[2] for command in self.calls], ["create"])

    def test_retry_repairs_matching_draft_then_checks_promotion_again(self):
        self.responses["repos/owner/repo/releases/tags/" + BUILD.tag] = {"draft": True, "target_commitish": SHA}
        self.invoke()
        self.assertEqual([command[2] for command in self.calls], ["upload", "edit"])
        self.assert_channel(True)

    def test_retry_never_changes_published_release_or_another_commit(self):
        endpoint = "repos/owner/repo/releases/tags/" + BUILD.tag
        self.responses[endpoint] = {"draft": False, "target_commitish": SHA}
        self.invoke()
        self.assertEqual(self.calls, [])
        self.responses[endpoint] = {"draft": True, "target_commitish": "b" * 40}
        with self.assertRaises(ValueError):
            self.invoke()
        self.assertEqual(self.calls, [])


class InputAndAPITests(unittest.TestCase):
    def test_publish_only_retry_reuses_original_verified_version(self):
        env = {
            "GITHUB_EVENT_NAME": "workflow_dispatch", "GH_REPO": BUILD.repo,
            "GITHUB_REF": BUILD.ref, "GITHUB_SHA": SHA,
            "VERSION": BUILD.version, "RELEASE_TAG": BUILD.tag,
            "GITHUB_RUN_NUMBER": "42", "GITHUB_RUN_ATTEMPT": "2",
        }
        self.assertEqual(Build.from_environment(env), BUILD)
        env["GITHUB_EVENT_NAME"] = "pull_request"
        with self.assertRaises(ValueError):
            Build.from_environment(env)

    def test_mismatched_artifact_metadata_is_rejected(self):
        for changes in ({"tag": "v0.1.0"}, {"sha": "b" * 40}, {"version": "../asset"}):
            with self.subTest(changes=changes), self.assertRaises(ValueError):
                replace(BUILD, **changes)

    def test_only_http_404_is_treated_as_missing(self):
        api = GitHubAPI("test-token")
        for status in (403, 404, 500):
            with self.subTest(status=status):
                error = HTTPError("https://api.github.com/repos/owner/repo/releases/latest", status, "error", {}, None)
                with patch("scripts.publish_release.urlopen", side_effect=error):
                    if status == 404:
                        self.assertIsNone(api.get("repos/owner/repo/releases/latest", missing_ok=True))
                    else:
                        with self.assertRaises(HTTPError):
                            api.get("repos/owner/repo/releases/latest", missing_ok=True)


if __name__ == "__main__":
    unittest.main()
