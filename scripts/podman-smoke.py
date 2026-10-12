#!/usr/bin/env python3
"""Native rootless acceptance against a disposable Sonarr fixture."""
import argparse
import json
import os
import pathlib
import platform
import subprocess
import tempfile
import time
import uuid


def run(*args, check=True):
    if args[0] == "podman":
        args = ("podman", "--cgroup-manager=cgroupfs", "--events-backend=file", *args[1:])
    result = subprocess.run(args, text=True, capture_output=True)
    if check and result.returncode:
        raise RuntimeError("command failed: " + " ".join(args) + "\n" + result.stderr)
    return result


def main():
    parser = argparse.ArgumentParser()
    parser.add_argument("--image", required=True)
    parser.add_argument("--architecture", required=True, choices=["amd64", "arm64"])
    parser.add_argument("--output", default="artifacts/runtime.json")
    args = parser.parse_args()
    architecture = {"aarch64": "arm64", "x86_64": "amd64"}.get(platform.machine())
    assert architecture == args.architecture, "native runner architecture mismatch"
    assert os.getuid() != 0, "acceptance requires an unprivileged account"
    info = json.loads(run("podman", "info", "--format", "json").stdout)
    assert info["host"]["security"]["rootless"], "engine must be rootless"
    image = json.loads(run("podman", "image", "inspect", args.image).stdout)[0]
    assert image["Architecture"] == architecture
    assert image["Config"]["User"] == "65532:65532"
    nonce = "sra-smoke-" + uuid.uuid4().hex[:12]
    network, fake, app = nonce + "-net", nonce + "-fake", nonce + "-app"
    fixture_image = "localhost/" + nonce + "-fixture"
    checks = []
    with tempfile.TemporaryDirectory(prefix="sra-smoke-") as directory:
        root = pathlib.Path(directory)
        config_dir = root / "config"
        state_dir = root / "state"
        config_dir.mkdir(mode=0o700)
        state_dir.mkdir(mode=0o700)
        (config_dir / "key").write_text("smoke-secret-never-export\n")
        (config_dir / "key").chmod(0o600)
        (config_dir / "config.yaml").write_text(
            "sonarr:\n  url: http://sonarr:8989\n  api_key_file: /config/key\n"
            "poll_interval: 1s\nrecovery:\n  wait: 2s\n"
        )
        (config_dir / "config.yaml").chmod(0o600)
        try:
            run("podman", "build", "--format", "oci", "-f", "scripts/FakeContainerfile", "-t", fixture_image, ".")
            run("podman", "network", "create", network)
            run("podman", "run", "-d", "--name", fake, "--network", network,
                "--network-alias", "sonarr", "--read-only", "--cap-drop=all",
                "--security-opt=no-new-privileges", fixture_image)

            def start():
                run("podman", "run", "-d", "--name", app, "--network", network,
                    "--userns=keep-id:uid=65532,gid=65532", "--read-only", "--cap-drop=all",
                    "--security-opt=no-new-privileges", "--stop-timeout=30",
                    "-v", str(config_dir) + ":/config:ro", "-v", str(state_dir) + ":/state",
                    args.image, "run", "--config", "/config/config.yaml")

            def query(*command):
                return json.loads(run("podman", "exec", app, "/sonarr-remediator", *command, "--json").stdout)

            def ready():
                deadline = time.monotonic() + 30
                while time.monotonic() < deadline:
                    health = run("podman", "exec", app, "/sonarr-remediator", "healthcheck", check=False)
                    if health.returncode == 0 and query("actions"):
                        return
                    time.sleep(0.5)
                raise AssertionError("service failed readiness: " + run("podman", "logs", app).stderr)

            start()
            ready()
            status = query("status")
            assert status["dry_run"] and status["connectivity"] == "connected"
            issue = query("show", "fake-download")
            assert issue["state"] == "recoverable"
            assert issue["eligible_elapsed_ns"] >= 2_000_000_000
            assert len(query("issues")) == 1
            actions = query("actions", "--download-id", "fake-download")
            assert len(actions) == 1 and actions[0]["outcome"] == "recommended"
            assert run("podman", "exec", fake, "/fake-sonarr", "stats").stdout.strip() == "0"
            assert "smoke-secret-never-export" not in run("podman", "logs", app).stderr
            container = json.loads(run("podman", "inspect", app).stdout)[0]
            assert container["HostConfig"]["ReadonlyRootfs"]
            assert not container["NetworkSettings"].get("Ports")
            assert {m["Destination"] for m in container["Mounts"]} == {"/config", "/state"}
            assert run("podman", "exec", app, "/sonarr-remediator", "show", "unknown", check=False).returncode != 0
            checks += ["non-root", "private-state", "readonly-root-config", "no-ports-media-socket",
                       "dry-run-zero-mutations", "local-queries", "health", "recommendation-deduplication"]
            began = time.monotonic()
            run("podman", "stop", "--time", "30", app)
            shutdown = time.monotonic() - began
            assert shutdown < 25, "application shutdown exceeded deadline"
            run("podman", "rm", app)
            # The first post-restart observation cannot credit the stopped interval.
            time.sleep(3)
            start()
            ready()
            assert len(query("actions")) == 1
            assert query("show", "fake-download")["eligible_elapsed_ns"] >= issue["eligible_elapsed_ns"]
            assert run("podman", "exec", fake, "/fake-sonarr", "stats").stdout.strip() == "0"
            checks += ["restart-persistence", "bounded-shutdown"]
            # A config error and unreadable state must fail before observation/action.
            invalid = run("podman", "run", "--rm", "--network", network, "--read-only",
                          "--cap-drop=all", "--security-opt=no-new-privileges", args.image,
                          "run", "--config", "/missing.yaml", check=False)
            assert invalid.returncode != 0
            badstate = root / "badstate"
            badstate.mkdir(mode=0o500)
            invalid = run("podman", "run", "--rm", "--network", network,
                          "--userns=keep-id:uid=65532,gid=65532", "--read-only",
                          "--cap-drop=all", "--security-opt=no-new-privileges",
                          "-v", str(config_dir) + ":/config:ro", "-v", str(badstate) + ":/state",
                          args.image, "run", "--config", "/config/config.yaml", check=False)
            assert invalid.returncode != 0
            checks += ["config-permission-failure", "state-permission-failure"]
            output = pathlib.Path(args.output)
            output.parent.mkdir(parents=True, exist_ok=True)
            output.write_text(json.dumps({
                "architecture": architecture, "rootless": True, "uid": os.getuid(),
                "podman_version": info["version"]["Version"],
                "storage_driver": info["store"]["graphDriverName"],
                "image_config": "sha256:" + image["Id"].removeprefix("sha256:"), "version": run("podman", "run", "--rm", args.image, "version").stdout.strip(),
                "checks": checks, "shutdown_seconds": round(shutdown, 3),
            }, indent=2) + "\n")
            print(output.read_text())
        finally:
            run("podman", "rm", "-f", app, fake, check=False)
            run("podman", "network", "rm", network, check=False)
            run("podman", "rmi", fixture_image, check=False)
            if (root / "badstate").exists():
                (root / "badstate").chmod(0o700)


if __name__ == "__main__":
    main()
