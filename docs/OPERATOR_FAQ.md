# awg-mesh operator FAQ

Reference for operator-facing semantics that are not obvious from the `--help`
output of the individual commands. Each entry below is paired with the source
of truth in the code so you can drill into the mechanics from there.

## Token lifecycle and `MESH_TOKEN_HASH`

`MESH_TOKEN_HASH` is a bcrypt hash of the raw gRPC auth token, **not** the raw
token itself. It is identical for all three node modes (master, endpoint, client)
— no per-mode difference in intent or flow.

| Artefact | Contents | Where | Who writes |
|----------|----------|-------|-----------|
| `<nodeDir>/token` on admin | Raw 64-char hex token (the secret) | Admin-side config dir | `mesh-ctl <mode> prepare` |
| `<nodeDir>/mesh.token` on admin | Bcrypt hash of the raw token | Admin-side config dir | `mesh-ctl <mode> prepare` |
| `MESH_TOKEN_HASH` env var | Bcrypt hash | Docker Compose file | `mesh-ctl <mode> prepare` |
| `/config/mesh.token` on node | Bcrypt hash (same as env var) | Inside the running container | `awg-mesh-node` on **first boot only** |

**First-boot flow:**
1. `awg-mesh-node` starts, reads `MESH_TOKEN_HASH` from env.
2. If `/config/mesh.token` does not exist, the binary validates the hash
   (`bcrypt.Cost([]byte(hash))`) and writes it to `/config/mesh.token`.
3. On subsequent boots the existing `/config/mesh.token` wins and the env var
   is ignored. The startup log still carries the env value; the node does
   not re-read it.

**gRPC auth:**
- `mesh-ctl` reads the **raw** token from `<nodeDir>/token` and includes it in
  RPC bearer headers.
- `awg-mesh-node` reads the hash from `/config/mesh.token`, and on each
  incoming RPC calls `bcrypt.CompareHashAndPassword(hash, incomingToken)`.
- No challenge-response; the token is the bearer secret on the wire. mTLS is
  layered on top for transport security.

**Why the env var holds the hash, not the raw token:**
First-boot bootstrap is a one-shot operation; after it completes the env var
is dead weight. Persisting the hash (not the raw value) limits the blast
radius of a leaked compose file — a compose file containing a bcrypt hash
cannot be replayed as a valid auth token against the live node. The raw
token is only held by the admin CLI.

**Rotation:** `mesh-ctl token rotate <node>` generates a new raw token,
bcrypt-hashes it, writes the hash to the master's admin state, and prints the
plain value for `MESH_TOKEN_HASH` substitution on the endpoint. See
`pkg/tls/token.go:19-44` for the authoritative docstring.

> Related engram issue: #151 items B5 and B19.

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

> Related engram issue: #151 item B16.

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

> Related engram issue: #151 item B22.

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

```
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

> Related engram issue: #151 items B24, B25, B26, B28.

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

> Related engram issue: #151 item B31.
