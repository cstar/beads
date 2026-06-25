# Plan — be-6ebm0: configurable read deadline for `bd dolt push`/`pull`

**Bead:** be-6ebm0 (bug, P2) · **Rig:** beads · **Base:** `feat/connection-pooling` · **Branch:** `gc/be-6ebm0`
**Executor model:** claude-sonnet-4-6

## Problem (one paragraph)

`bd dolt push` of the HQ (`po`) store aborts mid-upload. The 72-bead `po` store
streams a large delta to the `git+ssh` remote; the dolt sql-server blocks while
it uploads, emitting no intermediate MySQL packets. The client connection that
issued `CALL DOLT_PUSH(...)` carries a **hardcoded `cfg.ReadTimeout = 5 *
time.Minute`** (`internal/storage/dolt/store.go`), so the client read deadline
trips at exactly 300s, the connection goes "invalid", the server's upload dies
(broken pipe), and **the remote head never advances** (no partial push). The
sysadmin's fresh chrono (2026-06-11) matches this precisely: total 5m08s, i/o
timeout on the SQL channel at ~5m08 → `invalid connection`. The direct-endpoint
path (v1.0.7.1, `storeForRawDoltSync`, "using direct dolt endpoint") was
confirmed in the logs, which means the proxy multiplexer — and therefore PR #5's
proxy-side `mysqlwire.go` dial/accept deadlines — is **out of the path**. The
binding constraint on the confirmed direct path is the client-side MySQL
`ReadTimeout`.

## Root cause (exact)

Two one-shot sync connections hardcode the same 5-minute read deadline:

| Fn | store.go line (base `feat/connection-pooling`) | Used by |
| --- | --- | --- |
| `execWithLongTimeout` | `cfg.ReadTimeout = 5 * time.Minute` @ **1294** | `Pull` (wrapped in tx, DOLT_PULL) |
| `execWithLongTimeoutNoTx` | `cfg.ReadTimeout = 5 * time.Minute` @ **1321** | `Push` (no tx, DOLT_PUSH) — **the incident** |

A third occurrence at **~2290** is the steady-state pool/default connection for
normal queries — **leave it untouched**; lengthening normal-query reads is out of
scope and undesirable.

## Fix (recommended approach)

Introduce one package-level helper and apply it at the two **sync** call sites
only. The deadline becomes configurable via env, default **raised to 30m**, with
`0` as an explicit "no deadline" escape hatch.

```go
// doltSyncReadTimeout returns the MySQL client read-deadline applied to the
// one-shot connections that run CALL DOLT_PUSH / DOLT_PULL. These commands block
// while the dolt sql-server streams the delta to the git+ssh remote, emitting no
// intermediate packets — so a fixed read deadline shorter than the upload aborts
// an otherwise-healthy push (be-6ebm0: the `po` store took 5m08s and tripped the
// previous hardcoded 5m deadline, leaving the remote head un-advanced).
//
// Override with BEADS_DOLT_PUSH_TIMEOUT (Go duration, e.g. "45m"); it governs
// BOTH push and pull long-timeout connections. A value that parses to zero
// ("0", "0s") disables the deadline entirely (unbounded — rely on context /
// SIGINT for cancellation). An unparseable value is ignored and the default used.
func doltSyncReadTimeout() time.Duration {
	const def = 30 * time.Minute
	raw := strings.TrimSpace(os.Getenv("BEADS_DOLT_PUSH_TIMEOUT"))
	if raw == "" {
		return def
	}
	d, err := time.ParseDuration(raw)
	if err != nil {
		return def
	}
	return d // d == 0 → go-sql-driver treats as "no read timeout"
}
```

Then at lines **1294** and **1321**: `cfg.ReadTimeout = doltSyncReadTimeout()`.

`os`, `strings`, `time` are already imported in `store.go` (executor verifies).

**Why in-process & no server bounce:** the direct path opens this connection
inside the `bd` process (`storeForRawDoltSync` → `dolt.New` → `execWithLongTimeoutNoTx`),
so `BEADS_DOLT_PUSH_TIMEOUT` is read by the `bd` client at push time. The
sysadmin sets the env and retries immediately — no dolt-server restart.

## Micro-tasks

| id | description | acceptance (a single failing test) | est_minutes | slings |
| --- | --- | --- | --- | --- |
| T-001 | Add failing unit test `TestDoltSyncReadTimeout` (sub-cases: default-when-unset→30m, `45m`→45m, `0`→0, invalid→30m) to `internal/storage/dolt/store_unit_test.go`, using `t.Setenv`. | `go test ./internal/storage/dolt/ -run TestDoltSyncReadTimeout` **fails to compile** (`undefined: doltSyncReadTimeout`) — the red. | 4 | — |
| T-002 | Add the `doltSyncReadTimeout()` helper to `internal/storage/dolt/store.go`. | `go test ./internal/storage/dolt/ -run TestDoltSyncReadTimeout` is **green**. | 5 | — |
| T-003 | Replace the two **sync** call sites (`execWithLongTimeout` @1294, `execWithLongTimeoutNoTx` @1321) with `cfg.ReadTimeout = doltSyncReadTimeout()`. Confirm the ~2290 occurrence is the steady-state pool default and **leave it**. | `go build ./...` green; `grep -c "ReadTimeout = 5 \* time.Minute" internal/storage/dolt/store.go` returns **1** (only the pool default remains). | 5 | — |
| T-004 | Update existing `TestExecWithLongTimeoutDSNRewrite` to assert `doltSyncReadTimeout()` (not the literal `5*time.Minute`) flows into the rewritten DSN — locks the regression. | `go test ./internal/storage/dolt/ -run TestExecWithLongTimeoutDSNRewrite` green; fails if the literal `5*time.Minute` is reintroduced at a sync site. | 3 | — |
| T-005 | Document `BEADS_DOLT_PUSH_TIMEOUT` in `docs/CLI_REFERENCE.md` (semantics: governs push+pull long-timeout conns; default 30m; `0`=unbounded) and add a `CHANGELOG.md` entry. | `grep -q BEADS_DOLT_PUSH_TIMEOUT docs/CLI_REFERENCE.md` succeeds. | 4 | — |
| T-006 | Package gate. | `go test -tags "$BUILD_TAGS" ./internal/storage/dolt/...` green **and** `go vet ./internal/storage/dolt/...` clean. (CGO=1 per Makefile; use `make test` if env is set up.) | 5 | — |

First task is the failing test (TDD hard rule, Architecture §10). Projected diff
~80 lines.

## Files touched

- `internal/storage/dolt/store.go` — helper + 2 sync call-site swaps (code, ~17 lines)
- `internal/storage/dolt/store_unit_test.go` — new test + regression-lock update (~50 lines)
- `docs/CLI_REFERENCE.md` — env-var note (mechanical)
- `CHANGELOG.md` — entry (mechanical, repo convention)

**Complexity note (global CLAUDE.md ">3 files → split"):** code surface is **2
files**; the other two are mechanical doc/changelog touches in one logical
commit, not review-bearing logic. Within the spirit of the rule — no split.
Projected <100 lines, well under the 500-line architect-pre-pass threshold.

## Validation (post-PR, ops — not a code gate)

Sysadmin is volunteered to test a build (acceptance criteria). Steps:
1. Build the `gc/be-6ebm0` branch binary (CGO=1, the v1.0.7.1-line direct-sync
   path is present on this base).
2. From `/Users/cstar/portharbour`: `BEADS_DOLT_PUSH_TIMEOUT=45m bd dolt push`
   on the `po` store; confirm "using direct dolt endpoint" still logs and the
   **remote head advances** (push completes).
3. Sanity: with default (env unset) a >5min upload now succeeds (was the bug).

If the push still cuts at a *different* boundary after raising the client
deadline, that points to a **server-side or ssh-side** limit independent of the
client `ReadTimeout` → file a follow-up bead (see Open questions #3). This plan
fixes the confirmed dominant 300s client-deadline; it does not claim to fix a
hypothetical second wall.

## Open questions (reviewer / architect — none are PM-only)

1. **Default value: 30m (recommended) vs unbounded (`0`).** Chose bounded-30m
   because the pool reconciler runs `bd dolt push` non-interactively — an
   unbounded default could hang an automated push forever. `0` remains available
   as an explicit operator escape hatch (SIGINT is wired at `cmd/bd/main.go:1361`,
   so interactive unbounded is recoverable). Reviewer to confirm 30m.
2. **Env name governs both directions.** `BEADS_DOLT_PUSH_TIMEOUT` is applied to
   push *and* pull (shared helper). Slight misnomer for pull, but it matches the
   sysadmin's report and the acceptance criteria verbatim. Alternative: add a
   `BEADS_DOLT_SYNC_TIMEOUT` canonical name with `_PUSH_` as alias — judged
   gold-plating. Reviewer to confirm single-name is acceptable.
3. **The 7s-earlier data-channel broken pipe.** Sysadmin chrono showed the data
   channel break ~7s *before* the SQL-channel i/o timeout. The dominant, in-our-
   code 300s signal is the client `ReadTimeout`; the earlier data break may be a
   secondary server/ssh-side limit. Confirm during ops validation (step above);
   if a residual hard wall remains, follow-up bead — out of scope here.
4. **Third `ReadTimeout` @~2290.** Executor must confirm it is the steady-state
   pool default (normal queries) and leave it unchanged.

## GDPR data-flow impact

None. This changes only a client-side MySQL **read deadline** on the beads
issue-tracker's Dolt remote-sync (developer tooling). No personal-data
processing path is created or altered: `bd dolt push` transmits issue-tracker
metadata (titles/descriptions authored by developers) to a `git+ssh` remote, and
the change governs only how long the client waits — not what is transmitted,
stored, or who can access it. No data-subject rights (Art. 15–20) are touched;
no new PII flow.

## MDR Class I traceability

No-op. This code is not in the voxmemo → voxist-api clinical pipeline; it is
issue-tracker sync infrastructure. No chain-of-evidence metadata (microphone →
exported clinical note) is involved. Heading retained per planner discipline so
an auditor sees the explicit consideration.
