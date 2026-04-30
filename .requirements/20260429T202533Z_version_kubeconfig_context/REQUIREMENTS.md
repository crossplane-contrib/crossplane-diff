# Fix `version` server lookup to respect kubeconfig context

GitHub Issue: https://github.com/crossplane-contrib/crossplane-diff/issues/285

## As Is

`crossplane-diff version` fetches the server (Crossplane) version by calling `xpversion.FetchCrossplaneVersion(ctx)` from `github.com/crossplane/crossplane/v2/cmd/crank/version`. Internally that function calls `ctrl.GetConfig()` (from controller-runtime), which prefers the **in-cluster ServiceAccount config** over kubeconfig. When `crossplane-diff` runs inside a pod:

- The command ignores the kubeconfig context set by `kubectl config use-context <ctx>`.
- The command ignores the `--context` flag that the rest of the CLI supports.
- Users see `Error: unable to get crossplane version: Crossplane version or image tag not found` because the lookup targets the management cluster (no Crossplane) instead of the switched-to cluster.

Meanwhile `crossplane-diff xr` / `crossplane-diff comp` *do* respect the kubeconfig context because they build their REST config via `provideRestConfig` in `cmd/diff/main.go`, which uses `clientcmd.NewDefaultClientConfigLoadingRules()` + configurable `ConfigOverrides{CurrentContext: ...}`.

Additionally, the `Context KubeContext` flag is declared on `CommonCmdFields` and embedded only by `XRCmd` / `CompCmd`. `versioncmd.Cmd` is in a separate package and does not have a `--context` flag, so there is no way today for a user to force the version command to look at a specific kubeconfig context.

## To Be

`crossplane-diff version` builds its Kubernetes REST config using the same kubeconfig-aware loading path as `xr` and `comp`:

- When run outside a pod: uses `~/.kube/config` (or `$KUBECONFIG`), honoring the kubeconfig's current context.
- When run inside a pod: still uses the kubeconfig's current context, **not** the in-cluster ServiceAccount — unless no kubeconfig is discoverable (then fall back to in-cluster, matching controller-runtime's current behavior so we don't regress the common case).
- Accepts a `--context` flag matching the `xr`/`comp` CLI, so users can override the context explicitly (e.g. `crossplane-diff version --context my-ctx`).
- Still supports `--client` as an existing-behavior short-circuit that never contacts a cluster.

## Requirements

1. **R1 — Kubeconfig context is respected for server version lookup.**
   The version command must use the same REST-config resolution strategy as `xr`/`comp`: `clientcmd` loading rules with optional `CurrentContext` override, **not** `ctrl.GetConfig()`.

2. **R2 — `--context` flag on `version` command.**
   The version command must accept `--context <name>` identical in semantics to `xr --context` / `comp --context`.

3. **R3 — `--client` flag preserved.**
   Existing `--client` behavior is unchanged: prints only client version, never attempts any REST/kube call.

4. **R4 — Error surface unchanged on failure path.**
   When the server lookup fails, error wrapping remains `unable to get crossplane version: <underlying>` so existing scripts/docs don't break.

5. **R5 — No regression for the common out-of-cluster case.**
   Running `crossplane-diff version` on a developer laptop continues to read `~/.kube/config` and return the current context's server version.

6. **R6 — Minimal change footprint.**
   Do not introduce new abstractions or shuffle packages. Reuse existing patterns (`ContextProvider`, `provideRestConfig`) where possible; inline a small helper if reuse across the `main` → `versioncmd` boundary is awkward.

## Acceptance Criteria

- **AC1 (R1, R2, R5):** Given a kubeconfig with two contexts `A` and `B` where A is current, `crossplane-diff version --context B` fetches from cluster B, and bare `crossplane-diff version` fetches from cluster A. Verified via unit test that injects a kubeconfig file and stubs the deployment fetch.
- **AC2 (R1):** Inside a pod with both a mounted ServiceAccount token and an explicit `KUBECONFIG` env var pointing to a kubeconfig with a different cluster, `crossplane-diff version` targets the **kubeconfig** cluster, not the in-pod ServiceAccount cluster. Verified by unit test that sets `KUBECONFIG` and asserts the REST config host matches the kubeconfig's cluster.
- **AC3 (R3):** `crossplane-diff version --client` still returns zero and prints only `Client Version:` with no server contact. Verified by existing `TestCmd_Run_ClientOnly` test continuing to pass.
- **AC4 (R4):** When the deployment list fails, the returned error message starts with `unable to get crossplane version:`. Verified by unit test with an injected fetcher that returns an error.
- **AC5 (R2):** `crossplane-diff version --help` lists a `--context` flag with description matching `xr`/`comp`. Verified by unit test that parses help output via kong.
- **AC6 (R6):** No new Go packages introduced; `versioncmd.Cmd` changes are additive (new field + new dependency); unit test suite in `versioncmd/` keeps working with minimal edits.

## Testing Plan

All tests are Go unit tests in `cmd/diff/versioncmd/version_test.go` (same package) unless noted. No e2e changes required — the bug reproduces only under in-pod conditions that e2e doesn't cover today.

### T1 — `--client` still works (regression)
Existing `TestCmd_Run_ClientOnly` must pass unchanged.

### T2 — Server version fetch uses injected fetcher
Refactor `Cmd.Run` so the fetcher function is an injectable field with a default of the kubeconfig-aware fetcher. Test injects a fake fetcher that:
- asserts it was called (i.e., `--client=false` path reaches it), and
- returns `"v2.0.2"`.
Assert `Server Version: v2.0.2` appears in stdout and `err == nil`.

### T3 — Server version fetch error is wrapped correctly
Inject a fetcher that returns `errors.New("boom")`. Assert `err.Error()` contains `unable to get crossplane version: boom`.

### T4 — Kubeconfig-aware fetcher honors `--context`
Write a small helper (to live alongside `Cmd`) that takes a `*rest.Config` and returns the version. Verify via a separate unit test with a stub `*rest.Config.Host` that the helper uses the config it was handed (not `ctrl.GetConfig()`).

### T5 — Config builder honors `--context` and `$KUBECONFIG`
A unit test writes a temporary kubeconfig with two contexts pointing at different `server:` hosts, sets `$KUBECONFIG`, calls the config builder once with `""` (no override, expect current-context host) and once with the other context name (expect other host).

### T6 — Kong flag parsing
Unit test: run `kong.Parse` against a minimal CLI wrapping `versioncmd.Cmd`, pass `--context foo`, assert the resulting struct has `Context == "foo"`.

## Implementation Plan

Design decisions (confirmed with user):
- **D1:** Inject the REST config via Kong providers, sharing `provideRestConfig` with `xr`/`comp`.
- **D2:** When clientcmd cannot find a kubeconfig, fall back to `rest.InClusterConfig()` and emit a warning to stderr.
- **D3:** `--client` semantics unchanged.

Consequence of D1: we must break the current implicit cycle where `KubeContext`/`ContextProvider` live in the `main` package while `versioncmd` needs to implement `ContextProvider`. Extract those two symbols + `provideRestConfig` into a new small package, `cmd/diff/kubecfg`, that both `main` and `versioncmd` can import.

Smallest sequential steps. Each step runs `go test ./cmd/diff/...` relevant to the change before moving on.

### Step 1 — Create the `cmd/diff/kubecfg` package.
- Move `KubeContext` (rename to `Context`) and `ContextProvider` (rename to `Provider`) into `cmd/diff/kubecfg/kubecfg.go`.
- Move `provideRestConfig` → exported `kubecfg.Provide(Provider) (*rest.Config, error)`.
- Add `rest.InClusterConfig` fallback when `clientcmd` returns `clientcmd.ErrEmptyConfig` (or equivalent "no config" error). On fallback, emit a one-line warning to `os.Stderr` (will later be made injectable).
- **Test T5:** write a unit test in `kubecfg/kubecfg_test.go` that writes a temp kubeconfig with two contexts, sets `$KUBECONFIG`, and asserts:
  - default (empty override) uses current-context host,
  - explicit override uses the other host,
  - missing kubeconfig with in-cluster unavailable returns a descriptive error.
- **Test:** `go test ./cmd/diff/kubecfg/...`.

### Step 2 — Rewire `main.go`, `CommonCmdFields`, `xr.go`, `comp.go`.
- Replace references to `KubeContext` → `kubecfg.Context`, `ContextProvider` → `kubecfg.Provider`.
- `provideRestConfig` in `main.go` becomes a thin forwarder to `kubecfg.Provide`, or is deleted and `kong.BindToProvider(kubecfg.Provide)` is used directly.
- **Test:** `cd cmd/diff && go test ./...`.

### Step 3 — Port `FetchCrossplaneVersion` into `versioncmd` as config-accepting helper.
- New file `cmd/diff/versioncmd/fetch.go` with `FetchCrossplaneVersion(ctx context.Context, cfg *rest.Config) (string, error)`.
- Same logic as upstream (deployment list `app=crossplane`, prefer `app.kubernetes.io/version` label, fallback to image tag).
- Construct the `kubernetes.Clientset` from the passed config instead of calling `ctrl.GetConfig()`.
- **Test T4:** unit test uses `kubernetes/fake.NewSimpleClientset` — extract a tiny `deploymentLister` interface (just `List(ctx, opts) (*appsv1.DeploymentList, error)`) so the fake can be substituted. The exported `FetchCrossplaneVersion(cfg)` stays small; the testable helper is unexported.

### Step 4 — Teach `versioncmd.Cmd` to implement `kubecfg.Provider`.
- Add `Context kubecfg.Context \`help:"Kubernetes context to use (defaults to current context)." name:"context"\`` field to `Cmd`.
- Implement `GetKubeContext() kubecfg.Context`.
- Add `BeforeApply(ctx *kong.Context)` that calls `ctx.BindTo(c, (*kubecfg.Provider)(nil))` mirroring `CommonCmdFields.BeforeApply`.
- **Test T6:** kong.Parse on `&struct{ Version versioncmd.Cmd }` with `version --context foo`; assert `Context == "foo"`.

### Step 5 — Rework `Cmd.Run` to use injected `*rest.Config`.
- Change signature to `Run(k *kong.Context, cfg *rest.Config) error`. Kong will resolve `*rest.Config` via `kubecfg.Provide` because the provider is already bound globally in `main.go`.
- On `c.Client == true`, short-circuit **before** requesting the config (no cluster contact). This requires the config to be an optional dependency — in practice, commands' `Run` signatures pull the config lazily via `kong.BindToProvider`, but because `Run` parameter binding is eager, we need to keep the config out of the signature and instead call `kubecfg.Provide(c)` manually inside `Run` when not in client-only mode. Action: manual call, not parameter injection, to preserve `--client` behavior.
- Call `FetchCrossplaneVersion(ctx, cfg)`; wrap error as `unable to get crossplane version: %w`.
- Introduce an unexported, test-only seam `fetchFn func(ctx, cfg) (string, error)` on `Cmd` that defaults to `FetchCrossplaneVersion`.
- Drop `xpversion` import.
- **Test T2, T3:** inject a fake `fetchFn` in-package.

### Step 6 — Update existing tests.
- `TestCmd_Run_ClientOnly` stays unchanged (T1).
- `TestCmd_Run_ServerVersion` becomes an in-package test that sets `fetchFn` to a stub returning `"v2.0.2"` and asserts output contains `Server Version: v2.0.2`. This makes it deterministic and independent of any cluster state.

### Step 7 — Full tests.
`cd cmd/diff && go test ./...`.

### Step 8 — Manual smoke.
`earthly +build` then:
- `./_output/bin/darwin_arm64/crossplane-diff version --help` — verify `--context` flag visible.
- `./_output/bin/darwin_arm64/crossplane-diff version --client` — verify still works with no cluster.

### Step 9 — Serena memory.
Record a short note about the `cmd/diff/kubecfg` package and the `--context`-honoring version command so future changes to REST-config plumbing stay consistent.
