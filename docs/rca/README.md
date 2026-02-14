# Piccolo RCAs

This directory houses root-cause analysis documents for notable production or development incidents. Each RCA should:

1. Live in its own Markdown file named `YYYYMMDD-short-title.md`.
2. Capture the timeline, root cause, impact, and remediation — both immediate and preventive.
3. Include a final section titled `Remediation Status` summarizing what landed (commit/PR references, follow-ups, or open items).

Workflow:
- Open an RCA file in this folder after any incident that reveals a systemic flaw (race condition, data corruption, silent failure, etc.).
- Reference the RCA from relevant PR descriptions and RFCs.
- Update the `Remediation Status` section once fixes land so future readers know the outcome.

This keeps incident history close to the code and gives the team a single place to track failure modes and the mitigations applied.
