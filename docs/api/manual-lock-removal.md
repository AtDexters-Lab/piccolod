# Public manual-lock API removal

`POST /api/v1/crypto/lock` has been removed. This is an intentional breaking
API change: Piccolod no longer exposes a public operation that discards the
in-memory storage key while the appliance remains running.

Use operating-system power-off when the appliance must go offline, or reboot
when a clean restart is required. There is no replacement manual-lock API.
Internal lifecycle and storage lock operations used by shutdown and recovery
remain unchanged.
