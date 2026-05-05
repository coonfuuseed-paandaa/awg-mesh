# Production Testing Playbook - awg-mesh v2.0

Customer-mode release walkthrough for `awg-mesh-node` and `mesh-ctl`.
Run this before tagging any v2.x release after the critical suite has passed.

This playbook is not a source-code review. The operator follows documented
commands, observes product output, and records whether the product works from a
user point of view.

## Scope

This playbook validates these user-facing surfaces:

- `mesh-ctl` first-run help, version, topology, node, backup, restore, and
  upgrade planning commands.
- `awg-mesh-node` first-run version output and master-owned-zone documentation
  contract.
- v2.0 topology-as-code using `pkg/topology/testdata/v2-topology.yml`.
- Admin-side prepare -> backup -> restore lifecycle.
- Coordination/clientd certificate lifecycle coverage through the critical
  suite handoff.
- The handoff from customer-mode walkthrough to the automated critical suite.

This playbook deliberately does not validate:

- Full packet data plane on a real multi-host mesh. That is covered by the
  critical suite and simulation gates.
- RouterOS CHR behavior. The CR-014 static RouterOS script gate runs in the
  critical suite; real CHR import remains in the release gate.
- v1.x topology migration. CR-013 owns migration tooling.

## Prerequisites

| Requirement | How to verify |
|---|---|
| Go 1.25+ | `go version` |
| Bash | `bash --version` |
| Coreutils `timeout` | `timeout --help` |
| Docker, for release runs | `docker info` |
| Repo checkout at the release candidate commit | `git rev-parse --short HEAD` |

On Windows, run the commands from WSL2 or from the same Bash environment used by
the critical suite. PowerShell is fine for orchestration, but the playbook
harness itself is a Bash script.

## Quick Run

From the repository root:

```bash
bash tests/emulation-playbook/run.sh
```

Expected final lines:

```text
Overall verdict: PRODUCT_WORKS
Gate decision: PASS
PRODUCT_WORKS
```

The script writes a customer-mode run report to:

```text
.agent/reports/emulation-playbook-run-<timestamp>.md
```

## Customer-Mode Rules

1. Run each command exactly as written.
2. Use public docs, the playbook, and compiled binaries only.
3. Do not inspect implementation files while executing a scenario.
4. Record surprises even when the command exits 0.
5. If any scenario fails, the overall verdict is `BROKEN` and the release is
   blocked until the failure is fixed and the playbook is rerun.

## Scenarios

### S1 - First-run binaries

**Flow:**

1. Build `mesh-ctl` and `awg-mesh-node` from the checkout.
2. Run `mesh-ctl version`.
3. Run `awg-mesh-node --version`.
4. Run `mesh-ctl --help`.

**Expected user-visible output:**

- Both binaries build without errors.
- `mesh-ctl version` prints a recognizable version string.
- `awg-mesh-node --version` prints `awg-mesh-node v...`.
- `mesh-ctl --help` lists `topology`, `node`, `backup`, `restore`, and
  `upgrade`.

**Failure signatures:**

- Build fails.
- Version output is empty or not product-branded.
- Help omits a v2 operator command.

### S2 - Topology-as-code first look

**Flow:**

1. Run `mesh-ctl topology validate --topology pkg/topology/testdata/v2-topology.yml`.
2. Run the same command with `--output json`.
3. Run `mesh-ctl node list --output json` on the same topology.

**Expected user-visible output:**

- Human output says the topology is valid.
- JSON output reports `status: valid`, `schema_version: 2`, `nodes: 5`, and
  `services: 1`.
- Node list reports five declared nodes including `master-01`,
  `ingress-de-01`, and `home-server-01`.

**Failure signatures:**

- v2 fixture is rejected.
- JSON output cannot be consumed by automation.
- Node list omits declared nodes.

### S3 - Admin prepare artifacts

**Flow:**

1. Create a throwaway config directory.
2. Run `mesh-ctl node prepare master-01`.
3. Run `mesh-ctl node prepare home-server-01`.
4. Inspect only the generated product artifacts.

**Expected user-visible output:**

- Each prepare command exits 0 and prints the generated node directory.
- The config directory contains `ca.crt`.
- Each prepared node has `token`, `mesh.token`, `node.crt`, and `node.key`.

**Failure signatures:**

- Prepare exits 0 but no token or certificate exists.
- Generated files land outside the requested config directory.
- A rerun corrupts the shared CA material.

### S4 - Backup and restore

**Flow:**

1. Back up the throwaway config and topology to a zip archive.
2. Restore that archive into a second throwaway config directory.
3. Verify restored files from the user-visible paths.

**Expected user-visible output:**

- `mesh-ctl backup <archive>` prints `backup written`.
- `mesh-ctl restore <archive> --confirm` prints `backup restored`.
- Restored topology and node cert/token files exist in the requested targets.

**Failure signatures:**

- Archive is missing or empty.
- Restore writes to the original source directory instead of the requested
  target.
- Restore succeeds without `--confirm`.

### S5 - Upgrade plan and master-owned coordination contract

**Flow:**

1. Run `mesh-ctl upgrade v2.0.1 --dry-run`.
2. Read the public README architecture and command sections.
3. Confirm the documented current path uses master-owned zones and a
   responsible master coordination endpoint.

**Expected user-visible output:**

- Upgrade output shows ordered phases for masters, mesh roles, and clients.
- The README describes `mesh-ctl` as the desired-state tool.
- The README describes responsible masters as the current coordination target.
- The customer-mode playbook does not instruct operators to start a standalone
  daemon as the current happy path.

**Failure signatures:**

- Upgrade plan omits a declared role.
- Public docs present a standalone daemon as the current deployment path.
- Public docs imply runtime master-to-master shared state or universal peering
  as a required invariant.

### S6 - Critical-suite handoff

**Flow:**

1. Run `bash tests/critical/run-all.sh`.
2. Read the final summary.

**Expected user-visible output:**

- Developer-mode critical suite exits 0.
- Summary reports no failures.
- Any skips are named and tied to later CRs.

**Failure signatures:**

- Critical runner crashes.
- Summary reports a failure.
- A skip is silent or unlabelled.

## Scenario Index per F-ID

| F-ID / requirement | User-visible feature | Scenarios |
|---|---|---|
| F-009 FR-1..FR-14 | v2 mesh operator surface and release readiness | S1, S2, S6 |
| F-009 FR-15 | Decommission lifecycle evidence through critical suite handoff | S6 |
| F-009 FR-16 | Automatic node certificate lifecycle evidence through critical suite handoff | S6 |
| F-009 FR-18 | Backup/restore and coordination recovery | S4, S5, S6 |
| F-009 FR-19 | Upgrade planning flow | S5 |
| F-009 FR-20 | Audit log query coverage through critical suite handoff | S6 |
| Rule 11 | Customer-mode product walkthrough | S1..S6 |

## Failure-Mode Catalog

| Symptom | Likely class | First diagnostic |
|---|---|---|
| Command exits 0 but expected file is missing | silent-success regression | Re-run the exact command and inspect generated artifact paths |
| Help omits a documented command | CLI surface drift | Compare help output to this playbook |
| JSON output loses expected fields | automation contract regression | Save stdout and validate field names |
| Docs present a standalone daemon as the current path | architecture contract drift | Compare README architecture and S5 wording |
| Critical suite skip is unlabeled | release-gate drift | Open the critical runner output and identify the script |

## Verdict Template

Copy this into the run report when executing manually:

```markdown
## Verdict - <YYYY-MM-DD HH:MM TZ>

**Release candidate:** <commit or tag>
**Duration:** <N> minutes
**Agent identity:** customer-mode

| # | Scenario | Verdict | Notes |
|---|---|---|---|
| S1 | First-run binaries | PASS | - |
| S2 | Topology-as-code first look | PASS | - |
| S3 | Admin prepare artifacts | PASS | - |
| S4 | Backup and restore | PASS | - |
| S5 | Upgrade plan and master-owned coordination contract | PASS | - |
| S6 | Critical-suite handoff | PASS | - |

**Surprises:**
- None

**Breakages:**
- None

**Overall:** PRODUCT_WORKS
**Gate decision:** PASS
```

## Maintenance

- Add a scenario in the same CR that adds a user-visible command, endpoint, or
  release gate.
- Keep this playbook aligned with `tests/critical/run-all.sh`.
- Stale v1.x commands are release blockers for v2.x.
