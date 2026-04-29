# awg-mesh operator FAQ

Reference for operator-facing semantics that are not obvious from the `--help`
output of the individual commands. Each entry below is paired with the source
of truth in the code so you can drill into the mechanics from there.

## Token lifecycle and `MESH_TOKEN_HASH` (v2 format — v1.14.0+)

`MESH_TOKEN_HASH` is a v2-format hash of the raw gRPC auth token, **not** the
raw token itself. v2 format string: `mesh1.<base64url(54-byte blob)>` —
exactly 78 characters, charset `[A-Za-z0-9._-]`. Identical semantics across
all three node modes (master, endpoint, client) — no per-mode difference in
intent or flow.

**Wire format (54-byte blob, BigEndian, fixed offsets):**

| Offset | Size | Field | Value (v1) |
|--------|------|-------|-----------|
| 0 | 1 | `format_version` | `0x01` |
| 1 | 1 | `algo` | `0x01` (argon2id) |
| 2 | 2 | `m_cost_kb` | `4096` |
| 4 | 1 | `t_cost` | `1` |
| 5 | 1 | `parallelism` | `1` |
| 6 | 16 | `salt` | CSPRNG random |
| 22 | 32 | `hash` | argon2id key |

54 bytes raw → `base64.RawURLEncoding` (no padding) = 72 chars → prefix
`mesh1.` = **78 chars total**.

| Artefact | Contents | Where | Who writes |
|----------|----------|-------|-----------|
| `<nodeDir>/token` on admin | Raw 64-char hex token (the secret) | Admin-side config dir | `mesh-ctl <mode> prepare` |
| `<nodeDir>/mesh.token` on admin | v2 hash (`mesh1.<...>`) | Admin-side config dir | `mesh-ctl <mode> prepare` |
| `MESH_TOKEN_HASH` env var | v2 hash | Docker Compose file (raw, no shell quoting needed) | `mesh-ctl <mode> prepare` |
| `/config/mesh.token` on node | v2 hash (same as env var) | Inside the running container | `awg-mesh-node` on **first boot only** |

**First-boot flow:**
1. `awg-mesh-node` starts, reads `MESH_TOKEN_HASH` from env.
2. `bootstrapTokenHash` calls `pkgtls.ParseV2` on the value.
3. If parse fails (legacy bcrypt `$2a$...`, empty string, malformed blob,
   unknown prefix): the node emits structured zerolog error
   `{"event":"token_hash_invalid","format":"unknown","msg":"MESH_TOKEN_HASH must be v2 format"}`
   and exits non-zero. **There is no fallback to bcrypt.**
4. On parse success, if `/config/mesh.token` does not exist, the validated
   hash is written to it.
5. On subsequent boots the existing `/config/mesh.token` wins and the env
   var is ignored.

**gRPC auth:**
- `mesh-ctl` reads the **raw** token from `<nodeDir>/token` and includes it
  in RPC bearer headers.
- `awg-mesh-node` reads the hash from `/config/mesh.token`, and on each
  incoming RPC calls `pkgtls.VerifyToken(token, hash)` which:
  - Parses the v2 blob (`ParseV2`).
  - Computes argon2id with the salt + parameters from the blob.
  - Compares against the stored key via `subtle.ConstantTimeCompare`
    (constant-time, timing-attack resistant).
- No challenge-response; the token is the bearer secret on the wire. mTLS
  is layered on top for transport security.

**Charset / shell-safety:**

v2 hashes contain ZERO shell or RouterOS metacharacters by construction
(`mesh1.` prefix uses `.`, body uses base64url alphabet `[A-Za-z0-9_-]`).
This is what enabled the v1.14.0 cleanup of `composeEscapeDollar` (Docker
Compose `$$` doubling) and `quoteRouterOSValue` wrapping for the
`MESH_TOKEN_HASH` env var. Operators editing docker-compose files by hand
no longer need to manually escape `$` characters in the hash.

**Rotation:** `mesh-ctl token rotate <node>` generates a fresh raw token,
v2-hashes it, sends the hash to the node via the `RotateToken` RPC (the
node overwrites its `/config/mesh.token`), and saves both values locally —
the raw token to `<nodeDir>/token` (for future mesh-ctl RPC calls) and the
v2 hash to `<nodeDir>/mesh.token`. The `MESH_TOKEN_HASH` env var in the
node's compose file is the v2 hash; re-run `mesh-ctl <role> prepare
<node>` to regenerate the compose file with the new hash substituted, OR
read it directly from `<nodeDir>/mesh.token`. Only pass `--show-token` when
you actually need the raw token printed to stdout (e.g. bootstrapping a new
node on a host that does not yet have the `<nodeDir>/token` file). See
`pkg/tls/token.go` for the authoritative lifecycle docstring.

**v1.14.0 cutover (operator action):**

v1.14.0 is a wire format break — see CHANGELOG.md "Operator Cutover
Runbook" for the per-node procedure. There is no transition window or
dual-format dual-accept by design. The node binary fail-fasts on legacy
bcrypt hashes; rollback to v1.13.0 requires re-preparing nodes with
bcrypt format from the v1.13.0 `mesh-ctl`.

> Added in v1.12.11/v1.12.12 audit items B5 and B19. Argon2id v2 format
> introduced in v1.14.0 (issue #181 Track 3 — wire-protocol-token-format-v2).

## Pubkey admin-side file formats

Admin-side pubkeys (`<nodeDir>/pubkey`) may be in either of two formats; every
reader accepts both:

| Format | Length | Origin |
|--------|--------|--------|
| Raw 32-byte binary | 32 bytes | Legacy — written by pre-v1.11.2 `init` commands |
| 64 hex chars (+ optional newline) | 64 or 65 bytes | Current — written by `adminstate.SetPubkey` since v1.11.2 |

All of `readEndpointPublicKey` (master.go), `readAdminPubkeyRaw` (endpoint.go),
`readAdminPubkeyBytes` (reconcile.go), and `readAdminPubkey` (inspect.go) fall
back to either format automatically.

**If you see a pubkey file changing length** between runs (32 → 65 or vice
versa), it is almost certainly the result of a manual operator edit
(`hex` / `xxd` / PowerShell `Get-Content`); `mesh-ctl` itself writes only the
current 64-hex format and never rewrites a legacy binary file unless `endpoint
init` / `master init` runs again. A re-init on the same node does not flip
the format — `SetPubkey` atomically writes the new hex value.

> Added in v1.12.11/v1.12.12 audit item B16.

## Client admin-side state

`mesh-ctl client prepare <name>` writes these under `<configDir>/nodes/<name>/`:

- `mesh.token` — bcrypt hash (60 bytes), server-side verification.
- `token` — raw token (64 bytes), admin-side RPC bearer.

**No `pubkey` file is written for clients by design.** Clients generate their
WireGuard keypair at container boot (like endpoints) and self-register with
each bound master via `AddPeer` RPC. The master stores the client's pubkey in
its own state (`/config/transport.yml`) after the first `AddPeer` call, not
in the admin-side directory.

**Consequences:**
- `mesh-ctl client init <name>` cannot pre-register anything on masters — the
  client must boot first.
- `mesh-ctl inspect <client>` will show admin-side pubkey as blank until the
  client has called `AddPeer` and the master's `GetTransportState` surfaces
  the new peer.
- Tier-3 keypair rotation (`mesh-ctl rotate --tier 3 --node <client>`) is
  **not supported for clients** — only for endpoints. Rotate the client by
  restarting its container; the new keypair is negotiated via `AddPeer` on
  the next boot.

> Added in v1.12.11/v1.12.12 audit item B22.

## Config-dir `.bak.*` and related artefacts

`mesh-ctl` writes exactly three categories of transient files under the
config dir:

| Path | Written by | Purpose |
|------|-----------|---------|
| `<configDir>/backups/upgrade-logs/upgrade-<ver>-<ts>.log` (v1.12.12+) | `pkg/upgrade.Logger` | JSONL audit log, one file per `mesh-ctl upgrade` invocation |
| `<configDir>/upgrade-<ver>-<ts>.log` (pre-v1.12.12) | `pkg/upgrade.Logger` | Same, at config root. Still surfaced by `mesh-ctl upgrade status` as a fallback; move under `backups/upgrade-logs/` manually when convenient |
| `<configDir>/nodes/<name>/<name>-docker-compose.yml.bak` | `pkg/upgrade/driver.phasePrepare` | Rollback snapshot; removed by the upgrade driver if verify succeeds |
| `<file>.bak` (next to `<file>`) | `mesh-ctl upgrade compose --in-place` | Safety copy before in-place schema migration |

**Backup conventions that `mesh-ctl` does NOT create:**

- `<configDir>/nodes/<name>.bak.<YYYYMMDD-HHMMSS>/` — whole-node-dir snapshot
  under a dotted suffix. **Operator-created** (e.g. a PowerShell
  `Rename-Item` before a risky init re-run). `mesh-ctl config show` filters
  these out of the node count via `isBackupDir(name)` so they do not inflate
  the count, but the CLI never creates them itself.
- `<configDir>/nodes.bak.<ts>/` — whole-tree snapshot at config-dir level.
  Also operator-created.
- `<configDir>/mesh-topology.yml.bak.<ts>` — topology YAML snapshot. Also
  operator-created. `mesh-ctl ip range add/resize` modifies the topology
  in-place via `topology.SaveTopology` without a backup; if you want a
  snapshot, take one yourself.

**If you follow the `backups/` convention yourself**, a clean tree looks like:

```text
~/.mesh-ctl/
  ca.crt / ca.key
  mesh-topology.yml
  domains.txt / domains-test.txt
  transport.yml
  nodes/                          # active node config, one dir per node
  clients/                        # generated client artefacts (.rsc, .sh, .json)
  backups/
    upgrade-logs/                 # upgrade-<ver>-<ts>.log (auto-managed)
    nodes/<name>.<ts>.bak/        # optional, operator-created
    topology/mesh-topology.yml.<ts>.bak  # optional
    compose/<name>-docker-compose.yml.<ts>.bak  # optional
```

> Added in v1.12.11/v1.12.12 audit items B24, B25, B26, B28.

## `mesh-ctl upgrade compose` — preview vs in-place

Without `--in-place`, the migrated compose is written to **stdout**. This
doubles as the preview mode: redirect to a file, `diff` against the original,
inspect, then rerun with `--in-place` when you are satisfied.

```bash
# Preview: write the migrated output to a file and diff it against the original.
mesh-ctl upgrade compose /path/to/old-compose.yml > /tmp/new-compose.yml
diff /path/to/old-compose.yml /tmp/new-compose.yml

# Apply: rewrite the original in-place (the pre-migration bytes are saved as
# `<file>.bak` alongside — the .bak is written before any changes touch the
# live file, so the rollback path is always available).
mesh-ctl upgrade compose --in-place /path/to/old-compose.yml
```

No `--dry-run` flag exists because the default (`stdout`) already is the
dry-run. `--in-place` is the only destructive mode.

> Added in v1.12.11/v1.12.12 audit item B31.
