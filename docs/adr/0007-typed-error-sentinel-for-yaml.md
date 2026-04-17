# ADR-0007: Typed Error Sentinel for YAML Corrupt-State Recovery

## Status

Accepted (v1.8.0)

## Context

`pkg/node/config.go::EnsureKeypair` is responsible for loading persistent node state at boot. When the on-disk `node_state.yml` is unparseable (truncated, non-YAML bytes, decoder error), `EnsureKeypair` must distinguish a recoverable corruption (delete and regenerate) from an I/O error (propagate to caller). Before v1.8.0, classification used two checks joined by OR:

```go
var yamlErr *yaml.TypeError
isYAMLErr := errors.As(loadErr, &yamlErr) ||
             strings.Contains(loadErr.Error(), "unmarshal node state yaml")
```

The `strings.Contains` sentinel was fragile:

- It matched a specific message string baked into `LoadNodeState`'s wrapper: `"unmarshal node state yaml: %w"`. Rename the wrapper message → auto-recovery silently disabled for the non-TypeError path. No compile-time warning. No test failure unless the test was specifically constructed to hit a non-TypeError parse failure (none was).
- `yaml.TypeError` covers field-type mismatches but NOT decoder-level failures: truncated YAML producing a syntax error, random binary bytes hitting the scanner, lexer failures. Those errors would bypass `errors.As` and rely entirely on the string sentinel.
- Go 1.20+ `errors.As` already unwraps through `%w` chains, so the `yamlErr` branch caught the TypeError case. The `strings.Contains` branch was there to catch the OTHER parse failure classes that the code author did not classify systematically.

This is GitHub issue **#24 (M4 MEDIUM — YAML error classification via `strings.Contains`)**.

## Decision Drivers

- Make refactoring `LoadNodeState`'s wrapper message safe — the classification must not depend on message text.
- Cover all parse failure classes (TypeError, syntax error, truncated, binary garbage) under one check.
- Preserve EC-6: I/O errors from `os.ReadFile` must NOT be classified as corrupt state — they propagate unchanged.
- Preserve EC-5: an empty state file (0 bytes) is a valid YAML document (null) and must NOT trigger recovery — the existing "missing keypair" check handles it.
- Go 1.20+ multi-wrap (`fmt.Errorf("...: %w: %w", err1, err2)`) is available — the stack requires go 1.25 already.

## Decision

Introduce an exported sentinel in `pkg/node`:

```go
// ErrCorruptNodeState indicates node state YAML could not be decoded.
// Auto-recovery (delete & regenerate) is safe when this sentinel matches.
var ErrCorruptNodeState = errors.New("node state is corrupt")
```

Modify `LoadNodeState` to wrap every YAML decode failure with a multi-wrap:

```go
if err := yaml.NewDecoder(...).Decode(&state); err != nil {
    return nil, fmt.Errorf("unmarshal node state yaml: %w: %w", ErrCorruptNodeState, err)
}
```

Keep the I/O error path UNCHANGED — `os.ReadFile`'s `*os.PathError` propagates without the sentinel, satisfying EC-6.

`EnsureKeypair` classifies via:

```go
if !errors.Is(loadErr, ErrCorruptNodeState) {
    return nil, loadErr // propagate non-corrupt errors
}
// trigger recovery path: delete + regenerate
```

The refactor is additive — pre-existing callers of `LoadNodeState` that only checked `err != nil` continue to work unchanged. Callers that want to specifically distinguish corrupt-state from I/O can now use `errors.Is(err, node.ErrCorruptNodeState)`.

Extension during PR #38 review:

The same pattern was extended to `pkg/transport/node_state.go::LoadNodeTransportState` (as `ErrCorruptTransportState`) and `pkg/node/client_state.go::loadClientState` (as `ErrCorruptClientState`). Three neighboring files with identical decode-error-wrapping needs; a single refactor covered them uniformly.

## Alternatives Considered

1. **Keep `strings.Contains` with a constant.** Define `const LoadWrapperMsg = "unmarshal node state yaml"` at package scope, use it both in `LoadNodeState` wrap and `EnsureKeypair` check. Moves the risk from "silent breakage on message rename" to "need to coordinate two constants", but the string-matching path is still semantically wrong for error classification. Rejected — trading one form of fragility for a weaker one.

2. **Use a typed error struct (`ErrCorruptNodeState{Inner error}`).** More idiomatic for carrying the inner error as a field rather than a wrap. Rejected — `errors.Is` + `%w` is the stdlib-blessed pattern since Go 1.13; a custom struct would mean writing `Is` and `Unwrap` methods and duplicating logic already in the stdlib. The buys nothing over multi-wrap.

3. **Classify by error type list.** `errors.As(err, &*yaml.TypeError{})`, then `errors.As(err, &*yaml.ScannerError{})`, etc. Rejected — coupling `EnsureKeypair` to the internal type hierarchy of `yaml.v3`. A library upgrade that renames an internal type would silently break classification.

4. **Do not distinguish corrupt from I/O at all — always delete-and-regenerate on load failure.** Rejected — deletes the state file on transient disk errors, losing the keypair (irreversible without operator action). EC-6 is explicit on this.

## Consequences

**Positive:**

- Refactoring the `LoadNodeState` wrapper message is now safe. The sentinel value `ErrCorruptNodeState` is the classification anchor, not the message text.
- All parse failure classes (TypeError, scanner error, truncated YAML, binary garbage) now classify correctly through a single `errors.Is` check.
- I/O errors preserve their original type (`*os.PathError`) — callers downstream can `errors.As` on that without interference from the sentinel.
- The pattern is repeatable — `ErrCorruptTransportState` and `ErrCorruptClientState` extend it to two neighboring files with zero ceremony.
- Tests for recovery paths become semantic (write specific corrupt bytes, assert `errors.Is` returns true), not string-dependent.

**Negative:**

- One more exported symbol per file that needs corrupt-state classification. Low cost — the sentinels are declared at package scope with clear godoc.

- Go 1.20+ multi-wrap is required. Project's `go.mod` already requires 1.25, so this is not a binding constraint.

**Neutral:**

- `pkg/node/config.go` dropped `strings` usage in the classification — but the import remains because `strings.TrimSpace` is used elsewhere in the file.

## Evidence

- **Root cause citation:** `explorer` agent mapping of `pkg/node/config.go:104-106` — verified `strings.Contains` was on exactly one site, used for YAML-specific classification only, and that the matching string `"unmarshal node state yaml"` was literally the wrapper message text baked in at line 78.
- **Parse-failure coverage:** `TestEnsureKeypairRecoversTruncatedYAML` in `pkg/node/config_test.go` exercises two failure modes through the sentinel:
  - Truncated YAML (`"private_key: abc\npublic_key: [unclosed"`) — triggers a syntax error path that `yaml.TypeError` does NOT match.
  - Random binary bytes (`[]byte{0xff, 0x00, 0xde, 0xad}`) — triggers a scanner error that `yaml.TypeError` does NOT match.
  Both recover via `errors.Is(err, ErrCorruptNodeState)`. This would have failed under the pre-fix `strings.Contains` classification only if the exact string happened to be included in the wrapper — which it was, but only coincidentally.
- **Go multi-wrap docs:** https://pkg.go.dev/errors#As and https://pkg.go.dev/fmt#Errorf confirm `%w: %w` and `errors.Is` traversal through multi-wrapped chains.

## References

- GitHub issue #24 — YAML error classification via `strings.Contains`
- Spec: `.agent/specs/internal-review-fixes/spec.md` FR-4, EC-5, EC-6
- Pull request #38 — initial `ErrCorruptNodeState` + reviewer extension to transport and client state
- Predecessor ADR: `docs/adr/0005-transport-state-schema-versioning.md` (v1.7.0 — same subsystem, different state-file concern)
