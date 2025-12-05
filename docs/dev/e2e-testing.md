# E2E Integration Testing

This project uses Go-based integration tests to verify the full system lifecycle against a running MicroOS VM.

## Update & Rollback E2E

The `tools/integration-test/updates` suite verifies the OS update mechanism:
1.  Applies an update (via `transactional-update`).
2.  Reboots the VM.
3.  Verifies the new snapshot is active.
4.  Rolls back to the previous snapshot.
5.  Reboots.
6.  Verifies the rollback.

### Prerequisites
1.  **DevServer:** You must have the MicroOS devserver running.
    ```bash
    ./tools/e2e/microos-devserver.sh
    ```
    *Note: The first run takes time to build the base image cache.*

### Running the Test
In a separate terminal:
```bash
go run tools/integration-test/updates/main.go
```

### Expected Output
```text
Starting E2E Update Test against http://localhost:18080/api/v1
...
>>> TEST STAGE: APPLY UPDATE
...
>>> TEST STAGE: REBOOT INTO NEW SNAPSHOT
...
>>> TEST STAGE: ROLLBACK
...
>>> TEST STAGE: REBOOT INTO ROLLBACK
...
PASS: Full Update & Rollback Cycle Complete.
```

### Troubleshooting
- **Timeout waiting for update:** If the test hangs at "Waiting for update...", check the VM logs. It might be downloading heavy updates. Set `LOCAL_OSS_MIRROR` for `microos-devserver.sh` to speed this up.
- **Fail: System pending after reboot:** The VM failed to boot into the new snapshot (GRUB issue) or `piccolod` failed to detect the active snapshot ID correctly.
