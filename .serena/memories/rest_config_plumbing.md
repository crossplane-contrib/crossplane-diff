# REST Config Plumbing

All crossplane-diff commands must build their Kubernetes REST config through
`cmd/diff/kubecfg.Provide(kubecfg.Provider)`.

- `kubecfg.Provider` has a single method `GetKubeContext() kubecfg.Context`.
- `main.CommonCmdFields` implements it for `xr` and `comp`; `versioncmd.Cmd`
  implements it for `version`. Each command binds itself via `BeforeApply`
  using `ctx.BindTo(c, (*kubecfg.Provider)(nil))`.
- `kong.BindToProvider(kubecfg.Provide)` is registered once in `main.main`.
  Kong resolves `*rest.Config` lazily per command.
- `kubecfg.Provide`:
  1. Uses `clientcmd.NewDefaultClientConfigLoadingRules()` + optional
     `ConfigOverrides{CurrentContext: ...}` — honors `$KUBECONFIG`,
     `~/.kube/config`, and `--context`.
  2. If `clientcmd.IsEmptyConfig(err)` is true (no kubeconfig at all), falls
     back to `rest.InClusterConfig()` and emits a warning to stderr.
  3. Applies default QPS=20, Burst=30.

## Why this matters (issue #285)
Do **not** call `ctrl.GetConfig()` (controller-runtime) to build a
`*rest.Config`. It prefers in-cluster first — when `crossplane-diff` runs
inside a pod (e.g. GHA runner), that makes the command ignore the user's
`--context` and `kubectl config use-context`. The `versioncmd` originally
used the upstream `cmd/crank/version.FetchCrossplaneVersion`, which has this
problem. We now vendor a copy in `versioncmd/fetch.go` that accepts an
already-built `*rest.Config`.

## New command checklist
When adding a new subcommand that talks to a cluster:
1. Embed `main.CommonCmdFields`, OR expose a `--context` flag and implement
   `kubecfg.Provider` directly, plus a `BeforeApply` that binds.
2. Accept `*rest.Config` (or `*AppContext`) via Kong injection.
3. Do not call `ctrl.GetConfig()` or any function that does.
