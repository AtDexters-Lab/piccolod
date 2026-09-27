#!/usr/bin/env python3
"""Build/serve a read-only OCI registry and inspect real VM rootfs attachments."""
import argparse
import gzip
import hashlib
import io
import json
import os
from pathlib import Path
import re
import signal
import subprocess
import sys
import tarfile
import time
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer


def encode(value):
    return json.dumps(value, separators=(",", ":"), sort_keys=True).encode()


def build(binary, out, arch):
    out.mkdir(parents=True, exist_ok=True)
    blobs = out / "blobs"
    blobs.mkdir()

    def blob(data, media):
        digest = hashlib.sha256(data).hexdigest()
        (blobs / digest).write_bytes(data)
        return {"mediaType": media, "digest": "sha256:" + digest, "size": len(data)}

    refs = {}
    for version in ("v1", "v2", "fail"):
        layer = io.BytesIO()
        with tarfile.open(fileobj=layer, mode="w", format=tarfile.USTAR_FORMAT) as tar:
            for name in ("etc", "data", "dev", "proc", "sys", "tmp"):
                if version == "fail" and name == "proc":
                    continue
                entry = tarfile.TarInfo(name)
                entry.type, entry.mode = tarfile.DIRTYPE, 0o755
                tar.addfile(entry)
            files = {"fixture": binary.read_bytes(), "image-marker": (version + "\n").encode()}
            # A regular file at /proc prevents the OCI runtime's proc mount
            # before PID 1 exists. Child-executable failure after --init starts
            # would not reliably fail the synchronous podman start operation.
            if version == "fail":
                files["proc"] = b"fixture pre-PID1 proc mount fault\n"
            files.update({"etc/" + name: b"" for name in ("hosts", "hostname", "resolv.conf", "mtab")})
            for name, data in files.items():
                entry = tarfile.TarInfo(name)
                entry.mode, entry.size = (0o755 if name == "fixture" else 0o644), len(data)
                tar.addfile(entry, io.BytesIO(data))
        plain = layer.getvalue()
        config = {"architecture": arch, "os": "linux", "config": {
            "Entrypoint": ["/fixture"],
            "WorkingDir": "/", "User": "0", "ExposedPorts": {"8080/tcp": {}}},
            "rootfs": {"type": "layers", "diff_ids": ["sha256:" + hashlib.sha256(plain).hexdigest()]}}
        manifest = {"schemaVersion": 2, "mediaType": "application/vnd.oci.image.manifest.v1+json",
                    "config": blob(encode(config), "application/vnd.oci.image.config.v1+json"),
                    "layers": [blob(gzip.compress(plain, mtime=0), "application/vnd.oci.image.layer.v1.tar+gzip")]}
        descriptor = blob(encode(manifest), manifest["mediaType"])
        refs[version] = descriptor["digest"]
    refs["v2-alias"] = refs["v2"]
    (out / "refs.json").write_bytes(encode(refs))


def serve(root):
    refs = json.loads((root / "refs.json").read_text())

    class Registry(BaseHTTPRequestHandler):
        def log_message(self, *_):
            pass

        def do_HEAD(self):
            self.do_GET(head=True)

        def do_GET(self, head=False):
            path = self.path.split("?", 1)[0]
            if path in ("/v2", "/v2/"):
                data, media, digest = b"{}", "application/json", None
            else:
                match = re.fullmatch(r"/v2/fixture/(manifests|blobs)/([A-Za-z0-9:._-]+)", path)
                if not match:
                    self.send_error(404)
                    return
                kind, reference = match.groups()
                digest = refs.get(reference, reference) if kind == "manifests" else reference
                if not re.fullmatch(r"sha256:[0-9a-f]{64}", digest):
                    self.send_error(404)
                    return
                target = root / "blobs" / digest.split(":")[1]
                if not target.is_file():
                    self.send_error(404)
                    return
                data = target.read_bytes()
                media = "application/vnd.oci.image.manifest.v1+json" if kind == "manifests" else "application/octet-stream"
            self.send_response(200)
            self.send_header("Docker-Distribution-API-Version", "registry/2.0")
            self.send_header("Content-Type", media)
            self.send_header("Content-Length", str(len(data)))
            if digest:
                self.send_header("Docker-Content-Digest", digest)
            self.end_headers()
            if not head:
                self.wfile.write(data)

    server = ThreadingHTTPServer(("127.0.0.1", 0), Registry)
    (root / "port").write_text(str(server.server_port))
    (root / "pid").write_text(str(os.getpid()))
    server.serve_forever()


def daemon_paths():
    # Match paths.resolveRoots and podmanRunRootBase, including daemon-specific
    # overrides. The podmanRuntimeForApp log prints layout.PodmanRoot, but the
    # returned runtime uses serviceRoot (the tmpfs per-app metadata graphroot).
    daemon_pid = int(subprocess.check_output(["systemctl", "show", "-p", "MainPID", "--value", "piccolod"]))
    assert daemon_pid > 0, "piccolod has no live process"
    daemon_env = dict(item.split(b"=", 1) for item in Path(f"/proc/{daemon_pid}/environ").read_bytes().split(b"\0") if b"=" in item)
    core = Path(os.fsdecode(daemon_env.get(b"PICCOLO_CORE_ROOT", b"/piccolo-core")))
    podman_root = Path(os.fsdecode(daemon_env.get(b"PICCOLO_PODMAN_ROOT", b"/run/piccolo/podman")))
    runroot_base = Path(os.fsdecode(daemon_env.get(b"PICCOLO_PODMAN_RUNROOT_BASE", b"/run/piccolo/podman")))
    return core, podman_root, runroot_base


def inspect(app):
    """Fail unless anchor and both service roots have live raw/idmap/container mounts."""
    core, podman_root, runroot_base = daemon_paths()
    app_dir = core / "mounts/control-plane/apps" / app
    metadata = json.loads((app_dir / "metadata.json").read_text())
    volumes = metadata["active_rootfs"]
    # AppMetadata contains only runtime fields. Definitions are persisted in
    # app.yaml; read the simple services/image scalars emitted for our fixture.
    image_refs, service, service_indent, in_services = {}, None, None, False
    for line in (app_dir / "app.yaml").read_text().splitlines():
        if line == "services:":
            in_services = True
            continue
        if not in_services or not line.strip():
            continue
        indent = len(line) - len(line.lstrip())
        if indent == 0:
            break
        if service_indent is None:
            service_indent = indent
        if indent == service_indent:
            match = re.fullmatch(r"\s+([A-Za-z0-9_-]+):", line)
            assert match, "unexpected fixture service key"
            service = match.group(1)
        elif service and re.match(r"\s+image:", line):
            value = line.split(":", 1)[1].strip().strip("\"'")
            assert re.fullmatch(r"[A-Za-z0-9/:@._-]+", value), "unexpected fixture image scalar"
            image_refs[service] = value
    assert set(image_refs) == {"main", "side"}, "fixture persisted image refs unavailable"
    username = "pa-" + app
    import pwd
    account = pwd.getpwnam(username)
    env = [f"HOME={account.pw_dir}", f"XDG_RUNTIME_DIR=/run/user/{account.pw_uid}"]
    runtime_root, runtime_runroot = podman_root / "apps" / app, runroot_base / ("app-" + app)
    assert runtime_root.is_dir() and runtime_runroot.is_dir(), "app Podman runtime directories unavailable"
    command = ["runuser", "-u", username, "--", "env", *env, "podman",
               "--root", str(runtime_root), "--runroot", str(runtime_runroot),
               "--storage-driver", "overlay", "inspect"]
    identities = {**metadata["containers"], "__netns__": metadata["network_anchor_id"]}
    host_mounts = Path("/proc/self/mountinfo").read_text().splitlines()
    result = {}
    os.chdir("/tmp")
    for service, cid in identities.items():
        vol = volumes[service]
        info = json.loads(subprocess.check_output(command + [cid]))[0]
        assert info["State"]["Running"], f"{service}: not running"
        pid = info["State"]["Pid"]
        assert pid > 0, f"{service}: no live PID"
        mapper = os.path.realpath("/dev/mapper/piccolo-vol-" + vol)
        assert Path(mapper).exists(), f"{service}: mapper missing"

        def mounted(lines, target):
            matches = [line.split() for line in lines if line.split()[4] == target]
            assert len(matches) == 1, f"{service}: expected one live mount at {target}"
            fields = matches[0]
            sep = fields.index("-")
            assert fields[sep + 1] == "btrfs", f"{service}: rootfs is not btrfs"
            assert os.path.realpath(fields[sep + 2]) == mapper, f"{service}: wrong rootfs source"
            assert "ro" in fields[5].split(","), f"{service}: rootfs is not read-only"

        raw_path, idmap_path = str(core / "mounts" / vol), str(core / "mounts" / (vol + "-idmap"))
        mounted(host_mounts, raw_path)
        mounted(host_mounts, idmap_path)
        container_mounts = Path(f"/proc/{pid}/mountinfo").read_text().splitlines()
        roots = [line.split() for line in container_mounts if line.split()[4] == "/"]
        assert len(roots) == 1, f"{service}: container root mount unavailable"
        root = roots[0]
        sep = root.index("-")
        fs_type = root[sep + 1]
        if fs_type == "btrfs":
            mounted(container_mounts, "/")
        else:
            # Read-only rootfs handles use Podman --rootfs <idmap>:O. The
            # container's root is then a writable overlay, whose exact lower
            # directory must remain the selected, live, read-only idmap mount.
            assert fs_type == "overlay", f"{service}: unexpected container root filesystem {fs_type}"
            options = dict(opt.split("=", 1) for opt in root[sep + 3].split(",") if "=" in opt)
            lowerdirs = options.get("lowerdir", "").split(":")
            assert lowerdirs == [idmap_path], f"{service}: overlay root does not use selected idmap lowerdir: {lowerdirs}"
        volume_meta = json.loads((core / "volumes" / vol / "piccolo.volume.json").read_text())
        image_ref = image_refs[service] if service != "__netns__" else volume_meta["base_image_ref"]
        result[service] = {"volume": vol, "digest": volume_meta["base_image_digest"], "image_ref": image_ref, "pid": pid,
                           "container": cid, "root_filesystem": fs_type, "live_mounts": True}
    assert {"main", "side", "__netns__"} == set(result), "unexpected runtime services"
    print(json.dumps(result, sort_keys=True))


def stop(root):
    def owned_pids():
        found = []
        for path in Path("/proc").glob("[0-9]*/cmdline"):
            try:
                args = path.read_bytes().split(b"\0")
            except (FileNotFoundError, ProcessLookupError, PermissionError):
                continue
            if b"serve" in args and str(root).encode() in args and any(arg.endswith(b"fixture.py") for arg in args):
                found.append(int(path.parent.name))
        return found

    pids = owned_pids()
    # Ownership is the exact run-scoped registry command, never merely a PID
    # from a stale file. Record the matched PIDs before termination for evidence.
    print("owned registry PIDs: " + ",".join(map(str, pids)), flush=True)
    for pid in pids:
        try:
            os.kill(pid, signal.SIGTERM)
        except ProcessLookupError:
            pass
    for _ in range(50):
        if not owned_pids():
            print("owned registry process absent", flush=True)
            return
        time.sleep(0.1)
    raise RuntimeError("owned registry process still live; preserve registry files and PID evidence")


def transaction_cleared(app):
    core, _, _ = daemon_paths()
    transaction = core / "mounts/control-plane/apps" / app / "manifest_update_transaction.json"
    assert not transaction.exists(), f"manifest transaction remains at {transaction}"
    print(str(transaction) + " absent")


def failure_cause(path):
    content = path.read_text()
    try:
        content = json.dumps(json.loads(content))
    except json.JSONDecodeError:
        pass
    assert "app update rolled back: install container group:" in content.lower(), "failure did not prove candidate recreation and precommit rollback"
    ordinary_directory = r"""filesystem [\\"']*proc[\\"']* must be mounted on ordinary directory"""
    pattern = rf"(?:mount\w*|stat|mkdir)[^\r\n]{{0,350}}/proc(?![A-Za-z0-9_./-])[^\r\n]{{0,350}}(?:not a directory|enotdir|{ordinary_directory})"
    assert re.search(pattern, content, re.IGNORECASE), "failure did not identify the injected /proc mount ENOTDIR"
    print("injected /proc mount ENOTDIR proven")


def diagnostics(app):
    core, podman_root, runroot_base = daemon_paths()
    metadata = json.loads((core / "mounts/control-plane/apps" / app / "metadata.json").read_text())
    import pwd
    account = pwd.getpwnam("pa-" + app)
    os.chdir("/tmp")
    command = ["runuser", "-u", account.pw_name, "--", "env", f"HOME={account.pw_dir}",
               f"XDG_RUNTIME_DIR=/run/user/{account.pw_uid}", "podman", "--root", str(podman_root / "apps" / app),
               "--runroot", str(runroot_base / ("app-" + app)), "--storage-driver", "overlay", "inspect", metadata["containers"]["main"]]
    info = json.loads(subprocess.check_output(command))[0]
    volume = metadata["active_rootfs"]["main"]
    volume_meta = json.loads((core / "volumes" / volume / "piccolo.volume.json").read_text())
    golden = volume_meta.get("golden_lv", "")
    result = {"main_container": metadata["containers"]["main"], "selected_rootfs": volume,
              "Entrypoint": info["Config"].get("Entrypoint"), "Cmd": info["Config"].get("Cmd"),
              "Path": info.get("Path"), "Args": info.get("Args"), "golden_lv": golden}
    result["State"] = {key: info["State"].get(key) for key in ("Status", "Running", "ExitCode", "Error", "Pid", "StartedAt", "FinishedAt")}
    if golden and re.fullmatch(r"[A-Za-z0-9_-]+", golden):
        config_path = core / "volumes" / golden / "image-config.json"
        result["golden_config_path"] = str(config_path)
        if config_path.is_file():
            config = json.loads(config_path.read_text())
            result["golden_config"] = {key: config.get(key) for key in ("entrypoint", "cmd", "user", "working_dir")}
    print(json.dumps(result, sort_keys=True))


if __name__ == "__main__":
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("action", choices=("build", "serve", "inspect", "stop", "transaction-cleared", "failure-cause", "diagnostics"))
    parser.add_argument("root", type=Path)
    parser.add_argument("--binary", type=Path)
    parser.add_argument("--arch", choices=("amd64", "arm64"), default="amd64")
    args = parser.parse_args()
    if args.action == "build":
        build(args.binary, args.root, args.arch)
    elif args.action == "serve":
        serve(args.root)
    elif args.action == "inspect":
        inspect(str(args.root))
    elif args.action == "transaction-cleared":
        transaction_cleared(str(args.root))
    elif args.action == "failure-cause":
        failure_cause(args.root)
    elif args.action == "diagnostics":
        diagnostics(str(args.root))
    else:
        stop(args.root)
