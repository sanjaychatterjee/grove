# Implementation Plan: MinAvailable Disaggregation and Promotion to `grove.io/v1beta1`

Source design: GREP-0877: Terminal States, Termination and Restart Policies ([PR #878](https://github.com/ai-dynamo/grove/pull/878), revision `37c2bb8`, at `docs/proposals/0877-termination-and-restart-policies/README.md` on the `terminal-states` branch). This plan details **Phase 1** of the GREP's Implementation Plan. The code references below were checked against upstream `main` at `a5be55f`. It covers the sections "Disaggregating MinAvailable", "API changes", "Defaults", "Validation" and "Migration from legacy fields". Tracking issue: [#877](https://github.com/ai-dynamo/grove/issues/877).

## 1. Scope

In scope (GREP Phase 1):

- Split `MinAvailable` on PodClique (PCLQ) and PodCliqueScalingGroup (PCSG) into three fields:
  - `init.minReplicas` for gang scheduling;
  - `rollingUpdate.minReplicas` for the Coherent minimum update unit, on standalone PCLQs and PCSGs only, and only under `Coherent`;
  - `termination.resource.availability.minReplicas` for the availability floor.
- Replace the PCS-wide `template.terminationDelay` with a per-component `termination.resource.availability.breachTolerance`.
- Change `rollingUpdate.maxUnavailable` from `int32` to int-or-percentage.
- Drop the deprecated `topologyConstraint.packDomain` from v1beta1. Conversion maps it to `pack.required`.
- Introduce `grove.io/v1beta1` for all five kinds in the group: PodCliqueSet, PodClique, PodCliqueScalingGroup, PodGangMap and ClusterTopologyBinding. v1beta1 is the conversion hub and, one release later, the storage version. It carries only the new fields.
- A conversion webhook, so existing `v1alpha1` objects, manifests and Go clients keep working unchanged.

Unchanged by this plan:

- `PodCliqueSetSpec.updateStrategy`, including its `RollingRecreate` default;
- `rollingUpdate.progressDeadline`;
- the placement of `rollingUpdate` (standalone clique templates and PCSG configs);
- all status types, condition types and reasons;
- the `scheduler.grove.io` group (PodGang), which stays at `v1alpha1`.

Out of scope here (GREP Phase 2, additive to v1beta1):

- terminal states and completion policies;
- restart policies, replica rules and `ttlAfterFinished`;
- the PCS-level `termination` section;
- the `Never`/`OnFailure` pod restart policies.

Behavioural contract, the GREP's Phase 1 exit criteria: **every existing `v1alpha1` workload behaves exactly as before.** That covers gang scheduling, gang termination, rolling updates and the scale guard, and the upgrade recreates no pods. New behaviour appears only through fields that `v1alpha1` cannot express.

## 2. Findings from the current code

1. **The legacy default is `1`.** `PodCliqueSpec.MinAvailable` and `PodCliqueScalingGroupConfig.MinAvailable` carry `+kubebuilder:default=1`, and the API server applies schema defaults before the defaulting webhook runs. The GREP's migration table now matches this.
2. **The generation hashes are safe.** `computeGenerationHash` (`podcliqueset/reconcilespec.go`) and `ComputePCLQPodTemplateHash` (`utils/component/podclique.go`) hash only labels, annotations, pod spec and priority class. Moving the min-replica, update and termination fields therefore cannot trigger a rolling update. The upgrade test asserts this.
3. **All admission webhooks use `matchPolicy: Exact` on `v1alpha1`.** That covers PCS defaulting and validation, PCLQ and PCSG validation (including `/scale`), ClusterTopologyBinding validation, and the authorizer rule for `podcliques`, `podcliquescalinggroups` and `podgangmaps`. If v1beta1 is served without changing them, v1beta1 writes bypass validation and the authorizer. GREP Phase 1 step 2 requires the switch to land in the same change; §6.3 implements it.
4. **CRDs ship through Helm's `crds/` directory** (`operator/hack/prepare-charts.sh`), plus the opt-in `crdInstaller` (GREP-436). Files in `crds/` cannot be templated, but `spec.conversion.webhook.clientConfig.service.namespace` depends on the release namespace. A multi-version CRD without `spec.conversion` falls back to `strategy: None`, under which a v1alpha1 request's `minAvailable` is silently pruned. The GREP says "`spec.conversion` on the CRDs" but not how it is delivered; §6.1 covers that.
5. **Webhook certificates are managed by OPA cert-controller** (`internal/controller/cert/cert.go`) in `auto` mode, and by cert-manager in `manual` mode. cert-controller supports a `CRDConversion` webhook type for `caBundle` injection.
6. **About 267 Go files import `operator/api/core/v1alpha1`**, which is why the import switch is its own mechanical PR (GREP step 3).
7. **About 30 non-test call sites read `MinAvailable`, `TerminationDelay` or the int32 `MaxUnavailable`.** These include the Kueue scheduler backend ([#707](https://github.com/ai-dynamo/grove/pull/707)) and the Coherent sub-step planner. §7.3 maps each one to a purpose.
8. **`updateStrategy.type` is mutable today**, with no immutability check in `pcs/validation`. That is why the GREP needs the immutability exception for `rollingUpdate.minReplicas` when the strategy changes to or from `Coherent` (§4.5).
9. **The PCSG object has no `rollingUpdate`.** The PCSG and PCLQ controllers read the update config from the PCS template (`rollingUpdateConfigForPCSG`, `rollingUpdateConfigForPCLQ`). Only `init` and `termination` are copied into PCSG and PCLQ objects, as the GREP specifies.
10. **`mirrorPCSMetadata` copies PCS annotations onto PodGangs** (`podcliqueset/components/podgang/podgang.go`). The conversion annotation exists only in v1alpha1 representations, and the operator reads v1beta1, so it is never propagated. A test pins this.
11. **There is already an upgrade e2e test**, `Test_VUPG1_UpgradeFromLatestGitHubRelease` in `operator/e2e/tests/upgrade`. It is the home for upgrade and rollback tests.

## 3. Release strategy (GREP Phase 1, steps 7–9)

| Release | v1alpha1 | v1beta1 | Storage | Operator internals | Rollback to previous release |
|---|---|---|---|---|---|
| **N** | served, deprecated | served, final Phase 1 shape | **v1alpha1** | v1beta1 types | Safe. Objects are stored as v1alpha1. Objects that use v1beta1-only values carry the conversion annotation, and N-1 reads them as plain `minAvailable`. This is documented. |
| **N+1** | served, deprecated | served | **v1beta1** | v1beta1 | Safe back to N, which knows v1beta1 and serves `/convert`. |
| **N+1** (post-start) | — | — | Storage migrator rewrites all objects; `status.storedVersions` becomes `["v1beta1"]` | — | — |
| **N+2, N+3** | served, deprecated (the two-release window after the storage migration) | served | v1beta1 | v1beta1 | Safe back to N+1. |
| **N+4** | removed: no longer served, and dropped from the CRDs via the GREP-436 CRD upgrader. The Go package and conversion code are deleted. | served | v1beta1 | v1beta1 | Safe back to N+3, which still serves v1alpha1. `storedVersions` has held only v1beta1 since N+1. |

The deprecation window is the two releases that follow the storage migration in N+1. The v1alpha1 `deprecationWarning` and the release notes from N onward name N+4 as the removal release.

The Phase 1 v1beta1 schema must be final when N is cut, so PRs 1–7 in §8 land before N. Track this with a release-blocking issue, or land the PRs on a feature branch.

Phase 2 can ship in N+1 or later. It needs every v1beta1 object created under Phase 1 to have an unset `resource.restart` (§5.4).

## 4. v1beta1 API

### 4.1 Package layout

- New package `operator/api/core/v1beta1`: `doc.go`, `register.go`, copies of all type files and helper methods (`TopologyConstraint.RequiredDomain` and so on), and `hub.go` with `Hub()` for all five kinds.
- `+kubebuilder:storageversion` stays on v1alpha1 in release N and moves to v1beta1 in N+1.
- Move the CRD output from `api/core/v1alpha1/crds/` to `api/core/crds/`, with one multi-version CRD per kind. Update `embed.go`, `prepare-charts.sh`, `cmd/install-crds` and `internal/crdinstaller`.
- `api/hack/generate.sh`:
  - pass both packages to `controller-gen crd paths=...`;
  - `kube::codegen::gen_helpers` also runs conversion-gen (§5.1);
  - `gen_client` produces typed clients, listers and informers for both versions.
- Mark v1alpha1 deprecated in Go docs. In the CRD, set `deprecated: true` and a `deprecationWarning`, using a post-processing step in `generate.sh`, since controller-gen has no marker for this.

### 4.2 New and changed types

The shapes follow the GREP's API changes exactly. Phase 2 adds `completion` and `restart` to `ResourceTerminationSpec`, and `replica` and `ttlAfterFinished` to `TerminationSpec`, without breaking anything.

```go
// InitSpec is carried by PCSGs and PodCliques only.
type InitSpec struct {
	// +optional
	MinReplicas *int32 `json:"minReplicas,omitempty"`
}

// RollingUpdateConfiguration is the existing type. MinReplicas is new, and MaxUnavailable changes type.
type RollingUpdateConfiguration struct {
	// Only allowed when updateStrategy.type is Coherent.
	// +optional
	MinReplicas *int32 `json:"minReplicas,omitempty"`
	// Integer or percentage of the component's spec.replicas; a percentage rounds down, minimum 1.
	// +optional
	MaxUnavailable *intstr.IntOrString `json:"maxUnavailable,omitempty"`
	// +optional
	ProgressDeadline *metav1.Duration `json:"progressDeadline,omitempty"`
}

// TerminationSpec. Phase 2 adds Replica and TTLAfterFinished.
type TerminationSpec struct {
	// +optional
	Resource *ResourceTerminationSpec `json:"resource,omitempty"`
}

// ResourceTerminationSpec. Phase 2 adds Completion and Restart.
type ResourceTerminationSpec struct {
	// +optional
	Availability *AvailabilityPolicy `json:"availability,omitempty"`
}

type AvailabilityPolicy struct {
	// +optional
	MinReplicas *int32 `json:"minReplicas,omitempty"`
	// +optional
	BreachTolerance *metav1.Duration `json:"breachTolerance,omitempty"`
}
```

### 4.3 Field placement

| Type | Removed in v1beta1 | Added or changed in v1beta1 |
|---|---|---|
| `PodCliqueSetSpec` | — | — (`updateStrategy` unchanged; `termination` arrives in Phase 2) |
| `PodCliqueSetTemplateSpec` | `terminationDelay` | — |
| `PodCliqueTemplateSpec` | — | `rollingUpdate.minReplicas`, `rollingUpdate.maxUnavailable` becomes int-or-percentage |
| `PodCliqueSpec` (template and PCLQ object) | `minAvailable` | `init`, `termination` |
| `PodCliqueScalingGroupConfig` | `minAvailable` | `init`, `termination`, `rollingUpdate.minReplicas`, int-or-percentage `maxUnavailable` |
| `PodCliqueScalingGroupSpec` (PCSG object) | `minAvailable` | `init`, `termination` (copied from the config) |
| `TopologyConstraint` (PCS template, clique templates, PCSG configs) | `packDomain` | — (`pack.required` is the only way to express the required domain) |
| PodGangMap, ClusterTopologyBinding | — | — (identical schema) |

Dropping `packDomain` also simplifies the v1beta1 types and webhooks:

- the three root-level `XValidation` rules on `PodCliqueSet` that forbid `packDomain` on new workloads are removed;
- the `TopologyConstraint` rules become `has(self.pack)`;
- `TopologyConstraint.RequiredDomain()` no longer falls back to `PackDomain`;
- in `pcs/validation/topologyconstraints.go`, the deprecation warning and the update rules that allow a `packDomain` → `pack.required` migration are deleted. Both forms convert to the same v1beta1 value, so that migration is now an identity update.

The v1alpha1 schema keeps its existing `packDomain` CEL rules unchanged.

Printer columns: on `pclq` and `pcsg`, replace `MinAvail` with `InitMin` (`.spec.init.minReplicas`) and `AvailMin` (`.spec.termination.resource.availability.minReplicas`).

### 4.4 Defaults (v1beta1 defaulting webhook, Phase 1 subset of the GREP's Defaults table)

| Field | Default |
|---|---|
| `availability` | nil for new v1beta1 objects. Always set on converted objects (§5.2). |
| `availability.minReplicas`, `breachTolerance` (when `availability` set) | `1`, `4h` |
| `init.minReplicas` (PCSG, PCLQ) | `availability.minReplicas` if set, else `1` |
| `rollingUpdate.minReplicas` (standalone PCLQ, PCSG) | under `Coherent`: `availability.minReplicas` if set, else `1`; not set under other strategies |
| `rollingUpdate.maxUnavailable` | under `Coherent`: `rollingUpdate.minReplicas`; under `RollingRecreate`: `1` (as today); always written as an integer |
| `updateStrategy.type` | `RollingRecreate` (unchanged) |

The CA (completion-aware) column of the GREP table (`init.minReplicas: spec.replicas`) arrives with `completion` in Phase 2.

### 4.5 Validation (v1beta1 validating webhooks)

- **Ordering:** `1 ≤ availability.minReplicas ≤ init.minReplicas ≤ replicas`, and `1 ≤ rollingUpdate.minReplicas ≤ replicas`.
- **Autoscaling:** `scaleConfig.minReplicas ≥ availability.minReplicas`. This replaces the current `≥ minAvailable`.
- **`rollingUpdate` rules:** today's rules stay, with `minReplicas` in place of `minAvailable` (`validateRollingUpdateConfiguration` in `pcs/validation/podcliqueset.go`):
  - `rollingUpdate` is rejected on a PCSG-member clique template;
  - `maxUnavailable` must be an integer `≥ 1` or a percentage in `[1%, 100%]`. There is no upper bound against `replicas`. [#884](https://github.com/ai-dynamo/grove/pull/884) removed it because the live PCLQ or PCSG is scaled independently of the template through `/scale`, and runtime planning already caps disruption at the replicas that remain. The GREP's validation section still says `[1, replicas]` and should be aligned (§11);
  - under `Coherent`, the resolved `maxUnavailable` must be `≥ rollingUpdate.minReplicas`, and the controllers also use `max(resolved, minReplicas)` at run time;
  - `rollingUpdate.minReplicas` is rejected under `RollingRecreate`, and all of `rollingUpdate` is rejected under `OnDelete`, as today.
- **Immutable:** `init.minReplicas`, `rollingUpdate.minReplicas` and `availability.minReplicas`. Adding or removing `availability` is also rejected, because it switches gang termination on or off (§11 item 2).
- **Immutability exception** (finding 8): a request that changes `updateStrategy.type` to `Coherent` may add `rollingUpdate.minReplicas`, and defaulting fills it in if it is omitted. A request that changes the strategy away from `Coherent` must remove it.
- **Mutable:** `breachTolerance`, `maxUnavailable`, `progressDeadline` and `updateStrategy`.
- **Scale guard** (`scaleguard.go`): `spec.replicas` must be `0` or `≥ availability.minReplicas`, falling back to `init.minReplicas` when `availability` is nil. The anchor `PodGroup.minReplicas` is clamped to `min(init.minReplicas, replicas)`.

Validation runs once, on the v1beta1 form (§6.3), so a v1alpha1 user's error messages name v1beta1 paths. Add CEL `XValidation` rules on the v1alpha1 schema for `minAvailable ≤ 0` and `minAvailable > replicas`, so those errors use v1alpha1 field names.

## 5. Conversion

### 5.1 Mechanics

- Hub: v1beta1. Spoke: v1alpha1, implementing `conversion.Convertible`.
- Generate the field copying with **conversion-gen**: add `+k8s:conversion-gen=github.com/ai-dynamo/grove/operator/api/core/v1beta1` to `v1alpha1/doc.go`. conversion-gen refuses to compile until every non-trivial field has a hand-written `Convert_*` function, so a field added in Phase 2 cannot be dropped silently.
- Hand-written functions live in `operator/api/core/v1alpha1/conversion.go`. They must be pure: no API calls, no defaulting, deterministic.
- **PodGangMap and ClusterTopologyBinding** have identical schemas in both versions and use `conversion.strategy: None`, so they do not depend on the webhook. A unit test asserts that the schemas are structurally identical.

### 5.2 Translation from v1alpha1 to v1beta1 (`ConvertTo`), as in the GREP's migration table

| v1alpha1 | v1beta1 |
|---|---|
| Standalone clique `spec.minAvailable: N` (nil means `1`) | `init.minReplicas: N`, `availability.minReplicas: N`; `rollingUpdate.minReplicas: N` only when `updateStrategy.type` is `Coherent` |
| PCSG-member clique `spec.minAvailable: N` (nil means `1`) | `init.minReplicas: N`, `availability.minReplicas: N` |
| PCSG config / PCSG object `minAvailable: N` (nil means `1`) | same as a standalone clique, on the PCSG |
| `template.terminationDelay: D` (nil means `4h`) | `availability.breachTolerance: D` on every clique and every PCSG config |
| `rollingUpdate.maxUnavailable: N` (`int32`) | `intstr.FromInt32(N)` |
| `topologyConstraint.packDomain: X` (PCS template, clique templates, PCSG configs) | `topologyConstraint.pack.required: X`, creating `pack` if needed. The v1alpha1 CEL rules already forbid setting both. |
| `updateStrategy`, `rollingUpdate.progressDeadline` | unchanged |

`availability` is always set on converted objects, which is what keeps gang termination firing. The conversion of a PCS template reads `updateStrategy.type` from the same object, so the `Coherent`-only row is well defined. When `updateStrategy` is nil, it is treated as `RollingRecreate`.

**PodClique and PodCliqueScalingGroup objects** use the per-object rows. A v1alpha1 PCLQ or PCSG object has no `terminationDelay`, and its `rollingUpdate` lives on the PCS (finding 9). **Its `breachTolerance` comes from the owning PCS's v1alpha1 `template.terminationDelay`** (nil means `4h`). Conversion is pure and cannot read the owning PCS, so the value is resolved in three places:

1. **Conversion of the PCS** writes `terminationDelay` into every component's `breachTolerance` in the template (table above).
2. **Conversion of the child** restores `breachTolerance` from the conversion annotation when present. Otherwise it leaves `availability.breachTolerance` unset, and never invents a default.
3. **The `BreachTolerance` accessor** returns the child's own value when set. Otherwise it returns the owning PCS template's `breachTolerance` for that component, which step 1 derived from `terminationDelay`. The PCS controller also writes the resolved value into the child spec on its next sync, so it becomes visible on the object. During release N, that value survives storage at v1alpha1 through the conversion annotation.

The result always equals the PCS-level `terminationDelay` for converted objects, and no reconcile ever sees a different value.

### 5.3 Translation from v1beta1 to v1alpha1 (`ConvertFrom`) and round-trip fidelity

1. **Closest v1alpha1 form:**
   - `minAvailable` = `init.minReplicas`, as specified by the GREP;
   - `terminationDelay` = the common `breachTolerance` if all components agree, else the maximum;
   - a percentage `maxUnavailable` is resolved against the template's `replicas`, rounding down with a minimum of 1. Under `Coherent` the result is raised to `minAvailable`, so a v1alpha1 client that drops the annotation still passes the `maxUnavailable ≥ minAvailable` check. The template count can differ from the live component, which is scaled independently through `/scale` (#884). The resolved integer is therefore only an approximation for v1alpha1 readers. The exact percentage stays in the stash, and the controllers always resolve against the live PCLQ or PCSG `spec.replicas`;
   - `pack.required` is written as `pack.required`, never as `packDomain`. Workloads that used `packDomain` therefore read back through v1alpha1 in the non-deprecated form. A marker in the stash restores the `packDomain` form on the next v1alpha1 read, so GitOps tools comparing v1alpha1 manifests do not report drift.
2. **Stash:** if anything was lost, marshal the original v1beta1 values into the annotation `grove.io/v1beta1-conversion-data`, as named by the GREP. Values that can be lost:
   - `init.minReplicas` ≠ `availability.minReplicas`;
   - `availability` nil;
   - `rollingUpdate.minReplicas` ≠ `init.minReplicas`;
     - per-component tolerances;
     - a percentage `maxUnavailable`;
     - the fact that a topology constraint was originally written as `packDomain`.

   Store the stash as v1beta1 sub-trees keyed by field path, not as a fixed struct. Phase 2's `restart`, `completion`, `replica` and `ttlAfterFinished` then go in the same annotation without a format change.
3. **Restore in `ConvertTo`:** translate as in §5.2. If the annotation is present, overlay a stashed value only where the v1alpha1 field still equals what `ConvertFrom` wrote, then delete the annotation. Two extra rules:
   - a stashed `rollingUpdate.minReplicas` is dropped when the converted `updateStrategy.type` is no longer `Coherent`, so that a v1alpha1 client that switches strategy produces a valid object;
   - the annotation is never persisted at v1beta1.

Consequences for the docs:

- `kubectl apply`, server-side apply and Go `Update` from a v1alpha1 client keep v1beta1-only values.
- `kubectl replace` with a v1alpha1 manifest that drops the annotation re-derives everything from `minAvailable`. Immutability validation then rejects the request rather than silently changing behaviour.
- In release N, with storage still v1alpha1, objects created at v1beta1 live in etcd with this annotation, so the round trip is load-bearing. Fuzz tests cover it (§9).

### 5.4 Forward compatibility with Phase 2

The GREP's Phase 2 migration treats **an unset `resource.restart` on a long-running PCLQ or PCSG as "created before Phase 2"** and keeps today's escalation for it. Phase 1 must preserve that signal:

- Phase 1 conversion and defaulting never write a `restart` field. The field does not exist in Phase 1, which conversion-gen enforces.
- The Phase 1 stash never contains `restart`, so a v1alpha1 round trip cannot introduce one.
- Phase 1's per-component gang termination (§7.3) is exactly the behaviour that Phase 2's `onFailure: {policy: Limit, limit: 0}` fallback reproduces. The Phase 2 equivalence suite reuses the Phase 1 e2e tests unchanged.

### 5.5 Conversion webhook server

- Register `/convert` on the existing webhook server with controller-runtime's conversion handler, built from a scheme with both versions. `internal/webhook/register.go` gains an always-on `conversion` registration.
- `/convert` must be serving before the manager's caches start. In N+1, before migration, the operator's own list/watch goes through it. controller-runtime starts webhook runnables before caches; add a guard test and a readiness check on `/convert`.
- `WaitTillWebhookCertsReady` must also wait for the CRD `caBundle` injection (§6.2) before controllers start.
- `/convert` is served on every operator replica, not just the leader.
- Metrics: `grove_conversion_requests_total`, `grove_conversion_duration_seconds` and `grove_conversion_failures_total`, per kind and direction.

## 6. Delivery: CRDs, certificates and webhook configuration

### 6.1 CRD `spec.conversion`

The PodCliqueSet, PodClique and PodCliqueScalingGroup CRDs need `spec.conversion.webhook.clientConfig.service.{name, namespace, path}`.

| Option | Pros | Cons |
|---|---|---|
| **A (recommended).** Move these three CRDs from `crds/` to `templates/` with `helm.sh/resource-policy: keep`. Template `spec.conversion` from `Release.Namespace` and `service.name`. | Conversion config is always correct and applied atomically with the operator upgrade. Helm also upgrades these CRDs. | Diverges from GREP-436's `crds/` choice for three kinds. Existing installs need an adoption step: Helm ownership labels and annotations on the CRDs, set by the `crdInstaller` or a pre-upgrade hook. |
| B. Keep `crds/`. The `crdInstaller` (namespace from the downward API) or the operator at startup patches `spec.conversion`. | Keeps the GREP-436 layout. | Until the patch lands, the CRD has two versions and `strategy: None`, so fields are silently pruned (finding 4). It also makes `crdInstaller` mandatory. |

In either option, the `crdInstaller` renders `spec.conversion` itself when enabled, so `install-crds` and Helm stay consistent.

### 6.2 Certificates

- **`auto` mode:** add `cert.WebhookInfo{Type: cert.CRDConversion, Name: "<plural>.grove.io"}` for the three CRDs in `getWebhooks`. Grant `get`, `list`, `watch`, `update` and `patch` on `customresourcedefinitions` in `clusterrole.yaml`, restricted by `resourceNames`.
  - The existing CRD rule in `clusterrole.yaml` is inside `{{- if .Values.crdInstaller.enabled }}` and has no `list` or `watch`. The conversion rule must be a separate, **unconditional** rule, because `caBundle` injection is needed whether or not the installer is enabled.
  - Leave the legacy `clustertopologies.grove.io` name out of it; that CRD is never converted.
- **`manual` mode:** add `cert-manager.io/inject-ca-from: <ns>/<cert>` to the templated CRDs. Option A makes this possible.
- Extend `e2e/tests/cert_management_test.go` to cover CRD `caBundle` injection and rotation in both modes.

### 6.3 Admission webhook configurations

Change every `grove.io` rule in `charts/templates/*-webhook-config.yaml` to `apiVersions: ["v1beta1"]` with `matchPolicy: Equivalent`. This covers:

- PCS defaulting and validation;
- PCLQ and PCSG validation, including `/scale`;
- ClusterTopologyBinding validation;
- the `grove.io` rule of the authorizer.

The API server then converts v1alpha1 requests before calling the webhooks, so defaulting and validation have a single v1beta1 implementation. This lands in PR 2, the first PR that serves v1beta1.

Because every request now arrives as v1beta1, every handler under `internal/webhook/admission` must decode into v1beta1 types in the same PR. That covers PCS defaulting and validation, PCSG validation, ClusterTopologyBinding validation, the authorizer, and the PCLQ handler's `decodeReplicas` for Kueue scale validation (#707). Decoding v1beta1 into the v1alpha1 Go types fails or silently mis-decodes. PR 2 therefore moves `internal/webhook/...` to v1beta1 ahead of the general import switch in PR 3. This is cheap, because the schemas are still identical in PR 2. Add a test in `charts/deployment_test.go` that fails if any `grove.io` rule uses `Exact` or omits v1beta1.

## 7. Operator changes

### 7.1 Accessor layer (GREP step 1, lands first on v1alpha1)

These helpers go in `internal/utils/component`. On v1alpha1, each returns today's value (`MinAvailable`, `TerminationDelay` or the int32 `MaxUnavailable`), so the PR changes no behaviour, and every call site is classified.

```go
func InitMinReplicas(spec) int32                       // gang scheduling
func AvailabilityMinReplicas(spec) (int32, bool)       // breach threshold; false = availability not set
func ReadyThreshold(spec) int32                        // "available" counting for status: availability.minReplicas if set, else init.minReplicas
func UpdateMinReplicas(pcs, component) int32           // MVU share and update-completion threshold
func ResolvedMaxUnavailable(pcs, component) int32      // percentage of the live component's spec.replicas, plus max(·, minReplicas) under Coherent
func BreachTolerance(pcs, component) time.Duration
```

`UpdateMinReplicas` returns:

- under `Coherent`, for a standalone PCLQ or a PCSG: `rollingUpdate.minReplicas`;
- otherwise, including for every PCSG member: `init.minReplicas`, which conversion sets to the legacy `minAvailable`.

This implements the GREP's clarification 12 and keeps today's RollingRecreate thresholds.

`ReadyThreshold` is the per-PCLQ (or per-PCSG) count of ready replicas needed for the resource to count as available in status. It feeds PCSG and PCS `availableReplicas` and the PCSG replica ready state.

- When `availability` is set, it is `availability.minReplicas`, which equals the legacy `minAvailable` for converted objects, so status is unchanged.
- When `availability` is nil, it is `init.minReplicas`: the resource counts as available once its gang-scheduled minimum is ready. This matches the GREP's clarification 12, which uses a member's `init.minReplicas` for the PCSG replica ready checks.

### 7.2 Mechanical import switch (GREP step 3)

Switch the alias `grovecorev1alpha1` to `grovecorev1beta1` across `operator/internal`, `operator/api/common`, `operator/cmd`, `operator/initc`, `operator/test/utils` and `operator/e2e`. Do it as one scripted rename (`gofmt -r` / `goimports`), reviewed as a pure rename. The scheme registers both versions.

### 7.3 Call-site mapping

| Call site | Today | v1beta1 accessor |
|---|---|---|
| `podcliqueset/components/podgang/syncflow.go` (PodGroup `minReplicas`, for standalone cliques and, through `pclqInfo.minAvailable`, for PCSG members) | clique `MinAvailable` | `InitMinReplicas` |
| `podcliqueset/components/podgangmap/steadystate.go` (Anchor/Tail split) | PCSG `MinAvailable` | `InitMinReplicas` |
| `utils/component/podcliqueset.go` `GetPCSGMinAvailableFromPCSTemplateSpec` | PCSG `MinAvailable` | rename it, and use `InitMinReplicas` |
| `utils/component/podgangmap.go` `minAvailableForClique` (startup-dependency wait) | clique `MinAvailable` | `InitMinReplicas` |
| `api/common/namegen.go` `GeneratePodGangNameForPodCliqueOwnedByPCSG` | PCSG `MinAvailable` | `InitMinReplicas` |
| `podclique/reconcilestatus.go` `PodCliqueScheduled` | clique `MinAvailable` | `InitMinReplicas` |
| `podclique/reconcilestatus.go` `MinAvailableBreached` (including the scheduled-below branch) | clique `MinAvailable` | `AvailabilityMinReplicas`; permanently `False` when not set |
| `podcliquescalinggroup/reconcilestatus.go` `MinAvailableBreached` | PCSG `MinAvailable` | `AvailabilityMinReplicas` |
| `podcliquescalinggroup/reconcilestatus.go` replica available (line 182) | member `MinAvailable` | `ReadyThreshold` |
| `podcliqueset/reconcilestatus.go` PCS replica availability: standalone PCLQ `ReadyReplicas ≥` and PCSG `AvailableReplicas ≥` threshold | clique and PCSG `MinAvailable` | `ReadyThreshold` (PCLQ) and `ReadyThreshold` (PCSG) |
| `podcliquescalinggroup/reconcilestatus.go` `isPCSGChildPCLQUpdated` (ready and updated checks) | member `MinAvailable` | `UpdateMinReplicas`, which is the member's `init.minReplicas` (clarification 12) |
| `podcliquescalinggroup/components/podclique/rollingupdate.go` `isReplicaUpdated` | member `MinAvailable` | `UpdateMinReplicas` (member) |
| `podcliquescalinggroup/components/podclique/rollingupdate.go` max unavailable | PCSG `MinAvailable`, `RollingUpdate` | `ResolvedMaxUnavailable` |
| `utils/component/podcliquescalinggroup.go` replica state: scheduled / ready | member `MinAvailable` | `InitMinReplicas` / `ReadyThreshold` |
| `podcliquescalinggroup/components/podclique/sync.go` replica-recycle guard and delay | PCSG `MinAvailable`, PCS `TerminationDelay` | `AvailabilityMinReplicas`; member's `BreachTolerance` |
| `podcliqueset/components/podcliquesetreplica/gangterminate.go` | PCS `TerminationDelay` | each breached child's `BreachTolerance` |
| `podclique/reconciler.go` resync interval | PCS `TerminationDelay` | the PCLQ's `BreachTolerance` |
| `podcliqueset/components/podcliquesetreplica/rollingupdate.go` scheduled-pod count | member `MinAvailable` | `InitMinReplicas` |
| `utils/component/rollingupdate.go` `EffectiveMaxUnavailable`, `CoherentMinAvailableByComponent`, `CoherentMaxUnavailableByComponent` | `MinAvailable`, int32 `MaxUnavailable` | `UpdateMinReplicas`, `ResolvedMaxUnavailable` |
| `podgangmap/syncflow.go` `computeMVUTemplate` | `MinAvailable` (via `CoherentMinAvailableByComponent`) | `UpdateMinReplicas`; the resolved `maxUnavailable` is frozen in the plan when the update starts (deferred, §11 item 6) |
| `podgangmap/substepplanner.go` (anchor-bearing step targets and the per-sub-step budget) | `CoherentMinAvailableByComponent`, `CoherentMaxUnavailableByComponent` | no direct change; it consumes the two helpers above |
| `scheduler/kueue/backend.go` `basePodGangPodGroupCount` | PCSG `MinAvailable` | `InitMinReplicas` (PCSG) |
| `scheduler/kueue/backend.go` partial-gang rejection (`0 < minAvailable < replicas`) | clique `MinAvailable` | `InitMinReplicas`. Its comment "a nil minAvailable defaults to replicas" is wrong today (the schema default is `1`) and becomes moot, because `init.minReplicas` is always set after defaulting. |
| `podclique/components/pod/rollingupdate.go` max unavailable | PCLQ `MinAvailable`, `RollingUpdate` | `ResolvedMaxUnavailable` |
| `utils/component/podclique.go` `IsPCLQUpdateComplete` (updated and ready ≥ threshold) | `MinAvailable` | `UpdateMinReplicas` |
| `podcliqueset/components/podcliquescalinggroup/podcliquescalinggroup.go` (PCSG spec build) | copies `MinAvailable` | copies `init`, `termination` |
| `podcliqueset/components/podclique` (PCLQ spec build) | copies the template spec | also writes the resolved `breachTolerance` |
| `webhook/admission/scaleguard/scaleguard.go` | `MinAvailable` | §4.5 rule |
| `webhook/admission/pcs/defaulting`, `pcs/validation` | `MinAvailable`, `TerminationDelay` (`defaultTerminationDelay`), int32 `MaxUnavailable` | rewritten per §4.4–4.5; `defaultTerminationDelay` becomes the `breachTolerance` default |
| `webhook/admission/pclq/validation/handler.go` `decodeReplicas` (Kueue scale validation, #707) | decodes `grovecorev1alpha1.PodClique` | decodes `grovecorev1beta1.PodClique`, in PR 2 (§6.3) |
| `api/common/constants/constants.go` | doc comments | comments only; condition and reason values unchanged |
| `api/core/v1beta1` `TopologyConstraint.RequiredDomain`, `HasAnyPackDomain` | fall back to `PackDomain` | read `pack` only |
| `webhook/admission/pcs/validation/topologyconstraints.go` (`packDomain` warning, domain check, migration and immutability rules) | `PackDomain` | the `packDomain` branches are deleted; immutability compares `pack` only |
| `utils/component/topologyconstraint.go`, `podgang/syncflow.go` | via `RequiredDomain()` | unchanged call sites; the helper now reads `pack` only |

For converted objects, per-component `breachTolerance` is a no-op, because every component receives the same `terminationDelay`. With distinct values, a PCS replica is gang-terminated when any child has been breached for longer than that child's tolerance. The requeue uses the minimum remaining wait across children, which `GetMinAvailableBreachedPCLQInfo` already computes; it gains a per-PCLQ tolerance input.

### 7.4 Storage version migration (GREP step 8, release N+1)

- A leader-only runnable, `internal/storagemigrator`, modelled on `internal/podgangmigrator`. For each kind, it lists all objects and issues an unchanged `Update`. The API server re-encodes each object at the storage version, because the bytes differ.
- It retries on conflict and runs with bounded concurrency.
- When it finishes, it patches `status.storedVersions` to `["v1beta1"]`. This needs RBAC on `customresourcedefinitions/status` with `resourceNames`.
- It is idempotent. It can be disabled by operator config for clusters that run kube-storage-version-migrator.

## 8. PR sequence

Numbers in brackets are the GREP's Phase 1 steps.

| PR | Content | Behaviour change |
|---|---|---|
| **0** | Merge GREP-0877 ([#878](https://github.com/ai-dynamo/grove/pull/878)), folding in the GREP feedback from §11. | None |
| **1** [1] | Accessor layer (§7.1) on v1alpha1, with unit tests that pin today's values. | None |
| **2** [2] | v1beta1 package as an identical copy of v1alpha1, plus `Hub()`. Trivial generated conversion. Multi-version CRDs (storage v1alpha1). `/convert`. CRD `spec.conversion` (§6.1). `caBundle` injection and RBAC (§6.2). **Webhook rules switched to v1beta1 with `Equivalent`**, and `internal/webhook/...` handlers moved to v1beta1 types (§6.3). | None. The schemas are identical. |
| **3** [3] | Mechanical import switch (§7.2) and regenerated clients. | None |
| **4** [4, 5] | v1beta1 schema (§4.2–4.3), including dropping `packDomain`. Hand-written conversion with the stash (§5.2–5.3). Fuzz and table tests. v1alpha1 CEL rules. Deprecation markers. | v1alpha1: none. v1beta1-native: new semantics. |
| **5** [6] | Accessors on v1beta1 fields. Defaulting and validation, including the immutability exception (§4.4–4.5). Scale guard. Per-component `breachTolerance`. Percentage `maxUnavailable` in update orchestration. Printer columns. | Same as PR 4 |
| **6** [10] | Envtest and e2e from §9: upgrade from N-1, rollback, the equivalence suites with v1alpha1 and v1beta1 manifests, and Story 1. | — |
| **7** [7] | Docs and samples:<br>- `docs/api-reference` (add v1beta1 to `hack/generate-api-docs.sh`);<br>- user guide and samples on v1beta1, keeping one v1alpha1 sample;<br>- a migration guide with the §5.2 table;<br>- release notes covering rollback caveats and Go consumers such as Dynamo.<br>**Cut release N.** | — |
| **8** [8] | Release N+1: move `+kubebuilder:storageversion` to v1beta1. Storage migrator (§7.4). N → N+1 upgrade and N+1 → N rollback e2e. | Storage only |
| **9** [9] | Release N+4, after the two-release window (N+2, N+3): stop serving v1alpha1 and remove it from the CRDs through the GREP-436 upgrader. Delete the v1alpha1 package, the conversion code and the v1alpha1 CEL rules. | Breaking for v1alpha1 clients |

PRs 4 and 5 may have to merge together, because removing `minAvailable` breaks every call site that PR 1 has not already routed through an accessor. PR 1 is what keeps that merge reviewable.

## 9. Test plan

**Unit tests:**

- **Round-trip fuzzing** for every kind (`k8s.io/apimachinery/pkg/api/apitesting/fuzzer` and `roundtrip`):
  - v1alpha1 → v1beta1 → v1alpha1 is identical;
  - v1beta1 → v1alpha1 (with annotation) → v1beta1 is identical;
  - the annotation-dropped and strategy-switched cases re-derive values per §5.3.
- **Translation table:** one case per row of §5.2, including:
  - the omitted fields: `minAvailable` → `1`, `terminationDelay` → `4h`, `updateStrategy` → `RollingRecreate`;
  - the `Coherent`-only `rollingUpdate.minReplicas`;
  - a percentage `maxUnavailable` → integer, rounding down with a minimum of 1;
  - `packDomain` → `pack.required` at all three levels, and the v1alpha1 round trip restores the `packDomain` form;
  - a v1alpha1 PCLQ or PCSG object without the annotation leaves `breachTolerance` unset, and the accessor resolves it to the owning PCS's `terminationDelay`, including the nil → `4h` case.
- **Schemas:** PodGangMap and ClusterTopologyBinding schemas are identical across versions.
- **Defaulting and validation:** every §4.4–4.5 rule, including:
  - rejection of adding or removing `availability`;
  - the immutability exception when `updateStrategy.type` changes;
  - a v1alpha1 update that moves `packDomain` to `pack.required` is accepted as an identity change.
- **Ready threshold:** `ReadyThreshold` is `availability.minReplicas` when set, and `init.minReplicas` when `availability` is nil.
- **Accessor parity:** in PR 1, the accessors return today's values; in PR 5, they return the same values for converted objects.
- **Phase 2 safety:** neither conversion nor the stash ever produces a `restart` field (§5.4).
- **Chart:** no `grove.io` webhook rule uses `Exact`, and every rule includes v1beta1.
- **PodGangs:** PodGang annotations never contain `grove.io/v1beta1-conversion-data`.

**Envtest**, with CRD conversion patched to the local webhook server via `WebhookInstallOptions`:

- create at v1alpha1 and read at v1beta1, and the reverse; list/watch at both versions;
- server-side apply of a v1alpha1 manifest onto a v1beta1-native object keeps v1beta1-only values;
- `/scale` at both versions goes through the scale guard;
- the authorizer rejects non-operator writes to PCLQ, PCSG and PodGangMap at both versions.

**E2E:**

- `Test_VUPG1_UpgradeFromLatestGitHubRelease`, extended:
  - deploy N-1 with standalone and PCSG cliques under `Coherent`, `RollingRecreate` and `OnDelete`, with a non-default `terminationDelay` and a topology constraint written with `packDomain`;
  - upgrade to N and assert:
    - pod UIDs, `CurrentGenerationHash` and PodGangs are unchanged;
    - objects read correctly at both versions;
    - a PCLQ breach still gang-terminates after the original delay.
- **N → N+1:** the migrator completes and `storedVersions == ["v1beta1"]`.
- **Rollbacks:** N → N-1 and N+1 → N keep workloads running and readable.
- **Equivalence:** `gang_termination_test.go`, `scaleguard_test.go`, `gang_scheduling_test.go` and the `update/` suite pass with v1alpha1 manifests and again with equivalent v1beta1 manifests.
- **New v1beta1 behaviour:**
  - Story 1: `init.minReplicas: 8` with `availability.minReplicas: 6` tolerates the loss of 2 pods without gang termination.
  - A PCSG whose members have no `availability` counts a replica as available only once each member has `init.minReplicas` ready pods.
  - A Coherent update with `rollingUpdate.minReplicas` larger than `availability.minReplicas` rolls in the larger unit.
  - A percentage `maxUnavailable` is resolved correctly before and after scaling.
  - `rollingUpdate.minReplicas` is rejected under `RollingRecreate` and `OnDelete`, filled in when switching to `Coherent`, and must be removed when switching away.
- **Certificates:** `caBundle` injection in `auto` and `manual` modes.
- **Operator restarts** before storage migration (N+1) recover, because `/convert` comes up before the caches.
- **Kubernetes versions:** the compatibility matrix ([#820](https://github.com/ai-dynamo/grove/pull/820)) runs the standard suites on 1.34 and 1.35, but upgrade tests run only on their dedicated target. Add the conversion envtest and the v1alpha1/v1beta1 equivalence runs to the matrix. Run the N-1 → N upgrade test on both versions at least for release N, because conversion-webhook and `matchPolicy: Equivalent` behaviour is API-server-side.
- **Kueue backend:**
  - with v1alpha1 and with v1beta1 manifests, the Kueue partial-gang rejection and the base PodGang PodGroup count use `init.minReplicas`;
  - Kueue scale validation through the PCLQ webhook works at both versions.

## 10. Risks and mitigations

| Risk | Mitigation |
|---|---|
| **The operator is down, so conversion is down.** v1alpha1 reads fail. In N+1, reads of unmigrated objects fail at any version. | Run 2 or more operator replicas with a PodDisruptionBudget; webhooks run on all replicas. Add `/convert` readiness. The migrator shortens the window. |
| **Silent field pruning under `strategy: None`** (finding 4) | §6.1 option A. The `crdInstaller` always writes `spec.conversion`. A test fails if a multi-version CRD lacks it. |
| **Validation and authorizer bypass at v1beta1** (finding 3) | PR 2 switches every rule to v1beta1 with `Equivalent`, backed by the chart test. |
| **Lossy v1beta1 → v1alpha1 for older clients** such as Dynamo | Round-trip annotation (§5.3), fuzz tests, and release notes asking consumers to move to v1beta1. |
| **A v1alpha1 client switches `updateStrategy` while a stash holds `rollingUpdate.minReplicas`** | The strategy-aware overlay rule (§5.3) and a fuzz case for it. |
| **Percentage `maxUnavailable` drifts with scaling mid-update** | Freeze the resolved value in the Coherent plan; use `max(resolved, minReplicas)` at run time (GREP, Interaction with existing features). |
| **Unsafe rollback after the storage switch** | Two-release strategy (§3). |
| **v1alpha1 consumers still writing `packDomain` see `pack.required` on read** | The stash marker restores the `packDomain` form for v1alpha1 readers. Release notes point consumers to `pack.required`. |
| **Consumers miss the v1alpha1 removal in N+4** | The `deprecationWarning` names the removal release from N onward, and the release notes of N+1 to N+3 repeat it. |
| **Phase 1 accidentally breaks Phase 2's "unset restart means it predates Phase 2" rule** | §5.4, enforced by conversion-gen and a unit test. |
| **Import churn hides semantic changes** | PR 3 is purely mechanical; semantics are isolated in PRs 1, 4 and 5. |
| **v1alpha1 users see v1beta1 field paths in errors** | v1alpha1 CEL rules for the common errors (§4.5). |

## 11. Open questions and GREP feedback

Resolved by GREP revision `37c2bb8`, versus the first draft of this plan:

- the legacy default is `1`;
- under `RollingRecreate`, `maxUnavailable` keeps its default of `1`;
- `maxUnavailable` becomes int-or-percentage, rounding down with a minimum of 1;
- no `UpdateSpec`, and `updateStrategy` stays unchanged;
- no PCS `init`, and the PCS `termination` section arrives in Phase 2;
- PCSG members use `init.minReplicas` as the update threshold;
- the conversion annotation is named `grove.io/v1beta1-conversion-data`;
- storage moves one release later.

Decided for this plan. The GREP should be updated to match, on [#878](https://github.com/ai-dynamo/grove/pull/878):

1. **Ready threshold when `availability` is nil is `init.minReplicas`.** The resource counts as available once its gang-scheduled minimum is ready (§7.1). The GREP should define `ReadyThreshold` alongside clarification 12.
2. **Adding or removing `availability` is rejected** (§4.5). The GREP's immutable-fields list should say so.
3. **`breachTolerance` on converted PCLQ and PCSG objects comes from the PCS-level v1alpha1 `template.terminationDelay`** (§5.2). The value is resolved through the owning PCS, because conversion is pure. The GREP's migration table should add this to its `terminationDelay` row.
4. **`packDomain` is dropped from v1beta1** and converted to `pack.required` (§4.3, §5.2, §5.3). The GREP's API changes and migration table should list it.
5. **v1alpha1 is served for two releases after the storage migration**: N+2 and N+3, with removal in N+4 (§3). This fills in GREP step 9.

GREP text that is out of date relative to `main`:

- **`maxUnavailable` upper bound.** The GREP's validation section requires `[1, replicas]`. [#884](https://github.com/ai-dynamo/grove/pull/884) removed that bound, so the rule should read "integer `≥ 1` or percentage in `[1%, 100%]`" (§4.5). The GREP's "percentage of the component's `spec.replicas`" should say the *live* component's `spec.replicas`.
- **Kueue backend.** The GREP's "Interaction with existing features" lists gang-scheduling call sites but not the Kueue backend ([#707](https://github.com/ai-dynamo/grove/pull/707)). The backend's partial-gang rule and base PodGroup count become `init.minReplicas` (§7.3).

Deferred, to be revisited before the PRs that need them:

6. **Where the resolved `maxUnavailable` is frozen.** The GREP says a Coherent update "freezes the resolved value in its plan". Today's plan state lives in PodGangMap entries and `status.updateProgress`. If a new status field is needed, for example on `PodCliqueSetReplicaUpdateProgress`, it is a Phase 1 API addition and must be in the v1beta1 schema before release N. **Needed by PR 4.**
7. **CRD delivery** (§6.1, option A or B). This needs agreement from the GREP-436 owners, and the GREP's step 2 should name the choice. **Needed by PR 2.**
8. **Conversion strategy for PodGangMap and ClusterTopologyBinding.** `None` is proposed (§5.1). The GREP lists all kinds as converted, without saying how. **Needed by PR 2.**
