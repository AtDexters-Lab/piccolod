# Modify App image update VM regression

Run against an already-running, initialized alpha VM with root SSH and
block-native app storage:

```bash
PICCOLO_TEST_PASS_FILE=/path/to/test-password \
  ./scripts/alpha/dev-vm-alpha-test.sh 192.0.2.10 modify-app-image-update
```

For a VM using HTTP and SSH port forwards:

```bash
PICCOLO_TEST_SSH_HOST=127.0.0.1 PICCOLO_TEST_SSH_PORT=22047 \
  PICCOLO_TEST_PASS_FILE=/path/to/test-password \
  ./scripts/alpha/dev-vm-alpha-test.sh 127.0.0.1:18047 modify-app-image-update
```

The host needs Go and Python 3. The guest needs Python 3, curl, skopeo, Podman registry
drop-in support, enough app storage, and its normal network-anchor image cached
or obtainable. This focused stage does not start, reset, or destroy VMs and is
not part of the default `all` sequence.

The stage builds a static HTTP workload and deterministic OCI images locally.
It serves immutable v1/v2 images, a tag alias of v2, and a failure image containing
a regular file at `/proc` from a temporary guest-loopback registry. Main and side run
different ports; only the main image changes. No external registry supplies the
fixture workload. The network anchor remains the daemon's normal image.
The fixture HTTP listener explicitly permits public access to `/` and `/sentinel`.

Each update uses `/manifest/configure`, `/manifest/dry-run`, and
`/manifest/update`, including the returned plan hashes, token, and required
confirmations. Checks cover the persisted image reference and rootfs digest,
HTTP version marker, persistent data sentinel, and live raw, idmapped, and
container root mounts for both services and the anchor. Alias apply must reuse
the rootfs volumes. The failure image must prevent the OCI runtime from mounting
`/proc` before PID 1 starts, prove the corresponding mount ENOTDIR, and restore
the previously committed image, mounts, HTTP marker, and data.
The failure must also identify candidate container-group installation and
precommit rollback; a fault during image flattening does not satisfy this gate.

Run the same stage on the baseline and fixed binaries. A baseline attachment
regression fails the ordinary success gate; no expected-failure mode masks it.
The process exits nonzero when this stage has failed checks. API responses,
fixture digests, rootfs/PID snapshots, and mount errors remain under
`/tmp/piccolo-alpha-$UID/logs/modify-app-image-<run-id>/`. Failed stages also save
the daemon journal through the existing log helper. A missing prerequisite is
a fixture failure and does not establish the attachment regression.

Cleanup targets only the randomly named app created by that invocation, its
loopback registry process and temporary files, and its unique Podman registry
drop-in. It does not delete other apps or shared golden image volumes.
