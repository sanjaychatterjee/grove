# Implementation Plan: MinAvailable Disaggregation and Promotion to `grove.io/v1beta1`

Source design: GREP-0877: Terminal States, Termination and Restart Policies ([PR #878](https://github.com/ai-dynamo/grove/pull/878), revision `1560cbc`, at `docs/proposals/0877-termination-and-restart-policies/README.md` on the `terminal-states` branch). This plan details **Phase 1** of the GREP's Implementation Plan. The code references below were checked against upstream `main` at `a5be55f`. It covers the sections "Disaggregating MinAvailable", "API changes", "Defaults", "Validation" and "Migration from legacy fields", and clarification 15 (when breach evaluation starts). Tracking issue: [#877](https://github.com/ai-dynamo/grove/issues/877).

## 1. Scope

In scope (GREP Phase 1):

- Split `MinAvailable` on PodClique (PCLQ) and PodCliqueScalingGroup (PCSG) into three fields:
  - `init.minReplicas` for gang scheduling;
  - `rollingUpdate.minReplicas` for the Coherent minimum update unit, on standalone PCLQs and PCSGs only, and only under `Coherent`;
  - `termination.resource.availability.minReplicas` for the availability floor.
- Make the three fields mutable at v1beta1, each with its own effect (§7.7). `v1alpha1` `minAvailable` stays immutable.
- Replace the PCS-wide `template.terminationDelay` with a per-component `termination.resource.availability.breachTolerance`.
- Change `rollingUpdate.maxUnavailable` from `int32` to int-or-percentage.
- Drop the deprecated `topologyConstraint.packDomain` from v1beta1. Conversion maps it to `pack.required`.
- Introduce `grove.io/v1beta1` for all five kinds in the group: PodCliqueSet, PodClique, PodCliqueScalingGroup, PodGangMap and ClusterTopologyBinding. v1beta1 is the conversion hub and, from the same release, the storage version (§3). It carries only the new fields.

In scope, but outside GREP-0877 (it rides on the same v1beta1 promotion):

- Change `PodCliqueSpec.startsAfter` from `[]string` to `[]StartupDependency`. Each dependency names a parent clique and the Pod condition that releases it, which defaults to `Ready` (§4.2). Today the release condition is hard-coded to `PodReady`. Tracking issue: [#859](https://github.com/ai-dynamo/grove/issues/859), "Allow startup dependencies to wait for a pre-Ready workload milestone". The motivating case is the multi-Pod Snapshot restore, where target pods cannot become `Ready` until a restore step that itself waits on the dependency. Waiting on `Ready` would deadlock, so the dependency needs an earlier milestone such as `PlaceholderAvailable`.
- A conversion webhook, so existing `v1alpha1` objects, manifests and Go clients keep working unchanged.
- `MinAvailableBreached` stays `Unknown` until breach evaluation starts: on a PCLQ until its first schedule (`AwaitingInitialSchedule`), on a PCSG until its first availability (`AwaitingInitialAvailability`). This replaces the `WasPCLQEverScheduled` and `WasPCSGEverHealthy` gates (GREP clarification 15, §7.6).

Unchanged by this plan:

- `PodCliqueSetSpec.updateStrategy`, including its `RollingRecreate` default;
- `rollingUpdate.progressDeadline`;
- the placement of `rollingUpdate` (standalone clique templates and PCSG configs);
- all status types and condition types, and every existing reason. `MinAvailableBreached` gains two `Unknown` reasons (§7.6);
- the `scheduler.grove.io` group (PodGang), which stays at `v1alpha1`.

Out of scope here (GREP Phase 2, additive to v1beta1):

- terminal states and completion policies;
- restart policies, replica rules and `ttlAfterFinished`;
- the PCS-level `termination` section;
- the `Never`/`OnFailure` pod restart policies.

Behavioural contract, the GREP's Phase 1 exit criteria: **every existing `v1alpha1` workload behaves exactly as before.** That covers gang scheduling, gang termination, rolling updates and the scale guard, and the upgrade recreates no pods. New behaviour appears only through fields that `v1alpha1` cannot express. The one intended exception is that a PCLQ's breach tolerance is measured from its first schedule rather than from about its creation (§7.6).

## 2. Findings from the current code

1. **The legacy default is `1`.** `PodCliqueSpec.MinAvailable` and `PodCliqueScalingGroupConfig.MinAvailable` carry `+kubebuilder:default=1`, and the API server applies schema defaults before the defaulting webhook runs. The GREP's migration table now matches this.
2. **The generation hashes are safe.** `computeGenerationHash` (`podcliqueset/reconcilespec.go`) and `ComputePCLQPodTemplateHash` (`utils/component/podclique.go`) hash only labels, annotations, pod spec and priority class. Moving the min-replica, update and termination fields therefore cannot trigger a rolling update. The upgrade test asserts this.
3. **All admission webhooks use `matchPolicy: Exact` on `v1alpha1`.** That covers PCS defaulting and validation, PCLQ and PCSG validation (including `/scale`), ClusterTopologyBinding validation, and the authorizer rule for `podcliques`, `podcliquescalinggroups` and `podgangmaps`. If v1beta1 is served without changing them, v1beta1 writes bypass validation and the authorizer. GREP Phase 1 step 2 requires the switch to land in the same change; §6.3 implements it.
4. **CRDs ship through Helm's `crds/` directory** (`operator/hack/prepare-charts.sh`), plus the opt-in `crdInstaller` (GREP-436). Files in `crds/` cannot be templated, but `spec.conversion.webhook.clientConfig.service.namespace` depends on the release namespace. A multi-version CRD without `spec.conversion` falls back to `strategy: None`, under which a v1alpha1 request's `minAvailable` is silently pruned. GREP step 2 therefore ships the three converted CRDs with a fail-closed `Webhook` strategy at a default address, which the operator corrects at run time (option B, §6.1).
5. **Webhook certificates are managed by OPA cert-controller** (`internal/controller/cert/cert.go`) in `auto` mode, and by cert-manager in `manual` mode. cert-controller supports a `CRDConversion` webhook type for `caBundle` injection.
6. **About 267 Go files import `operator/api/core/v1alpha1`**, which is why the import switch is its own mechanical commit (GREP step 3, §8 commit 3).
7. **About 30 non-test call sites read `MinAvailable`, `TerminationDelay` or the int32 `MaxUnavailable`.** These include the Kueue scheduler backend ([#707](https://github.com/ai-dynamo/grove/pull/707)) and the Coherent sub-step planner. §7.3 maps each one to a purpose.
8. **`updateStrategy.type` is mutable today**, with no immutability check in `pcs/validation`. That is why a strategy change must be allowed to add or remove `rollingUpdate.minReplicas` (§4.5).
9. **The PCSG object has no `rollingUpdate`.** The PCSG and PCLQ controllers read the update config from the PCS template (`rollingUpdateConfigForPCSG`, `rollingUpdateConfigForPCLQ`). Only `init` and `termination` are copied into PCSG and PCLQ objects, as the GREP specifies.
10. **`mirrorPCSMetadata` copies PCS annotations onto PodGangs** (`podcliqueset/components/podgang/podgang.go`). The conversion annotation exists only in v1alpha1 representations, and the operator reads v1beta1, so it is never propagated. A test pins this.
11. **There is already an upgrade e2e test**, `Test_VUPG1_UpgradeFromLatestGitHubRelease` in `operator/e2e/tests/upgrade`. It is the home for upgrade and rollback tests.
12. **Startup ordering is enforced by the `grove-initc` init container.** For each parent clique committed in the pod's own PodGang, the operator passes `--podcliques=<pclq-fqn>:<count>` (`generateArgsForInitContainer` in `podclique/components/pod/initcontainer.go`). `<count>` is the parent's `MinAvailable` (`StartupDependencyTargetsInEntry`). The init container then waits until that many pods of each parent have the `PodReady` condition `True` (`initc/internal/wait.go`). The condition type is hard-coded, and the count is a minimum, not "every pod".
    - The init container image tag equals the operator version, so a pod always runs the `initc` from the operator release that created it.
    - `StartsAfter` is immutable (`pcs/validation`) and is not part of the pod-template hash.

## 3. Release strategy (GREP Phase 1, steps 7–9)

As in GREP steps 8 and 9, Phase 1 ships in **one release, N**: v1beta1 is served, becomes the storage version, and all objects are migrated to it.

| Release | v1alpha1 | v1beta1 | Storage | Operator internals | Rollback to previous release |
|---|---|---|---|---|---|
| **N** | served, deprecated | served, final Phase 1 shape | **v1beta1**, switched by the operator once `/convert` is serving. The storage migrator then rewrites all objects, and `status.storedVersions` becomes `["v1beta1"]` (§7.4). | v1beta1 | **Only through the downgrade procedure** (§7.5), run while N is still installed. A plain downgrade to N-1 fails, because the API server rejects N-1's single-version CRDs while `storedVersions` contains v1beta1. |
| **N+1, N+2** | served, deprecated (the two-release window after the storage migration) | served | v1beta1 | v1beta1 | Safe back to N, which knows v1beta1 and serves `/convert`. |
| **N+3** | removed: no longer served, and dropped from the CRDs via the GREP-436 CRD upgrader. The Go package and conversion code are deleted. | served | v1beta1 | v1beta1 | Safe back to N+2, which still serves v1alpha1. `storedVersions` has held only v1beta1 since N. |

The deprecation window is the two releases that follow the storage migration in N. The v1alpha1 `deprecationWarning` and the release notes from N onward name N+3 as the removal release.

**Staged storage switch within N.** A CRD whose storage version is v1beta1 needs `/convert` for every v1alpha1 write. It also needs it for every v1beta1 read of an object still stored at v1alpha1. If N's CRDs arrived with storage already on v1beta1, there would be a window with nothing serving `/convert`. The window runs from the CRD update (applied by N's `crd-installer` init container, by Helm, or by hand) until N's pod is ready and the CRD `caBundle` is injected. During it, the N-1 operator's writes and all v1alpha1 user writes would fail. N therefore switches storage in two stages:

1. **N's CRD manifests keep `storage: true` on v1alpha1.** `+kubebuilder:storageversion` stays on v1alpha1 in the N source. Applying them changes nothing for the running N-1 operator, which reads and writes v1alpha1, the storage version, so it needs no conversion. This is the same zero-risk step as the GREP's separate release.
2. **The N operator switches storage itself.** Once `/convert` passes its readiness check and `caBundle` injection is complete (§5.5), a leader-only runnable patches the CRDs to `storage: true` on v1beta1. The storage migrator then runs (§7.4). Until the switch, N reads v1alpha1-stored objects through `/convert`, which it serves itself.

The storage flag is **sticky**: nothing may move it back to v1alpha1 within N.
- The `crdInstaller` keeps the live storage version when the live CRD already stores v1beta1.
- Helm applies `crds/` only on first install and never on upgrade (§6.1), so `helm upgrade` cannot revert it.
- A manual `kubectl apply` of N's manifests would revert it. The runnable watches the CRDs and switches again, and the migrator re-runs; this is self-healing. The upgrade guide says to use the chart or `install-crds`.

From N+1 the marker is on v1beta1 in the source, and the switch runnable is a no-op. It is deleted in N+3.

**Upgrades must pass through N.** Only N switches storage and migrates objects, and only N carries over the PCSG awaiting state (§7.6). Therefore N+1 and later support upgrades only from N or later, and the release notes say so. Upgrading straight from N-1 would leave objects stored at v1alpha1, which N+3 can no longer read.

The Phase 1 v1beta1 schema must be final when N is cut. All of it lands in one PR, after which N is cut (§8).

What the combined release trades:

- **Gained:** one release instead of two before v1beta1 is the stored form. v1alpha1 is removed one release earlier, in N+3. The round-trip annotation is never persisted in etcd, because storage is v1beta1 as soon as N is up (§5.3).
- **Lost:** a free rollback to N-1. It now needs the downgrade procedure (§7.5), which is tested in e2e and documented in the release notes.

Phase 2 can ship in N+1 or later. It needs every v1beta1 object created under Phase 1 to have an unset `resource.restart` (§5.4).

## 4. v1beta1 API

### 4.1 Package layout

- New package `operator/api/core/v1beta1`: `doc.go`, `register.go`, copies of all type files and helper methods (`TopologyConstraint.RequiredDomain` and so on). The api module stays free of controller-runtime, so the types implement neither `Hub()` nor `Convertible`.
- `+kubebuilder:storageversion` stays on v1alpha1 in the release-N source. The N operator switches the live CRDs to v1beta1 at run time (§3). The marker moves to v1beta1 in N+1.
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

// PodCliqueSetStatus (excerpt). GangShapeRecreate is new, in both versions (§7.7).
type PodCliqueSetStatus struct {
	// GangShapeRecreate is set while replicas with a stale gang shape are being recreated,
	// and cleared once none is left.
	// +optional
	GangShapeRecreate *GangShapeRecreateProgress `json:"gangShapeRecreate,omitempty"`
}

type GangShapeRecreateProgress struct {
	// StartedAt is when the first stale replica was found.
	StartedAt metav1.Time `json:"startedAt"`
	// GangShapeHash is the gang-shape hash the replicas are being recreated to.
	GangShapeHash string `json:"gangShapeHash"`
	// CurrentlyRecreating is the replica that was deleted last, until it is available again.
	// +optional
	CurrentlyRecreating *int32 `json:"currentlyRecreating,omitempty"`
	// StaleReplicaIndices are the replicas still at an older gang shape, in recreate order.
	// +optional
	StaleReplicaIndices []int32 `json:"staleReplicaIndices,omitempty"`
}
```

- The field is in v1alpha1 as well, and conversion copies it. Before the storage switch in release N, storage is v1alpha1 and would otherwise prune it. N-1 never writes it.
- The PCS also gets a `GangShapeRecreateInProgress` condition. It is `True` while `gangShapeRecreate` is set, with reason `RecreatingReplica` or `WaitingForAvailableReplicas`. It is `False` with reason `GangShapeCurrent` otherwise.

The `startsAfter` change, which is outside GREP-0877:

```go
// PodCliqueSpec (excerpt). The field doc keeps today's DAG and cycle-detection text.
type PodCliqueSpec struct {
	// +listType=map
	// +listMapKey=name
	// +optional
	StartsAfter []StartupDependency `json:"startsAfter,omitempty"`
}

// StartupDependency is one startup dependency of a PodClique on a parent PodClique.
type StartupDependency struct {
	// Name is the PCLQ this clique depends on.
	// +kubebuilder:validation:MinLength=1
	Name string `json:"name"`
	// ConditionType is the Pod condition that must be True on the target pods of the parent
	// before the dependency is considered resolved/released. Defaults to "Ready".
	// +kubebuilder:default=Ready
	// +optional
	ConditionType string `json:"conditionType,omitempty"`
}
```

- `listType=map` keyed by `name` makes duplicate names a schema error and lets server-side apply merge entries by name. The v1alpha1 `[]string` stays atomic. Each version has its own schema, so the two list types can differ.
- `ConditionType` is a plain `string`, as requested. It is validated as a Kubernetes qualified name (§4.5). It is either a built-in Pod condition or a custom condition that the parent clique declares in `readinessGates`, such as `example.com/model-loaded`.
- The "target pods" are the number of parent pods the dependent pod waits for, which is the parent's `init.minReplicas` within the pod's own PodGang. It is not every pod of the parent (finding 12; §11 item 13).

How the design meets the required semantics of [#859](https://github.com/ai-dynamo/grove/issues/859):

| #859 requirement | How it is met |
|---|---|
| All required target pods are scheduled under the existing gang and topology constraints | The required pods are the parent's `init.minReplicas` pods in the dependent pod's own PodGang, which is gang-scheduled as a unit. `initc` only starts after its own pod is scheduled. |
| Each target reports a named, durable milestone below `Ready` | A custom `conditionType` must be declared as a readiness gate (`spec.readinessGates`) on the parent, and the workload's controller, sidecar or agent sets it. A pod with that gate cannot be `Ready` until the condition is `True`, so `Ready` always implies the milestone. Pod conditions persist on the Pod object. |
| The milestone is observable and restart-safe | The condition is visible on the Pod status. `initc` re-lists through its informer on restart, and `grove-initc` runs once per pod, so a released dependent stays released when its containers restart. A recreated dependent pod waits again. |
| Stale Pod UIDs or an incomplete target set fail closed | `initc` counts only target pods that are not terminating and whose condition is `True` on the current Pod object. It tracks them by UID, not by name, so a replaced pod with the same name does not inherit its predecessor's milestone. Release requires the full count; a partial set keeps the dependent in `Init` (§7.3). |
| `Ready` remains the default and stays backward compatible | `conditionType` defaults to `Ready`. v1alpha1 converts to `Ready`, and converted workloads get byte-identical init-container args (§5.2, §7.3). |
| Grove stays workload-neutral | Grove only gates release on the named condition. Reaching the milestone, deadlines and restore semantics belong to the external workload controller. |

### 4.3 Field placement

| Type | Removed in v1beta1 | Added or changed in v1beta1 |
|---|---|---|
| `PodCliqueSetSpec` | — | — (`updateStrategy` unchanged; `termination` arrives in Phase 2) |
| `PodCliqueSetTemplateSpec` | `terminationDelay` | — |
| `PodCliqueTemplateSpec` | — | `rollingUpdate.minReplicas`, `rollingUpdate.maxUnavailable` becomes int-or-percentage |
| `PodCliqueSpec` (template and PCLQ object) | `minAvailable` | `init`, `termination`; `startsAfter` becomes `[]StartupDependency` |
| `PodCliqueScalingGroupConfig` | `minAvailable` | `init`, `termination`, `rollingUpdate.minReplicas`, int-or-percentage `maxUnavailable` |
| `PodCliqueScalingGroupSpec` (PCSG object) | `minAvailable` | `init`, `termination` (copied from the config) |
| `TopologyConstraint` (PCS template, clique templates, PCSG configs) | `packDomain` | — (`pack.required` is the only way to express the required domain) |
| PodGangMap, ClusterTopologyBinding | — | — (identical schema) |
| `PodCliqueSetStatus` | — (`podGangStatuses` and the `grove.io` copies of `PodGangStatus` and `PodGangPhase` were never set or read, and were removed from v1alpha1 before this work, so v1beta1 never has them) | — |

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
| `autoScalingConfig.minReplicas` (standalone PCLQ, PCSG; when `autoScalingConfig` is set) | `availability.minReplicas` if set, else `1`. For requests whose `RequestKind` is v1alpha1: `replicas`, as today (`pcs/defaulting/podcliqueset.go`), so v1alpha1 behaviour is unchanged. |
| `startsAfter[].conditionType` | `Ready`, as a schema default. It is always set after admission and conversion. |

The CA (completion-aware) column of the GREP table (`init.minReplicas: spec.replicas`) arrives with `completion` in Phase 2.

### 4.5 Validation (v1beta1 validating webhooks)

- **Ordering:** `1 ≤ availability.minReplicas ≤ init.minReplicas ≤ replicas`, and `1 ≤ rollingUpdate.minReplicas ≤ min(init.minReplicas, replicas)`. `replicas` is the template's, and the rules hold on every write, as today's `minAvailable ≤ replicas` does. A live PCLQ or PCSG can still be scaled below `init.minReplicas` through `/scale`, down to the scale guard's floor.
- **Autoscaling:** `autoScalingConfig.minReplicas ≥ availability.minReplicas`, or `≥ 1` when `availability` is nil. This replaces the current `≥ minAvailable`. The default follows the same floor (§4.4).
- **`rollingUpdate` rules:** today's rules stay, with `minReplicas` in place of `minAvailable` (`validateRollingUpdateConfiguration` in `pcs/validation/podcliqueset.go`):
  - `rollingUpdate` is rejected on a PCSG-member clique template;
  - `maxUnavailable` must be an integer `≥ 1` or a percentage in `[1%, 100%]`. There is no upper bound against `replicas`. [#884](https://github.com/ai-dynamo/grove/pull/884) removed it because the live PCLQ or PCSG is scaled independently of the template through `/scale`, and runtime planning already caps disruption at the replicas that remain;
  - under `Coherent`, the resolved `maxUnavailable` must be `≥ rollingUpdate.minReplicas`, and the controllers also use `max(resolved, minReplicas)` at run time;
  - `rollingUpdate.minReplicas` is rejected under `RollingRecreate`, and all of `rollingUpdate` is rejected under `OnDelete`, as today.
- **Mutable minimum fields** (§7.7, §11 item 18). Every change re-runs the ordering, autoscaling and `rollingUpdate` rules above against the new values. So lowering `init.minReplicas` below `rollingUpdate.minReplicas` must lower the unit in the same edit, and, when `autoScalingConfig` is set, raising `availability.minReplicas` above its stored `minReplicas` must raise that too, because a defaulted value does not follow. When `autoScalingConfig` is nil, `availability.minReplicas` can change on its own.
  - `init.minReplicas`, on clique templates and PCSG configs, is mutable. A change recreates the PCS replicas one at a time.
  - `availability.minReplicas` is mutable and takes effect in place.
  - `rollingUpdate.minReplicas` is mutable, except while a Coherent update is in progress (`IsCoherentUpdateInProgress` on the old object, whose status the request carries). A change applies from the next Coherent update. For the same window, a change of `updateStrategy.type` away from `Coherent` is rejected, because it removes the field.
- **v1alpha1 requests keep today's immutability.** When the request's `RequestKind` is v1alpha1, which `matchPolicy: Equivalent` preserves, a change to any of the three values is rejected. So `minAvailable` stays immutable for v1alpha1 clients, and a lossy `kubectl replace` still fails (§5.3).
- **Strategy switch** (finding 8), at both versions: a request that changes `updateStrategy.type` to `Coherent` may add `rollingUpdate.minReplicas`, and defaulting fills it in if it is omitted. A request that changes the strategy away from `Coherent` must remove it. Neither counts as a change of the value.
- **Immutable:** adding or removing `availability`, because it switches gang termination on or off (§11 item 2).
- **Mutable:** `breachTolerance`, `maxUnavailable`, `progressDeadline` and `updateStrategy`, apart from the Coherent-update window above.
- **`startsAfter`:** today's rules in `pcs/validation/podcliqueset.go` apply to `startsAfter[].name`:
  - allowed only under `startupType: CliqueStartupTypeExplicit`;
  - names must be non-empty, unique (now also enforced by `listType=map`), and must not refer to the clique itself;
  - every name must identify a clique in the template;
  - the dependency graph must be acyclic, checked with `depG.AddDependencies` over the names.

  New for `conditionType`:
  - It must be a valid qualified name (`validation.IsQualifiedName`). Qualified names cannot contain `:`, which keeps the init-container argument format unambiguous (§7.3).
  - The built-in Pod conditions `PodScheduled`, `PodReadyToStartContainers`, `Initialized`, `ContainersReady` and `Ready` are always allowed, because the scheduler and kubelet set them.
  - `DisruptionTarget` is rejected. The control plane sets it when a pod is about to be evicted or preempted, so it would release dependents just as the parent goes away.
  - **Any other (custom) condition is rejected unless the parent clique template's pod spec declares it in `spec.readinessGates`.** The parent may be a standalone clique or a PCSG member; its template is in the same PodCliqueSet. The error names the dependency, the condition and the parent clique. This catches typos at admission. Because `startsAfter` is immutable, a typo would otherwise mean deleting and recreating the PodCliqueSet (§11 item 12).
- **Readiness gates referenced by `startsAfter` cannot be removed.** The parent's pod template stays mutable, so on update the webhook re-runs the rule above against the new template. It rejects a template change that drops a readiness gate still named by a dependent's `startsAfter`. Otherwise an update could recreate the parent pods without the gate, and the dependents would wait forever.
- **`startsAfter` is immutable,** including each `conditionType`, as today (`ValidateImmutableField` in the PCS update path).
- **Scale guard** (`scaleguard.go`): `spec.replicas` must be `0` or `≥ availability.minReplicas`. When `availability` is nil, the floor is `1`, the same as for `autoScalingConfig.minReplicas`, so only the scale-to-zero exception applies. Converted objects always have `availability`, so v1alpha1 workloads keep today's floor of `minAvailable`. The anchor `PodGroup.minReplicas` is clamped to `min(init.minReplicas, replicas)`.

Validation runs once, on the v1beta1 form (§6.3), so a v1alpha1 user's error messages name v1beta1 paths. Add CEL `XValidation` rules on the v1alpha1 schema for `minAvailable ≤ 0` and `minAvailable > replicas`, so those errors use v1alpha1 field names.

## 5. Conversion

### 5.1 Mechanics

- Hub: v1beta1. Spoke: v1alpha1. The operator registers each converted kind with controller-runtime's `NewHubSpokeConverter` and `NewSpokeConverter`, which call the generated and hand-written `Convert_*` functions, so the api module needs no `conversion.Convertible` methods. PodCliqueSet, PodClique and PodCliqueScalingGroup are registered.
- Generate the field copying with **conversion-gen**: add `+k8s:conversion-gen=github.com/ai-dynamo/grove/operator/api/core/v1beta1` to `v1alpha1/doc.go`. conversion-gen refuses to compile until every non-trivial field has a hand-written `Convert_*` function, so a field added in Phase 2 cannot be dropped silently.
- Hand-written functions live in `operator/api/core/v1alpha1/conversion.go`. They must be pure: no API calls, no defaulting, deterministic.
- **PodGangMap and ClusterTopologyBinding** have identical schemas in both versions and use `conversion.strategy: None` (decided, §11 item 10), so they do not depend on the webhook. A unit test asserts that the schemas are structurally identical.

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
| `spec.startsAfter: [a, b]` (clique templates and PCLQ objects) | `startsAfter: [{name: a, conditionType: Ready}, {name: b, conditionType: Ready}]`, in the same order. `conditionType` is written explicitly, not left to the schema default, so the result is the same with or without defaulting. |

`availability` is always set on converted objects, which is what keeps gang termination firing. The conversion of a PCS template reads `updateStrategy.type` from the same object, so the `Coherent`-only row is well defined. When `updateStrategy` is nil, it is treated as `RollingRecreate`.

**PodClique and PodCliqueScalingGroup objects** use the per-object rows. A v1alpha1 PCLQ or PCSG object has no `terminationDelay`, and its `rollingUpdate` lives on the PCS (finding 9). **Its `breachTolerance` comes from the owning PCS's v1alpha1 `template.terminationDelay`** (nil means `4h`). Conversion is pure and cannot read the owning PCS, so the value is resolved in three places:

1. **Conversion of the PCS** writes `terminationDelay` into every component's `breachTolerance` in the template (table above).
2. **Conversion of the child** restores `breachTolerance` from the conversion annotation when present. Otherwise it leaves `availability.breachTolerance` unset, and never invents a default.
3. **The `BreachTolerance` accessor** returns the child's own value when set. Otherwise it returns the owning PCS template's `breachTolerance` for that component, which step 1 derived from `terminationDelay`. The PCS controller also writes the resolved value into the child spec on its next sync, so it becomes visible on the object and is stored natively at v1beta1.

The result always equals the PCS-level `terminationDelay` for converted objects, and no reconcile ever sees a different value.

### 5.3 Translation from v1beta1 to v1alpha1 (`ConvertFrom`) and round-trip fidelity

1. **Closest v1alpha1 form:**
   - `minAvailable` = `init.minReplicas`, as specified by the GREP;
   - `terminationDelay` = the common `breachTolerance` if all components agree, else the maximum;
   - a percentage `maxUnavailable` is resolved against the template's `replicas`, rounding down with a minimum of 1. Under `Coherent` the result is raised to `minAvailable`, so a v1alpha1 client that drops the annotation still passes the `maxUnavailable ≥ minAvailable` check. The template count can differ from the live component, which is scaled independently through `/scale` (#884). The resolved integer is therefore only an approximation for v1alpha1 readers. The exact percentage stays in the stash, and the controllers always resolve against the live PCLQ or PCSG `spec.replicas`;
   - `startsAfter` is written as the list of `name`s, in order;
   - `pack.required` is written as `pack.required`, never as `packDomain`. Workloads that used `packDomain` therefore read back through v1alpha1 in the non-deprecated form. A marker in the stash restores the `packDomain` form on the next v1alpha1 read, so GitOps tools comparing v1alpha1 manifests do not report drift.
2. **Stash:** if anything was lost, marshal the original v1beta1 values into the annotation `grove.io/v1beta1-conversion-data`, as named by the GREP. Values that can be lost:
   - `init.minReplicas` ≠ `availability.minReplicas`;
   - `availability` nil;
   - `rollingUpdate.minReplicas` ≠ `init.minReplicas`;
     - per-component tolerances;
     - a percentage `maxUnavailable`;
     - the fact that a topology constraint was originally written as `packDomain`;
     - a `startsAfter[].conditionType` other than `Ready`, stashed per dependency name.

   Store the stash as v1beta1 sub-trees keyed by field path, not as a fixed struct. Phase 2's `restart`, `completion`, `replica` and `ttlAfterFinished` then go in the same annotation without a format change.
3. **Restore in `ConvertTo`:** translate as in §5.2. If the annotation is present, overlay a stashed value only where the v1alpha1 field still equals what `ConvertFrom` wrote, then delete the annotation. Two extra rules:
   - a stashed `rollingUpdate.minReplicas` is dropped when the converted `updateStrategy.type` is no longer `Coherent`, so that a v1alpha1 client that switches strategy produces a valid object;
   - a stashed `startsAfter[].conditionType` is applied only to a dependency whose name is still in the v1alpha1 list. A removed name drops its stash entry, and a new name gets `Ready`. Immutability validation then decides whether the change is allowed;
   - the annotation is never persisted at v1beta1.

Consequences for the docs:

- `kubectl apply`, server-side apply and Go `Update` from a v1alpha1 client keep v1beta1-only values.
- `kubectl replace` with a v1alpha1 manifest that drops the annotation re-derives everything from `minAvailable`. Immutability validation then rejects the request rather than silently changing behaviour.
- Storage is v1beta1 from the moment N is up (§3), so the annotation never lives in etcd during normal operation. It only exists in v1alpha1 representations returned to clients. It still matters for every v1alpha1 read-modify-write, and it becomes persistent during the downgrade procedure (§7.5), so fuzz tests cover it (§9).

### 5.4 Forward compatibility with Phase 2

The GREP's Phase 2 migration treats **an unset `resource.restart` on a long-running PCLQ or PCSG as "created before Phase 2"** and keeps today's escalation for it. Phase 1 must preserve that signal:

- Phase 1 conversion and defaulting never write a `restart` field. The field does not exist in Phase 1, which conversion-gen enforces.
- The Phase 1 stash never contains `restart`, so a v1alpha1 round trip cannot introduce one.
- Phase 1's per-component gang termination (§7.3) is exactly the behaviour that Phase 2's `onFailure: {policy: Limit, limit: 0}` fallback reproduces. The Phase 2 equivalence suite reuses the Phase 1 e2e tests unchanged.

### 5.5 Conversion webhook server

- Register `/convert` on the existing webhook server with controller-runtime's conversion handler, built from a scheme with both versions. `internal/webhook/register.go` gains an always-on `conversion` registration.
- `/convert` must be serving before anything reads a converted kind at v1beta1. From commit 3, until the migration finishes, the operator's own v1beta1 list/watch of v1alpha1-stored objects goes through it. So the webhooks, including `/convert`, are registered and served before the controllers are added. The pre-start steps that read converted kinds, today only the PodGang migration gate, move to that point and retry until the conversion webhook is reachable through the Service (§8, the commit before 3). `TestRegisterControllersAndWebhooks` pins the order.
- `WaitTillWebhookCertsReady` must also wait for the CRD `caBundle` injection (§6.2) before controllers start.
- `/convert` is served on every operator replica, not just the leader.
- Metrics: `grove_conversion_requests_total`, `grove_conversion_duration_seconds` and `grove_conversion_failures_total`, per kind and direction.

## 6. Delivery: CRDs, certificates and webhook configuration

### 6.1 CRD `spec.conversion`

The PodCliqueSet, PodClique and PodCliqueScalingGroup CRDs need `spec.conversion.webhook.clientConfig.service.{name, namespace, path, port}`. **Option B is chosen** (§11 item 9).

| Option | Pros | Cons |
|---|---|---|
| A. Move these three CRDs from `crds/` to `templates/` with `helm.sh/resource-policy: keep`. Template `spec.conversion` from `Release.Namespace` and `service.name`. | Conversion config is rendered for the release namespace. Helm also upgrades these CRDs. | Helm cannot adopt the CRDs that N-1 installed from `crds/`. The ownership check runs before pre-upgrade hooks, in Helm 3 and 4, so an adoption hook never gets to run. Users would need `--take-ownership` (Helm ≥ 3.17) or a manual relabel step. It also diverges from GREP-436. |
| **B (chosen).** Keep `crds/`. The CRDs ship with a fail-closed `Webhook` strategy at a default address. The `crdInstaller` and the operator correct the address at run time. | Keeps the GREP-436 layout and needs no adoption. A wrong or missing address fails requests instead of pruning fields. | Conversion fails until the operator has corrected the address and the `caBundle` is injected. A manual re-apply of the CRDs resets the address until the operator corrects it again. |

With option B:

- **Generated default.** `api/hack/generate.sh` adds `spec.conversion` to the three CRDs after `controller-gen`: `strategy: Webhook`, service `grove-operator` in namespace `default` (the documented install), path `/convert`, port 9443, and `conversionReviewVersions: [v1]`. There is no `caBundle`. controller-gen has no marker for this. PodGangMap and ClusterTopologyBinding keep `strategy: None` (§5.1).
- **`crdInstaller`.** It reads the operator namespace from the mounted service-account volume, and rewrites the service namespace of every CRD that has a Service-addressed conversion webhook before its server-side apply. The `caBundle` is not part of the applied configuration, so its injector keeps ownership of the field.
- **Operator startup.** Before `mgr.Start`, the operator patches the three CRDs' conversion to its namespace and configured webhook port, with a merge patch that leaves `caBundle` unchanged. It exits with `ExitErrConfigureCRDConversion` if a CRD is missing or does not serve v1beta1. Without that check, the N operator would run against N-1's CRDs, and its v1beta1 admission rules would never match.
- **Keeper.** A runnable on every replica re-applies the address every 30 seconds, so a `kubectl apply` of the CRD manifests heals. In `manual` mode, it also copies `ca.crt` from the server certificate directory into the `caBundle`, which picks up a rotated CA (§6.2).
- **Upgrades.** Helm does not upgrade `crds/`. Without the `crdInstaller`, users apply N's CRDs before `helm upgrade`, as GREP-436 already requires. Applying them while N-1 runs is safe, because N-1 reads and writes the storage version v1alpha1 and never needs conversion (§3). The release notes and the upgrade guide state the order (§8 commit 8).
- GREP step 2 describes option B.

### 6.2 Certificates

- **`auto` mode:** add `cert.WebhookInfo{Type: cert.CRDConversion, Name: "<plural>.grove.io"}` for the three CRDs in `getWebhooks`. cert-controller injects only the `caBundle`, and fails if `clientConfig` is missing, which the generated default (§6.1) rules out.
- **`manual` mode:** the operator's keeper copies `ca.crt` from the mounted certificate Secret into the three CRDs (§6.1). The Secret already has to contain `ca.crt`. The `cert-manager.io/inject-ca-from` annotation is not used, because files in `crds/` cannot be templated per install.
- **RBAC**, unconditional in `clusterrole.yaml`, because the address correction and `caBundle` injection are needed whether or not the installer is enabled:
  - `get`, `list` and `watch` on all `customresourcedefinitions`. cert-controller watches CRDs through an informer, which cannot be restricted by `resourceNames`.
  - `update` and `patch` on the three converted CRDs, restricted by `resourceNames`. The legacy `clustertopologies.grove.io` name stays out of it; that CRD is never converted.
- Extend `e2e/tests/cert_management_test.go` to cover CRD `caBundle` injection and rotation in both modes.

### 6.3 Admission webhook configurations

Change every `grove.io` rule in `charts/templates/*-webhook-config.yaml` to `apiVersions: ["v1beta1"]` with `matchPolicy: Equivalent`. This covers:

- PCS defaulting and validation;
- PCLQ and PCSG validation, including `/scale`;
- ClusterTopologyBinding validation;
- the `grove.io` rule of the authorizer.

The API server then converts v1alpha1 requests before calling the webhooks, so defaulting and validation have a single v1beta1 implementation. This lands in commit 2 of §8, the first commit that serves v1beta1.

Because every request now arrives as v1beta1, every handler under `internal/webhook/admission` must decode into v1beta1 types in the same commit. That covers PCS defaulting and validation, PCSG validation, ClusterTopologyBinding validation, the authorizer, and the PCLQ handler's `decodeReplicas` for Kueue scale validation (#707). Decoding v1beta1 into the v1alpha1 Go types fails or silently mis-decodes. In commit 2, the handlers are registered for v1beta1 types. Their logic and its callees, such as the scheduler backends, still take v1alpha1 until commit 3, so `internal/webhook/admission/v1alpha1bridge` converts the decoded v1beta1 object to v1alpha1 with the generated functions. The defaulting handler converts its result back. Commit 3 removes the bridge. The authorizer's decoder matches the v1beta1 kinds, since requests arrive with them.

All six admission webhooks move to `/webhooks/v1beta1/...` paths. During the N-1 → N rolling update, the Service still routes some requests to N-1 pods after Helm has switched the rules. On the new paths an N-1 pod answers 404 and the request fails closed. On the old paths it would decode a v1beta1 object as v1alpha1 (§10). A test in `charts/webhook_test.go` fails if any webhook uses `Exact`, a `grove.io` rule does not match exactly v1beta1, or a path is not versioned.

## 7. Operator changes

### 7.1 Accessor layer (GREP step 1, §8 commit 1, on v1alpha1)

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

For `startsAfter`, `StartupDependencyCliqueNames` (`utils/component/podgangmap.go`) already serves as the accessor. Commit 1 changes it into:

```go
type StartupDependency struct {
	CliqueName    string
	ConditionType corev1.PodConditionType
}

func StartupDependencies(pcs, cliqueName) ([]StartupDependency, error)
```

- On v1alpha1, every dependency gets `ConditionType: corev1.PodReady`, so nothing changes.
- The implicit dependency of `CliqueStartupTypeInOrder` on the preceding clique also uses `PodReady`. In-order startup has no field to override it.

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
| `podgangmap/syncflow.go` `computeMVUTemplate` | `MinAvailable` (via `CoherentMinAvailableByComponent`) | `UpdateMinReplicas`. The `maxUnavailable` that it consumes is resolved again on every reconcile, and is not frozen (§11 item 8). |
| `podgangmap/substepplanner.go` (anchor-bearing step targets and the per-sub-step budget) | `CoherentMinAvailableByComponent`, `CoherentMaxUnavailableByComponent` | no direct change; it consumes the two helpers above |
| `scheduler/kueue/backend.go` `basePodGangPodGroupCount` | PCSG `MinAvailable` | `InitMinReplicas` (PCSG) |
| `scheduler/kueue/backend.go` partial-gang rejection (`0 < minAvailable < replicas`) | clique `MinAvailable` | `InitMinReplicas`. Its comment "a nil minAvailable defaults to replicas" is wrong today (the schema default is `1`) and becomes moot, because `init.minReplicas` is always set after defaulting. |
| `podclique/components/pod/rollingupdate.go` max unavailable | PCLQ `MinAvailable`, `RollingUpdate` | `ResolvedMaxUnavailable` |
| `utils/component/podclique.go` `IsPCLQUpdateComplete` (updated and ready ≥ threshold) | `MinAvailable` | `UpdateMinReplicas` |
| `podcliqueset/components/podcliquescalinggroup/podcliquescalinggroup.go` (PCSG spec build) | copies `MinAvailable` | copies `init`, `termination` |
| `podcliqueset/components/podclique` (PCLQ spec build) | copies the template spec | also writes the resolved `breachTolerance` |
| `webhook/admission/scaleguard/scaleguard.go` | `MinAvailable` | §4.5 rule |
| `webhook/admission/pcs/defaulting`, `pcs/validation` | `MinAvailable`, `TerminationDelay` (`defaultTerminationDelay`), int32 `MaxUnavailable` | rewritten per §4.4–4.5; `defaultTerminationDelay` becomes the `breachTolerance` default |
| `webhook/admission/pclq/validation/handler.go` `decodeReplicas` (Kueue scale validation, #707) | decodes `grovecorev1alpha1.PodClique` | decodes `grovecorev1beta1.PodClique`, in commit 2 (§6.3) |
| `api/common/constants/constants.go` | doc comments | comments, plus the two new reasons `ConditionReasonAwaitingInitialSchedule` and `ConditionReasonAwaitingInitialAvailability` (§7.6); existing values unchanged |
| `podclique/reconcilestatus.go` and `podcliquescalinggroup/reconcilestatus.go` `computeMinAvailableBreachedCondition` | `Unknown` only during updates | awaiting states first (§7.6) |
| `podcliquescalinggroup/reconcilestatus.go` `computeNotInBreachReplicas` | member breached if `MinAvailableBreached=True` | also if `Unknown` with `AwaitingInitialSchedule` (§7.6) |
| `utils/component/podclique.go` `WasPCLQEverScheduled`, `WasPCSGEverHealthy`, `InitialScheduleGrace`, and their uses in `GetMinAvailableBreachedPCLQInfo` and `gangterminate.go` | gate gang termination | deleted from the gang-termination paths: an awaiting resource is never `True` (§7.6). `WasPCSGEverHealthy` survives only as the PCSG upgrade carry-over and is deleted in PR 2. |
| `api/core/v1beta1` `TopologyConstraint.RequiredDomain`, `HasAnyPackDomain` | fall back to `PackDomain` | read `pack` only |
| `webhook/admission/pcs/validation/topologyconstraints.go` (`packDomain` warning, domain check, migration and immutability rules) | `PackDomain` | the `packDomain` branches are deleted; immutability compares `pack` only |
| `utils/component/topologyconstraint.go`, `podgang/syncflow.go` | via `RequiredDomain()` | unchanged call sites; the helper now reads `pack` only |
| `utils/component/podgangmap.go` `StartupDependencyCliqueNames` | returns `StartsAfter` names, or the preceding clique under InOrder | `StartupDependencies`, which also returns a condition type per dependency (§7.1) |
| `utils/component/podgangmap.go` `StartupDependencyTargetsInEntry` and `StartupDependencyTarget` | target = PCLQ FQN + `MinReady` | the target also carries `ConditionType`; `MinReady` comes from `InitMinReplicas` (row above) |
| `podclique/components/pod/initcontainer.go` `generateArgsForInitContainer` | `--podcliques=<fqn>:<count>` | `--podcliques=<fqn>:<count>[:<conditionType>]`. The third part is omitted when the condition is `Ready`, so pods of converted workloads get the same init-container args as on N-1. |
| `initc/cmd/opts/options.go` `GetPodCliqueDependencies` | parses `name:count` into `map[string]int` | parses two or three parts into a map from FQN to `{count, conditionType}`. A missing third part means `Ready`. |
| `initc/internal/wait.go` `refreshReadyPodsOfPodClique` | counts pods with `PodReady` = `True`, keyed by pod name, including terminating pods | counts pods whose condition of that parent's type is `True`, keyed by UID, skipping pods with a `deletionTimestamp`, so that it fails closed (#859); log lines name the condition |
| `webhook/admission/pcs/validation/podcliqueset.go` (`startsAfter` DAG, self-reference, uniqueness, immutability) | `[]string` | iterate `startsAfter[].name`; add the `conditionType` checks (§4.5) |
| `podcliqueset/components/podclique` (PCLQ spec build) | copies `StartsAfter` | unchanged; it copies `[]StartupDependency` |
| `test/utils/pclq.go`, `pclqTemplate.go`, `pcs.go` `WithStartsAfter` / `WithPodCliqueParameters` | `[]string` | take dependency names and default to `Ready`; add a `WithStartupDependencies` variant |

For converted objects, per-component `breachTolerance` is a no-op, because every component receives the same `terminationDelay`. With distinct values, a PCS replica is gang-terminated when any child has been breached for longer than that child's tolerance. The requeue uses the minimum remaining wait across children, which `GetMinAvailableBreachedPCLQInfo` already computes; it gains a per-PCLQ tolerance input.

### 7.4 Storage switch and migration (GREP step 8, release N)

- **Storage switch:** a leader-only runnable, `internal/storagemigrator`, modelled on `internal/podgangmigrator`. It starts only after `/convert` is ready and `caBundle` is injected (§5.5). It patches PodCliqueSet, PodClique and PodCliqueScalingGroup to `storage: true` on v1beta1. PodGangMap and ClusterTopologyBinding use `conversion.strategy: None` (§5.1) and switch in the same step. The runnable watches the CRDs and re-applies the switch if something reverts it within N (§3).
- **Migration:** after the switch, for each kind, it lists all objects and issues an unchanged `Update`. The API server re-encodes each object at the storage version, because the bytes differ.
- It retries on conflict and runs with bounded concurrency.
- When it finishes, it patches `status.storedVersions` to `["v1beta1"]`. This needs RBAC on `customresourcedefinitions` (`patch`, for the switch) and on `customresourcedefinitions/status`, both restricted by `resourceNames`. They join the unconditional rule from §6.2.
- It is idempotent, and it reports progress in a condition on the operator's leader-election lease or in a metric. The migration step can be disabled by operator config for clusters that run kube-storage-version-migrator. The switch step cannot.

### 7.5 Downgrade procedure (N → N-1)

After N is up, a plain downgrade fails for two reasons. The API server refuses N-1's v1alpha1-only CRDs while `storedVersions` contains v1beta1. And N-1 does not serve `/convert`. N ships a reverse path, run while N is still installed:

1. `install-crds downgrade-storage` (a new subcommand of `cmd/install-crds`) disables the storage switch through operator config, patches the CRDs back to `storage: true` on v1alpha1, and rewrites every object.
   - The rewrite uses the same migrator code, so objects are stored at v1alpha1.
   - Any v1beta1-only values are kept in the conversion annotation (§5.3). N-1 ignores it and reads plain `minAvailable`, `terminationDelay` and `startsAfter` names, as in the GREP's original N rollback.
2. It sets `storedVersions` to `["v1alpha1"]`.
3. The user then downgrades the chart and CRDs to N-1.

The procedure is idempotent and safe to re-run. It is documented in the release notes and covered by e2e (§9). Values that v1alpha1 cannot express are lost to N-1. Examples are `init.minReplicas` ≠ `availability.minReplicas`, per-component `breachTolerance`, and custom `startsAfter` conditions. Upgrading back to N restores them from the annotation.

### 7.6 Start of breach evaluation (GREP clarification 15)

`MinAvailableBreached` stays `Unknown` until breach evaluation starts, so a resource that has never been scheduled or available is never `True`. This replaces today's gates in `utils/component/podclique.go`:
- `WasPCLQEverScheduled` reads `status.lastScheduled`;
- `WasPCSGEverHealthy` reads the PCSG condition with a 5-second grace (`InitialScheduleGrace`), and counts an `Unknown` to `True` transition as healthy.

The awaiting states apply only when `availability` is set; otherwise the condition is permanently `False`. The PCS has no `MinAvailableBreached` in Phase 1, so its `AwaitingInitialAvailability` arrives with PCS availability in Phase 2.

**PodClique** (`computeMinAvailableBreachedCondition` in `podclique/reconcilestatus.go`):

- The new first branch, ahead of the update and scale-to-zero branches, returns `Unknown` with reason `AwaitingInitialSchedule` while `status.lastScheduled` is nil. `lastScheduled` is already the durable marker of the first schedule and is never reset, so this state needs no upgrade carry-over. A PCLQ that N-1 has seen scheduled reads correctly on N's first reconcile.
- The existing branches follow unchanged. The scheduled-below-minimum branch now fires only on a regression.
- **Timing change.** `meta.SetStatusCondition` stamps `lastTransitionTime` on the `Unknown` to `True` transition, so a clique that is scheduled but never ready is `True` from its first schedule, and is gang-terminated `breachTolerance` after that. Today it is `True` from about creation, and is gang-terminated once scheduled if the delay has already passed. A clique that is already `True` at upgrade keeps its `lastTransitionTime` and its timing.
- **Restarts.** In Phase 1, gang termination and the PCSG replica recycle delete and recreate PCLQ objects, so a recreated clique starts awaiting. Phase 2's resource-scope restart must also clear `lastScheduled`; this goes into the Phase 2 plan.

**PodCliqueScalingGroup** (`computeMinAvailableBreachedCondition` in `podcliquescalinggroup/reconcilestatus.go`):

- While the PCSG has not left the awaiting state, it reports `Unknown` with reason `AwaitingInitialAvailability`. That holds during an update, when it is scaled to zero, and while its not-in-breach replicas are below `availability.minReplicas`. The first evaluation at or above the minimum, outside an update, writes `False` and leaves the state. After that, today's branches apply.
- **Has it left the awaiting state?** It is read from the previous condition, with no new status field. `False` or `Unknown` with `UpdateInProgress` means it has left, and `Unknown` with an awaiting reason, or no condition at all, means it has not. `True` means it has left only if `WasPCSGEverHealthy` passes.
- **Why that rule is safe.**
  - N never writes `True` or `UpdateInProgress` before the first availability, so for conditions written by N the rule is exact. The one exception is a PCSG that becomes available and then breaches within 5 seconds of its creation. Today's gate also treats that PCSG as never healthy.
  - For conditions written by N-1, it is today's gate. So on upgrade, no PCSG is gang-terminated that N-1 would have spared, and none is spared that N-1 would have terminated.
  - Upgrades must pass through N (§3), so every PCSG has been reconciled by N before N+1 runs, and PR 2 deletes `WasPCSGEverHealthy` and `InitialScheduleGrace`.
- **`computeNotInBreachReplicas`.** A replica is in breach if any member PCLQ is `True` or still `AwaitingInitialSchedule`. A never-scheduled member is `True` today, so the PCSG counts are unchanged. Without this rule, an awaiting member would look healthy, and the PCSG would leave its own awaiting state before its members were placed.

**Consumers:**

- `GetMinAvailableBreachedPCLQInfo` drops its `WasPCLQEverScheduled` filter, and `gangterminate.go` drops its `WasPCSGEverHealthy` filter. An awaiting resource is never `True`, so both filters are redundant.
- The PCSG replica recycle in `podcliquescalinggroup/components/podclique/sync.go` already uses `GetMinAvailableBreachedPCLQInfo`, so it is covered.
- The `MinBreached` printer column on `pclq` and `pcsg` now shows `Unknown` during startup instead of `True`. The release notes say so (§8 commit 8), because scripts that wait on `True` during startup see a change.
- **Lost status.** If an object's status is lost, for example when it is restored from a backup without status, it starts awaiting again. A PCLQ leaves the state again as soon as its pods are seen scheduled. A PCSG leaves only at its next availability, which is also how `WasPCSGEverHealthy` behaves today.

### 7.7 Mutable minimum fields

At v1beta1, the three minimum fields can change (§4.5). Each has its own effect. None of them enters `ComputePCLQPodTemplateHash` or `computeGenerationHash`, so finding 2 still holds.

**`init.minReplicas` recreates the PCS replica.** The value fixes the gang shape, and nothing built from it can change in place:
- PodGang `minAvailable` (`podgang/syncflow.go`);
- the PodGangMap base-gang layout (`podgangmap/steadystate.go`);
- the Kueue PodGroup counts, because Kueue Workload podSets are immutable;
- the `--podcliques=<fqn>:<count>` args of dependents' init containers (`StartupDependencyTargetsInEntry`, finding 12).

The mechanism:
- **Detecting stale replicas.** The *gang-shape hash* covers every clique template's and every PCSG config's `init.minReplicas`, keyed by name. The PCS controller labels each replica's PCLQs and PCSGs with it (`grove.io/gang-shape-hash`). A replica is stale when any of its children carries a different value.
- **Adopting unlabelled children.** A child without the label is adopted: the controller writes the current value and does not recreate the replica. So the upgrade, and objects created by N-1, recreate nothing.
- **Order.** Replicas are recreated one at a time. The next one is chosen in this order:
  1. the replica a rolling update had already started, if it is stale;
  2. stale replicas that are not available, lowest index first;
  3. the other stale replicas, lowest index first.

  "Available" is the check behind PCS `availableReplicas`:
  - every expected standalone PCLQ and PCSG exists and none is terminating;
  - each standalone PCLQ has `readyReplicas ≥ ReadyThreshold`;
  - each PCSG has `availableReplicas ≥ ReadyThreshold`;
  - a child at `replicas: 0` counts as available.

  Recreating unavailable replicas first restores the most capacity per disruption.
- **Pacing.** A replica is deleted only when every replica that is not stale is available. This includes the replica recreated last, which is no longer stale once it is rebuilt. Stale replicas do not hold the recreate back, and a replica being gang-terminated counts as not available.
  - This applies under every strategy, including `OnDelete`, because the gang shape is not a pod-template change.
  - It reuses gang termination's replica delete task (`createPCSReplicaDeleteTask` in `podcliquesetreplica/gangterminate.go`), with the event reason `GangShapeChanged`.
  - The normal sync rebuilds the replica from the current template: its PCLQs, PCSGs, PodGangs and PodGangMap entries.
  - The order and the wait are worked out again on every reconcile from the gang-shape labels, so an operator restart resumes where it stopped.
- **Status.** The recreate keeps `status.gangShapeRecreate` and the `GangShapeRecreateInProgress` condition current (§4.2). It sets them when the first stale replica is found and clears them once no replica is stale and `currentlyRecreating` is available.
  - It does not use `status.updateProgress`. That field drives the pod-template rollout in the PCS, PCSG and PCLQ controllers, and the generation-hash counters in it don't move during a recreate.
  - So the Coherent freeze of `rollingUpdate.minReplicas` (§4.5) still keys on `updateProgress` alone. The recreate does not read the unit.
  - An `init.minReplicas` edit during a recreate changes the target hash. Status takes the new `gangShapeHash`, and replicas already recreated to the old target become stale again.
- **Gang termination.** It is suspended only for `currentlyRecreating`, from the delete until that replica is available again, so the replica is never deleted twice. Other replicas still gang-terminate. Otherwise a replica that is not stale, cannot recover, and has gang termination suspended would block the wait forever. A stale replica that gang termination deletes is rebuilt at the new shape, so it counts as recreated.
- **Interaction with updates.** A recreated replica comes up at the newest pod templates, so it counts as updated. The recreate takes precedence over an update in flight:
  - the rolling update starts no new replica while any replica is stale or `currentlyRecreating` is set;
  - a replica the update had already started is recreated first (see **Order**).
- **No deadline.** A recreated replica that never becomes available stalls the recreate. The condition shows it with reason `WaitingForAvailableReplicas`. Phase 1 adds no deadline.
- **Disruptive by design.** Each recreate takes the whole replica down, base PodGang included. The field doc and the user guide say so.

**`availability.minReplicas` changes in place.** Pods are not restarted, and no hash changes.
- **Propagation.** It already reaches the children:
  - `buildResource` and `setPodCliqueSpec` (`podcliqueset/components/podclique/podclique.go`) deep-copy the template spec into each PCLQ;
  - the PCSG build writes the PCSG's `termination` on every sync (`podcliquescalinggroup.go`).
- **Gap: Coherent updates.** During a Coherent update, replicas not under update keep their whole old spec (`preserveRevision`). Overlay the template's `termination.resource.availability` onto that preserved spec, so every replica gets the new floor at once. The PCSG member build in `podcliquescalinggroup/components/podclique/podclique.go` needs the same overlay.
- **Breach semantics.** The PCLQ and PCSG status reconcilers evaluate `MinAvailableBreached` against the new floor on their next pass.
  - Raising the floor above what is available sets the condition `True` at that moment, and gang termination follows `breachTolerance` later. This is allowed, and documented.
  - Lowering it can clear a breach.
  - The awaiting states (§7.6) are unaffected.
  - `ReadyThreshold`, and with it `AvailableReplicas`, follows the new value.
- **Scale guard.** It reads the value stored on the child, so until the child is synced it applies the old floor. Fix the comments in `scaleguard.go` that call the field immutable.
- **Defaults don't follow.** A defaulted `rollingUpdate.minReplicas` keeps its value: defaulting only fills unset fields.

**`rollingUpdate.minReplicas` applies from the next Coherent update.**
- **No hashing.** The value is not hashed, so changing it never rolls pods.
- **Where it is read.** The controllers read it from the live PCS template (finding 9). The admission freeze during a Coherent update (§4.5) therefore guarantees that an update runs with a single unit throughout. Because the unit must stay `≤ min(init.minReplicas, replicas)`, the freeze also stops `init.minReplicas` and the template's `replicas` from going below it until the update ends.
- **Same-request edits.** A request that changes a pod template and the unit together is accepted, and that update uses the new unit. The same holds for a unit change admitted after a template edit but before the controller records `updateProgress`, because no replica has been rolled yet.

## 8. PR sequence

All Phase 1 work for release N lands as **one PR**. Release N is cut right after it merges. Numbers in brackets are the GREP's Phase 1 steps.

| PR | Content | Behaviour change |
|---|---|---|
| **0** | Merge GREP-0877 ([#878](https://github.com/ai-dynamo/grove/pull/878)). Revision `1560cbc` already includes all the §11.1 feedback. | None |
| **1** [1–8, 10] | The complete v1beta1 promotion, Phase 1 field split, `startsAfter` change, storage switch, tests and docs, as the ordered commits below. **Cut release N** after it merges. | v1alpha1: none. v1beta1-native: new semantics. Storage moves to v1beta1. |
| **2** | Release N+1: move `+kubebuilder:storageversion` to v1beta1 in the source, which makes the switch runnable a no-op. Delete `WasPCSGEverHealthy` and `InitialScheduleGrace`, the PCSG upgrade carry-over (§7.6). | None |
| **3** [9] | Release N+3, after the two-release window (N+1, N+2): stop serving v1alpha1 and remove it from the CRDs through the GREP-436 upgrader. Delete the v1alpha1 package, the conversion code, the v1alpha1 CEL rules, the storage switch and `downgrade-storage`. | Breaking for v1alpha1 clients |

PR 1 is organized as eight commits, in this order. **Each commit builds and passes the unit tests on its own**, so the PR can be reviewed and bisected commit by commit.

| Commit | Content | Behaviour change at that commit |
|---|---|---|
| **1** [1] | Accessor layer (§7.1) on v1alpha1, with unit tests that pin today's values. It includes `StartupDependencies`, plus `initc` support for the optional `:<conditionType>` argument (it parses but never emits it yet). | None |
| **2** [2] | v1beta1 package as an identical copy of v1alpha1. Trivial generated conversion, registered as hub-and-spoke converters in the operator (§5.1). Multi-version CRDs (storage v1alpha1). `/convert`. The three converted CRDs get a fail-closed `Webhook` conversion at the default address, which the `crdInstaller` and the operator correct (§6.1, option B). `caBundle` injection and RBAC (§6.2). **Webhook rules switched to v1beta1 with `Equivalent`**, and `internal/webhook/...` handlers registered for v1beta1 types through the v1alpha1 bridge (§6.3). | None. The schemas are identical. |
| **2b** | Serve the webhooks before the controllers are added, and move the PodGang migration gate from before `mgr.Start` to that point, with retries (§5.5). Without it, commit 3 would have the operator read PodCliqueSets through a `/convert` it does not serve yet. | None |
| **3** [3] | Mechanical import switch (§7.2). The v1beta1 clients were already generated in commit 2. Removes the v1alpha1 bridge of the admission handlers (§6.3). | None |
| **4** [4, 5, 6] | v1beta1 schema (§4.2–4.3), including dropping `packDomain` and the `[]StartupDependency` form of `startsAfter`. Hand-written conversion with the stash (§5.2–5.3), plus fuzz and table tests. v1alpha1 CEL rules. Deprecation markers. Accessors on v1beta1 fields. Defaulting and validation, including the mutability rules and the strategy-switch rule (§4.4–4.5). Mutable minimum fields: the gang-shape hash and replica recreate, with `status.gangShapeRecreate` and its condition, the availability overlay on preserved specs (§7.7). The `autoScalingConfig.minReplicas` default and floor (§4.4–4.5). Scale guard. Per-component `breachTolerance`. Percentage `maxUnavailable` in update orchestration. `startsAfter` validation, and the per-dependency condition type passed to and enforced by `initc`. `MinAvailableBreached` awaiting states, with the PCSG upgrade carry-over (§7.6). Printer columns. v1beta1 status doc fixes (§11 item 17). | v1alpha1: none, except that `MinAvailableBreached` is `Unknown` during startup and a PCLQ's breach tolerance runs from its first schedule (§7.6). v1beta1-native: new semantics. |
| **5** [8] | Storage switch and migrator (§7.4), the sticky-storage handling in the `crdInstaller` (§3), and the downgrade procedure (§7.5). | Storage only |
| **6** [10] | Envtest and e2e from §9: upgrade from N-1 under live traffic, downgrade to N-1, operator restarts, the equivalence suites with v1alpha1 and v1beta1 manifests, Story 1 and the #859 scenario. | — |
| **7** [7] | Docs and samples:<br>- `docs/api-reference` (add v1beta1 to `hack/generate-api-docs.sh`);<br>- user guide and samples on v1beta1, keeping one v1alpha1 sample. This includes the startup-ordering samples (`simple2`/`simple3-explicit-startup-order.yaml`) and the e2e workloads that use `startsAfter`;<br>- a migration guide with the §5.2 table. | — |
| **8** | Release notes covering the CRD-before-chart upgrade order (§6.1), the downgrade procedure (§7.5), the v1alpha1 removal in N+3, upgrades into N+1 and later only from N (§3), `MinAvailableBreached=Unknown` during startup and the breach-timing change (§7.6), and Go consumers such as Dynamo. | — |

The schema change, the conversion and the call-site migration are a single commit (4). Removing `minAvailable` breaks every call site that commit 1 has not already routed through an accessor, so they cannot build separately. Commit 1 is what keeps commit 4 reviewable. The mechanical import switch stays isolated in commit 3, so reviewers can check it as a pure rename.

What one PR changes compared with a PR series:

- **Gained:** release N has a single merge point. No intermediate state of `main` serves a half-finished v1beta1, so no release-blocking issue or feature branch is needed to keep the schema final. The GREP-436 CRD files, the chart and the operator always change together.
- **Cost:** a very large PR. The ~267-file import switch alone dominates the diff. It needs reviewers to work commit by commit, and the PR description should link each commit to its section here. CI runs the full e2e and the compatibility matrix (§9) on the PR, not on each commit.

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
  - a v1alpha1 PCLQ or PCSG object without the annotation leaves `breachTolerance` unset, and the accessor resolves it to the owning PCS's `terminationDelay`, including the nil → `4h` case;
  - `startsAfter: [a, b]` → `[{a, Ready}, {b, Ready}]` and back, preserving order, on PCS templates and PCLQ objects. A non-`Ready` condition survives the v1alpha1 round trip through the stash. A v1alpha1 update that drops a name also drops its stashed condition.
- **Startup dependencies:**
  - `StartupDependencies` returns `PodReady` for every v1alpha1 dependency and for the InOrder implicit dependency;
  - `generateArgsForInitContainer` omits the third argument part for `Ready`, so a converted workload's init-container args are byte-identical to today's;
  - `initc` option parsing accepts two- and three-part values and rejects bad ones (empty condition, extra parts);
  - `initc` waits on the given condition type and does not count `Ready` pods as satisfying a custom condition;
  - `initc` fails closed (#859): a terminating target, or a replacement pod with the same name and a new UID whose condition is not yet `True`, does not count, and an incomplete target set never releases.
- **Schemas:** PodGangMap and ClusterTopologyBinding schemas are identical across versions.
- **Defaulting and validation:** every §4.4–4.5 rule, including:
  - rejection of adding or removing `availability`;
  - the strategy-switch rule when `updateStrategy.type` changes;
  - mutability (§4.5):
    - each of the three fields is accepted at v1beta1;
    - each is rejected when `RequestKind` is v1alpha1;
    - a v1alpha1 strategy switch that drops `rollingUpdate.minReplicas` is still accepted;
    - a `rollingUpdate.minReplicas` change, or a switch away from `Coherent`, is rejected while a Coherent update is in progress and accepted once it has ended;
    - changes that break the ordering or autoscaling rules are rejected, including lowering `init.minReplicas` below the unit and raising `availability.minReplicas` above a set `autoScalingConfig.minReplicas`, while the same raise is accepted when `autoScalingConfig` is nil;
  - `rollingUpdate.minReplicas` above `init.minReplicas` or above `replicas` is rejected;
  - `autoScalingConfig.minReplicas`:
    - with `availability` set, it defaults to `availability.minReplicas`, and a lower value is rejected;
    - with `availability` nil, it defaults to `1`, and `1` is accepted;
    - a v1alpha1 request still defaults it to `replicas`;
  - the scale guard with `availability` nil accepts any `replicas ≥ 0`;
  - `startsAfter`: duplicate names, an invalid `conditionType` (including one containing `:`), changing any `conditionType` on update;
  - `startsAfter` condition types:
    - each built-in condition is accepted without a gate, and `DisruptionTarget` is rejected;
    - a custom condition is rejected when the parent doesn't declare it in `readinessGates` and accepted when it does, for standalone and PCSG-member parents;
    - an update that removes a referenced readiness gate from the parent's pod template is rejected, while removing an unreferenced gate is accepted;
  - a v1alpha1 update that moves `packDomain` to `pack.required` is accepted as an identity change.
- **Mutable minimum fields** (§7.7):
  - the gang-shape hash changes with any `init.minReplicas` and with nothing else;
  - an unlabelled child is adopted, not recreated;
  - a stale replica is deleted through the replica delete task, one replica at a time, under each strategy;
  - the recreate order: the replica the rolling update had started, then stale replicas that are not available, then the other stale replicas, each by lowest index;
  - no replica is deleted while a non-stale replica, including the one recreated last, is not available, and stale replicas don't hold the recreate back;
  - `status.gangShapeRecreate` and the `GangShapeRecreateInProgress` condition are set at the first stale replica, track `currentlyRecreating` and the remaining order, and are cleared at the end; `status.updateProgress` is untouched;
  - gang termination skips `currentlyRecreating` and still fires for other replicas;
  - an `init.minReplicas` edit mid-recreate updates `gangShapeHash` and marks the replicas already recreated as stale again;
  - `gangShapeRecreate` round-trips through v1alpha1;
  - the rolling update starts no replica while one is stale;
  - a changed `availability` reaches PCLQs whose spec is preserved during a Coherent update, and PCSG members;
  - neither pod-template hash nor generation hash changes with `availability.minReplicas` or `rollingUpdate.minReplicas`.
- **Ready threshold:** `ReadyThreshold` is `availability.minReplicas` when set, and `init.minReplicas` when `availability` is nil.
- **Awaiting states** (§7.6):
  - PCLQ: with `lastScheduled` nil, the condition is `Unknown`/`AwaitingInitialSchedule`, even during an update or at `replicas: 0`. Once scheduled, today's `True`/`False` results follow, and the first `True` has its `lastTransitionTime` at the schedule. An existing `True` keeps its `lastTransitionTime`.
  - PCSG transitions: none or awaiting stays awaiting during an update, at `replicas: 0` and below the minimum. Awaiting goes to `False` at the minimum. After that, today's logic applies.
  - PCSG carry-over, one case per N-1 condition shape: `False`; `True` with `lastTransitionTime` before and after the 5-second grace; `Unknown`/`UpdateInProgress`.
  - `computeNotInBreachReplicas` counts a replica with an awaiting member as in breach.
  - `GetMinAvailableBreachedPCLQInfo` and `gangterminate.go` never select an awaiting PCLQ or PCSG.
  - With `availability` nil, the condition is `False` in all of these cases.
- **Accessor parity:** at commit 1, the accessors return today's values; at commit 4, they return the same values for converted objects.
- **Phase 2 safety:** neither conversion nor the stash ever produces a `restart` field (§5.4).
- **Chart:**
  - no `grove.io` webhook rule uses `Exact`, and every rule includes v1beta1;
  - the operator's ClusterRole allows `update` and `patch` on the three converted CRDs with and without the `crdInstaller`.
- **CRD conversion:**
  - the generated PodCliqueSet, PodClique and PodCliqueScalingGroup CRDs have the default `Webhook` conversion, and PodGangMap and ClusterTopologyBinding have none;
  - the `crdInstaller` rewrites the service namespace and applies no `caBundle`;
  - the operator's startup correction sets the namespace and port, keeps an injected `caBundle`, and fails on a missing CRD or one without v1beta1;
  - the keeper restores a reset address, and in `manual` mode injects and rotates `ca.crt`.
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
    - a PCLQ breach still gang-terminates after the original delay;
    - a PCSG that was never healthy at the upgrade is not gang-terminated, and a PCSG that was healthy and then breached is gang-terminated after the delay (§7.6);
    - the storage switch happens only after `/convert` is ready, the migrator completes, and `storedVersions == ["v1beta1"]`;
    - after N's CRDs are applied over N-1's, with and without the `crdInstaller`, conversion points at the release namespace and N-1 keeps working until it is replaced (§6.1).
- **Upgrade under load:** during the N-1 → N upgrade, a client keeps writing v1alpha1 PodCliqueSets while the N-1 operator keeps reconciling. No write fails apart from ordinary conflicts. That holds because storage stays v1alpha1 until N serves `/convert` (§3).
- **Sticky storage:** re-running the `crdInstaller` init container and running `helm upgrade` with changed values both leave storage on v1beta1. A manual `kubectl apply` of N's CRDs is switched back by the runnable, and the migrator re-runs. The keeper restores the conversion address the apply reset.
- **Downgrade:** `install-crds downgrade-storage` then N → N-1 keeps workloads running and readable at v1alpha1. Upgrading back to N restores v1beta1-only values from the annotation.
- **Rollbacks:** N+1 → N keeps workloads running and readable.
- **Equivalence:** `gang_termination_test.go`, `scaleguard_test.go`, `gang_scheduling_test.go` and the `update/` suite pass with v1alpha1 manifests and again with equivalent v1beta1 manifests.
- **New v1beta1 behaviour:**
  - Story 1: `init.minReplicas: 8` with `availability.minReplicas: 6` tolerates the loss of 2 pods without gang termination.
  - A PodCliqueSet that cannot be scheduled shows `MinBreached` `Unknown` on its PCLQs and PCSGs and is never gang-terminated. A clique that is scheduled but never becomes ready is gang-terminated `breachTolerance` after its first schedule (§7.6).
  - A PCSG whose members have no `availability` counts a replica as available only once each member has `init.minReplicas` ready pods.
  - A Coherent update with `rollingUpdate.minReplicas` larger than `availability.minReplicas` rolls in the larger unit.
  - A percentage `maxUnavailable` is resolved correctly before and after scaling, and also after a scale-out or scale-in in the middle of an update, where the next reconcile uses the new budget (§11 item 8).
  - `rollingUpdate.minReplicas` is rejected under `RollingRecreate` and `OnDelete`, filled in when switching to `Coherent`, and must be removed when switching away.
  - Raising `init.minReplicas` recreates the PCS replicas one at a time, with the next starting only once every non-stale replica is available. Afterwards the PodGangs and the init-container args carry the new count. This is tested under `RollingRecreate`, `Coherent` and `OnDelete`.
  - With one stale replica made unavailable, that replica is recreated first. `status.gangShapeRecreate` and the condition are visible during the recreate and cleared afterwards. A breach on a replica not being recreated still gang-terminates it.
  - Raising `availability.minReplicas` above the ready pods gang-terminates after `breachTolerance`. Lowering it clears the breach. No pod is restarted in either case.
  - Changing `rollingUpdate.minReplicas` rolls no pods. The next pod-template change rolls in the new unit.
  - The upgrade test also asserts that no replica is recreated for a missing gang-shape label.
  - The #859 pre-Ready scenario: the leader clique declares a readiness gate `example.com/placeholder-available` and cannot become `Ready` until the dependent clique runs. The dependent clique has `startsAfter: [{name: leader, conditionType: example.com/placeholder-available}]`. Once the test sets the condition on the required leader pods, the dependent leaves `Init` while the leaders are still not `Ready`, so there is no deadlock. Deleting a leader pod before the condition is set keeps the dependent blocked. This extends `startup_ordering_test.go`.
  - The existing `startup_ordering_test.go` passes unchanged with v1alpha1 manifests, as part of the equivalence runs.
- **Certificates:** `caBundle` injection in `auto` and `manual` modes, and `manual`-mode rotation.
- **Operator restarts** in N, both between the storage switch and the end of migration and during migration, recover. `/convert` comes up before the caches, and the migrator resumes.
- **Kubernetes versions:** the compatibility matrix ([#820](https://github.com/ai-dynamo/grove/pull/820)) runs the standard suites on 1.34 and 1.35, but upgrade tests run only on their dedicated target. Add the conversion envtest and the v1alpha1/v1beta1 equivalence runs to the matrix. Run the N-1 → N upgrade test on both versions at least for release N, because conversion-webhook and `matchPolicy: Equivalent` behaviour is API-server-side.
- **Kueue backend:**
  - with v1alpha1 and with v1beta1 manifests, the Kueue partial-gang rejection and the base PodGang PodGroup count use `init.minReplicas`;
  - Kueue scale validation through the PCLQ webhook works at both versions.

## 10. Risks and mitigations

| Risk | Mitigation |
|---|---|
| **The operator is down, so conversion is down.** v1alpha1 reads and writes fail. In N, between the storage switch and the end of migration, reads of unmigrated objects also fail at v1beta1. | Run 2 or more operator replicas with a PodDisruptionBudget; webhooks run on all replicas. Add `/convert` readiness. The migrator shortens the window. |
| **Silent field pruning under `strategy: None`** (finding 4) | The generated CRDs carry a fail-closed `Webhook` strategy (§6.1), so a wrong address fails requests instead of pruning. A test fails if a converted CRD lacks it. |
| **Conversion fails until the address and `caBundle` are set** (option B, §6.1): on a fresh install, or after a manual re-apply of the CRDs | Before N's operator runs, nothing needs conversion: storage is v1alpha1 and clients use v1alpha1 (§3). The operator corrects the address before its manager starts, and the keeper heals a re-apply within 30 seconds. |
| **The N operator starts against N-1's CRDs** because the user ran `helm upgrade` without applying N's CRDs | The operator exits at startup when a CRD does not serve v1beta1 (§6.1). The release notes give the order. |
| **Requests reach N-1 pods during the rolling update.** The Service selects N-1 and N pods while the Deployment rolls. N-1 pods do not serve `/convert`. They also receive v1beta1 admission requests once Helm has switched the rules (§6.3). From commit 4, the N-1 defaulting handler would decode a v1beta1 object into v1alpha1 types and patch away the fields it doesn't know. | `/convert` on an N-1 pod returns 404, so conversion fails closed and clients retry. The v1beta1 admission handlers are served on `/webhooks/v1beta1/` paths (§6.3), so an N-1 pod also returns 404 instead of mis-decoding. Both fail closed, and the window lasts as long as the rollout. |
| **Validation and authorizer bypass at v1beta1** (finding 3) | Commit 2 switches every rule to v1beta1 with `Equivalent`, backed by the chart test. |
| **Lossy v1beta1 → v1alpha1 for older clients** such as Dynamo | Round-trip annotation (§5.3), fuzz tests, and release notes asking consumers to move to v1beta1. |
| **A v1alpha1 client switches `updateStrategy` while a stash holds `rollingUpdate.minReplicas`** | The strategy-aware overlay rule (§5.3) and a fuzz case for it. |
| **Percentage `maxUnavailable` changes with scaling mid-update** | This is intended, and matches Deployments. Every reconcile resolves the value again against the live component's `spec.replicas`, so a scale-out raises the budget and a scale-in lowers it. Under `Coherent`, `max(resolved, minReplicas)` keeps the budget at least the minimum update unit, and runtime planning caps disruption at the replicas that remain (#884). A unit test and the e2e scaling case in §9 cover it. |
| **A plain rollback from N to N-1 fails after the storage switch** | The downgrade procedure (§7.5), tested in e2e and documented in the release notes. N+1 → N and later rollbacks are plain. |
| **Writes fail during the N-1 → N upgrade** because storage moved before anything served `/convert` | Staged switch (§3). N's CRDs keep v1alpha1 storage, and the N operator switches only once `/convert` and `caBundle` are ready. An e2e test runs the upgrade under write load. |
| **Something reverts the storage flag within N** (manual `kubectl apply`) | The flag is sticky in the `crdInstaller`, and Helm never upgrades `crds/`. The runnable re-applies the switch, and the migrator re-runs. |
| **v1alpha1 consumers still writing `packDomain` see `pack.required` on read** | The stash marker restores the `packDomain` form for v1alpha1 readers. Release notes point consumers to `pack.required`. |
| **Consumers miss the v1alpha1 removal in N+3** | The `deprecationWarning` names the removal release from N onward, and the release notes of N+1 and N+2 repeat it. |
| **Phase 1 accidentally breaks Phase 2's "unset restart means it predates Phase 2" rule** | §5.4, enforced by conversion-gen and a unit test. |
| **Import churn hides semantic changes** | Commit 3 is purely mechanical; semantics are isolated in commits 1, 4 and 5. |
| **v1alpha1 users see v1beta1 field paths in errors** | v1alpha1 CEL rules for the common errors (§4.5). |
| **A custom `startsAfter` condition is never set, so dependent pods stay in `Init` forever** | Admission rejects custom conditions that the parent doesn't declare in `readinessGates`, and rejects removing a referenced gate (§4.5). That catches typos and drift. A declared gate that nothing sets also keeps the parent from becoming `Ready`, so the problem shows up on the parent rather than only on the dependents. The `initc` logs the condition it is waiting on, and the user guide documents the pattern. |
| **Requiring a readiness gate changes what `Ready` means for the parent** | This is intended: the milestone is "below `Ready`" (#859). The user guide states that the workload's agent must set the condition on every start, not only on restore. |
| **Clients rely on `MinAvailableBreached=True` during startup** (scripts, dashboards, `kubectl wait`) | Release notes (§8 commit 8). Grove's own consumers change in the same commit (§7.6). The awaiting reasons name the state explicitly. |
| **An upgrade gang-terminates, or spares, a PCSG differently from N-1** | The carry-over reads N-1 conditions with today's `WasPCSGEverHealthy` (§7.6), covered by unit cases per condition shape and by the upgrade e2e. |
| **A user upgrades from N-1 directly to N+1 or later** | Not supported (§3), and the release notes say so. The carry-over and the storage migration exist only in N. |
| **An `init.minReplicas` edit takes replicas down** | Intended (§7.7). Replicas are recreated one at a time, each only once every non-stale replica is available, and unavailable replicas go first. Gang termination is suspended only for the replica being recreated. The field doc and the user guide say so. v1alpha1 clients cannot trigger it. |
| **A recreate stalls** because a recreated replica never becomes available | Visible as `GangShapeRecreateInProgress=True` with reason `WaitingForAvailableReplicas`, and in `status.gangShapeRecreate` (§7.7). There is no deadline in Phase 1. |
| **The upgrade recreates replicas** because existing children have no gang-shape label | An unlabelled child is adopted, not recreated (§7.7). Unit and upgrade e2e tests cover it. |
| **Raising `availability.minReplicas` gang-terminates a running replica** | Intended and documented: the new floor applies at once, and termination waits `breachTolerance`. |
| **A unit change mid-update splits a Coherent update** | Admission freezes `rollingUpdate.minReplicas` and the strategy while a Coherent update is in progress (§4.5). |
| **Init-container args change for existing workloads on upgrade** | The `Ready` condition is never written into the args, so pods created by N for converted workloads match N-1. Running pods keep their original `initc` image and args. |

## 11. Open questions and GREP feedback

Resolved by GREP revision `37c2bb8`, versus the first draft of this plan:

- the legacy default is `1`;
- under `RollingRecreate`, `maxUnavailable` keeps its default of `1`;
- `maxUnavailable` becomes int-or-percentage, rounding down with a minimum of 1;
- no `UpdateSpec`, and `updateStrategy` stays unchanged;
- no PCS `init`, and the PCS `termination` section arrives in Phase 2;
- PCSG members use `init.minReplicas` as the update threshold;
- the conversion annotation is named `grove.io/v1beta1-conversion-data`.

### 11.1 Decided for this plan, and folded into the GREP

All of this plan's feedback for [#878](https://github.com/ai-dynamo/grove/pull/878) is now in the GREP. Revision `eda4763` added items 1–7, the two fixes against `main` and clarification 15 (the awaiting states, §7.6). Revision `1560cbc` added the rest. Nothing is outstanding. The items are kept here for reference:

1. **Ready threshold when `availability` is nil is `init.minReplicas`.** The resource counts as available once its gang-scheduled minimum is ready (§7.1; GREP clarification 14).
2. **Adding or removing `availability` is rejected** (§4.5; GREP immutable-fields list).
3. **`breachTolerance` on converted PCLQ and PCSG objects comes from the PCS-level v1alpha1 `template.terminationDelay`**, resolved through the owning PCS because conversion is pure (§5.2; GREP migration table).
4. **`packDomain` is dropped from v1beta1** and converted to `pack.required` (§4.3, §5.2, §5.3; GREP API changes and migration table).
5. **v1alpha1 is served for two releases after the storage migration**: N+1 and N+2, with removal in N+3 (§3; GREP step 9).
6. **Introduction and storage switch are combined into one release, N**, with a staged switch, sticky storage and a downgrade procedure (§3, §7.5; GREP step 8).
7. **All release-N work lands as one PR** of ordered, separately buildable commits (§8; GREP Phase 1 introduction).
Added in `1560cbc`:

- **Upgrades into N+1 and later must pass through N** (§3; GREP step 9).
- **`maxUnavailable` is resolved on every reconcile, not frozen** (item 8; GREP "Interaction with existing features" and Test Plan).
- **PodGangMap and ClusterTopologyBinding use `conversion.strategy: None`** (item 10, §5.1; GREP step 2). GREP step 2 also named option A for CRD delivery; it needs updating for option B (item 9).
- **`rollingUpdate.maxUnavailable` and `rollingUpdate.progressDeadline` stay mutable** (§4.5; GREP mutable-fields list).

Two fixes against `main`, both added in `eda4763`:

- **`maxUnavailable` upper bound.** [#884](https://github.com/ai-dynamo/grove/pull/884) removed it. The rule is "integer `≥ 1` or percentage in `[1%, 100%]`", resolved against the live component's `spec.replicas` (§4.5).
- **Kueue backend** ([#707](https://github.com/ai-dynamo/grove/pull/707)). Its partial-gang rule and base PodGroup count become `init.minReplicas` (§7.3).

### 11.2 Resolved before PR 1

These were deferred. Release N is cut right after PR 1 (§8), so each needed a decision before the commit that uses it.

8. **`maxUnavailable` is not frozen.** Every reconcile resolves it again against the live component's `spec.replicas`, under `Coherent` as `max(resolved, minReplicas)`. So no new status field is needed, and the v1beta1 schema gains nothing for it. GREP revision `1560cbc` removed its "freezes the resolved value in its plan" wording (§11.1). §7.3 and §10 follow this. (Commit 4.)
9. **CRD delivery uses option B** (§6.1), replacing the earlier choice of option A. Helm cannot adopt CRDs that N-1 installed from `crds/` through a hook, since its ownership check runs first. The three CRDs stay in `crds/` with a fail-closed `Webhook` conversion at the default address. The `crdInstaller` and the operator correct the address, and cert-controller (`auto`) or the operator (`manual`) injects the `caBundle`. GREP step 2 needs the matching update. (Commit 2.)
10. **PodGangMap and ClusterTopologyBinding use `conversion.strategy: None`** (§5.1). Their schemas are identical, so they never call the webhook, and their storage switches in N without it (§7.4). (Commit 2.)
11. **The admission webhook paths are versioned in N** (§6.3, §10). The v1beta1 handlers are served on `/webhooks/v1beta1/...`, so an N-1 pod that receives a v1beta1 admission request during the rollout returns 404 and fails closed. Otherwise it would decode the object into v1alpha1 types, and from commit 4 the defaulting handler would patch away fields it doesn't know. This includes the authorizer, which now matches the v1beta1 kinds of PodClique, PodCliqueScalingGroup and PodGangMap. (Commit 2.)

18. **The minimum fields are mutable at v1beta1** (§4.5, §7.7), replacing the GREP's immutability rule:
    - `init.minReplicas` recreates PCS replicas one at a time, under every strategy;
    - `availability.minReplicas` applies in place, including on replicas held back by a Coherent update;
    - `rollingUpdate.minReplicas` is unhashed and applies from the next Coherent update. It, and a switch away from `Coherent`, are frozen while a Coherent update is in progress;
    - v1alpha1 requests cannot change any of them.

    The GREP's immutable-fields rule, its Test Plan and step 4 need the matching update. (Commit 4.)
19. **Scaling floors** (§4.4–4.5):
    - `rollingUpdate.minReplicas ≤ min(init.minReplicas, replicas)`;
    - `autoScalingConfig.minReplicas` has the floor and default `availability.minReplicas`, or `1` when `availability` is nil;
    - the scale guard uses the same floor, with `0` always allowed;
    - v1alpha1 requests keep today's `replicas` default.

    The GREP needs the `≤ init.minReplicas` bound, an `autoScalingConfig.minReplicas` row in its Defaults table, the autoscaling rule, and the nil floor for the scale guard. (Commit 4.)

### 11.3 `startsAfter` change (outside GREP-0877)

Tracked by [#859](https://github.com/ai-dynamo/grove/issues/859), not by #878.

Decided:

11. **Tracking.** The change is tracked by #859, which leaves the exact API to Grove. Post the `[]StartupDependency` shape (§4.2) and the validation rules (§4.5) there for agreement before commit 4 is written.
12. **Custom conditions are rejected unless declared as a readiness gate.** A `startsAfter` dependency that names a custom condition must have that condition declared in the parent clique's `spec.readinessGates`, or admission rejects it. Removing a referenced gate later is also rejected, and `DisruptionTarget` is never allowed (§4.5). Workloads that set a condition without a gate must add the gate. This could be relaxed later if needed, whereas tightening after release would have been a breaking change.

13. **"Target pods" keeps today's behaviour.** A dependency is released once the parent's `init.minReplicas` pods within the dependent pod's own PodGang have the condition `True`. That is today's count with `MinAvailable` renamed (finding 12). It is not "every pod of the parent". The field doc says "on the target pods of the parent" (§4.2).
14. **The Snapshot design declares `PlaceholderAvailable` as a readiness gate** on the placeholder pods (confirmed). So it works with item 12 unchanged.

### 11.4 Status-field cleanup (outside GREP-0877)

Decided:

15. **`PodCliqueSetStatus.podGangStatuses` is removed** (§4.3). The `grove.io` copies of `PodGangStatus` and `PodGangPhase` go with it. Nothing ever set or read them. The removal is a separate change on v1alpha1 that lands before PR 1, so the v1beta1 copy never has it. Its release note should mention Go consumers that refer to these types.

From an audit of PCS, PCSG and PCLQ status. Every other field is written, and most are read by the controllers or used by `/scale`, printer columns or e2e:

16. **`PodCliqueSetReplicaUpdateProgress.inFlightEpochs` stays unchanged** in v1beta1, both the field and its doc. It is written but not read today, because the coherent update recomputes the epochs from the PodGangMap.
17. **The inaccurate status docs are fixed in PR 1** (commit 4):
    - `PodCliqueSetStatus.availableReplicas` says availability is based on `MinAvailableBreached`. The code compares ready and available counts against `MinAvailable`, which becomes `ReadyThreshold` (§7.1).
    - `PodCliqueScalingGroupStatus.replicas` says "observed", but it mirrors `spec.replicas`.
