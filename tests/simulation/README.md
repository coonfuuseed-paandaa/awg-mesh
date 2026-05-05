# Simulation Gates

This directory contains local simulation and release-gate harnesses for
awg-mesh. The current v2.0 release path is not the old role-command Docker
walkthrough; use the scripts below from the repository root.

## Required Release Simulations

Run these after unit tests, Docker image builds, the critical suite, and the
emulation playbook:

```bash
CGO_ENABLED=1 go test -race -count=1 ./...
bash tests/simulation/F-009-CR-001-foundation-smoke.sh
BUILDX_BUILDER=default bash tests/simulation/mikrotik-chr-baseline-runtime.sh
BUILDX_BUILDER=default bash tests/simulation/mikrotik-version-matrix.sh
```

`BUILDX_BUILDER=default` keeps the CHR gates on the local Docker builder. Some
developer machines also have a remote or multi-arch builder selected by
default; that is fine for image publishing, but it can break the local CHR
runtime path with unrelated registry or network errors.

## What Each Gate Proves

| Script | Purpose |
|---|---|
| `F-009-CR-001-foundation-smoke.sh` | Foundation smoke: tracked Go files are formatted, build/vet/test gates pass, binary modes start, schema v1 is rejected, schema v2 is accepted, and the critical-suite runner is present. |
| `mikrotik-chr-baseline-runtime.sh` | Bare RouterOS `/container` baseline: veth, bridge, NAT, firewall counters, container logs, and container-originated reachability work before the product is deployed. |
| `mikrotik-version-matrix.sh` | Product CHR E2E on RouterOS 7.21+ targets. The default is 7.21.4. Generator syntax coverage for 7.16.2 and 7.20.8 is handled by Go tests, not by this runtime gate. |

## Extended Harnesses

The following scripts remain useful for targeted investigation, but they are not
the v2.0 release gate:

```text
data-plane-extended.sh
modules/fr*.sh
issue-93-upgrade.sh
issue-99-allowedips.sh
issue-100-scp-compose.sh
mikrotik-onboard.sh
mikrotik-chr-import.sh
```

Run them only when working on the corresponding legacy behavior, migration
path, or diagnostic scenario.

## Cleanup

Most harnesses create temporary Docker networks, containers, images, and
directories under `.agent/tmp/`. Re-running a gate normally cleans its own
state. If a run is interrupted, inspect the script header for the exact cleanup
labels before removing resources manually.
