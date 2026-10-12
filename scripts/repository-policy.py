#!/usr/bin/env python3
"""Plan, apply, and read back the approved GitHub repository policy."""
import argparse
import json
import os
import pathlib
import subprocess
import urllib.error

from ci_common import CHECK_APP_ID, GitHub, WORKFLOW, require

POLICY = pathlib.Path(__file__).resolve().parents[1] / ".github/repository-policy.json"


def subset(actual, expected):
    if isinstance(expected, dict):
        return isinstance(actual, dict) and all(key in actual and subset(actual[key], value) for key, value in expected.items())
    if isinstance(expected, list):
        return isinstance(actual, list) and len(actual) == len(expected) and all(
            any(subset(item, value) for item in actual) for value in expected)
    return actual == expected


def inspect(api, policy):
    repository = api.get("")
    settings = {
        "repository": repository,
        "actions": api.get("actions/permissions"),
        "workflow_permissions": api.get("actions/permissions/workflow"),
    }
    settings["repository"]["visibility"] = "private" if repository["private"] else "public"
    if settings["actions"]["allowed_actions"] == "selected":
        settings["allowed_actions"] = api.get("actions/permissions/selected-actions")
    else:
        settings["allowed_actions"] = {}
    settings["rulesets"] = [api.get(f"rulesets/{item['id']}") for item in api.pages("rulesets?includes_parents=true")]
    try:
        settings["classic_protection"] = api.get("branches/main/protection")
    except urllib.error.HTTPError as error:
        if error.code != 404:
            raise
        settings["classic_protection"] = None
    mismatches = [key for key in policy if not subset(settings[key], policy[key])]
    if settings["classic_protection"] is not None:
        mismatches.append("overlapping classic branch protection")
    return settings, mismatches


def apply(api, policy):
    settings, _ = inspect(api, policy)
    require(settings["classic_protection"] is None, "resolve overlapping classic protection before applying rulesets")
    require(settings["repository"]["permissions"]["admin"], "repository administrator access required")
    expected_names = {item["name"] for item in policy["rulesets"]}
    require(all(item["name"] in expected_names and item["source_type"] == "Repository"
                for item in settings["rulesets"]), "unexpected/inherited ruleset needs operator review")
    require(len({item["name"] for item in settings["rulesets"]}) == len(settings["rulesets"]), "duplicate policy rulesets")
    # Read a real emitted check to establish integration identity before binding main.
    runs = [run for run in api.pages("actions/runs?event=pull_request", "workflow_runs")
            if run["path"] == WORKFLOW and run["repository"]["id"] == settings["repository"]["id"]]
    verified_app = False
    for run in runs[:10]:
        jobs = api.pages(f"actions/runs/{run['id']}/jobs", "jobs")
        for job in jobs:
            if job["name"] == "CI":
                check_id = int(job["check_run_url"].rsplit("/", 1)[1])
                check = api.get(f"check-runs/{check_id}")
                require(check["app"]["slug"] == "github-actions" and check["app"]["id"] == CHECK_APP_ID,
                        "emitted CI check does not have expected GitHub Actions integration")
                verified_app = True
                break
        if verified_app:
            break
    require(verified_app, "a PR CI run must emit CI before binding the required integration")
    prefix = api.prefix
    api.request(prefix.rstrip("/"), "PATCH", policy["repository"])
    api.request(prefix + "actions/permissions", "PUT", policy["actions"])
    api.request(prefix + "actions/permissions/selected-actions", "PUT", policy["allowed_actions"])
    api.request(prefix + "actions/permissions/workflow", "PUT", policy["workflow_permissions"])
    existing = {item["name"]: item for item in settings["rulesets"]}
    for ruleset in policy["rulesets"]:
        if ruleset["name"] in existing:
            api.request(prefix + f"rulesets/{existing[ruleset['name']]['id']}", "PUT", ruleset)
        else:
            api.request(prefix + "rulesets", "POST", ruleset)
    _, mismatches = inspect(api, policy)
    require(not mismatches, "policy read-back failed: " + ", ".join(mismatches))


def main():
    parser = argparse.ArgumentParser()
    parser.add_argument("mode", choices=["plan", "inspect", "apply"])
    parser.add_argument("--repository", default=os.environ.get("GITHUB_REPOSITORY", "calmcacil/sonarr-remediator"))
    args = parser.parse_args()
    policy = json.loads(POLICY.read_text())
    if args.mode == "plan":
        print(json.dumps(policy, indent=2))
        return
    if not (os.environ.get("GH_TOKEN") or os.environ.get("GITHUB_TOKEN")):
        os.environ["GH_TOKEN"] = subprocess.check_output(["gh", "auth", "token"], text=True).strip()
    api = GitHub(args.repository)
    if args.mode == "apply":
        apply(api, policy)
    settings, mismatches = inspect(api, policy)
    output = {"repository": args.repository, "repository_id": settings["repository"]["id"],
              "mismatches": mismatches, "actions": settings["actions"],
              "workflow_permissions": settings["workflow_permissions"], "rulesets": settings["rulesets"]}
    print(json.dumps(output, indent=2))
    require(not mismatches, "repository does not yet satisfy the approved policy")


if __name__ == "__main__":
    try:
        main()
    except ValueError as error:
        raise SystemExit(str(error)) from None
