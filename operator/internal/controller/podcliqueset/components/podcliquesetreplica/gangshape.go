// Copyright 2026 The Grove Authors.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package podcliquesetreplica

import (
	"context"
	"fmt"
	"slices"
	"strconv"

	apicommon "github.com/ai-dynamo/grove/operator/api/common"
	apiconstants "github.com/ai-dynamo/grove/operator/api/common/constants"
	grovecorev1alpha1 "github.com/ai-dynamo/grove/operator/api/core/v1alpha1"
	"github.com/ai-dynamo/grove/operator/internal/constants"
	"github.com/ai-dynamo/grove/operator/internal/controller/common/component"
	groveerr "github.com/ai-dynamo/grove/operator/internal/errors"
	"github.com/ai-dynamo/grove/operator/internal/utils"
	componentutils "github.com/ai-dynamo/grove/operator/internal/utils/component"
	k8sutils "github.com/ai-dynamo/grove/operator/internal/utils/kubernetes"

	"github.com/go-logr/logr"
	"github.com/samber/lo"
	"k8s.io/apimachinery/pkg/api/equality"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// gangShapeWork is the gang-shape recreate work of a PodCliqueSet. A replica is stale when one of its PodCliques
// or PodCliqueScalingGroups has a spec.minAvailable other than the one in the PodCliqueSet template, which
// happens after a MinAvailable change in a clique template or a PodCliqueScalingGroup config.
type gangShapeWork struct {
	// staleReplicaIndices are the indices of the stale replicas, in the order in which they are recreated. The
	// replica that recreateTask recreates is not included.
	staleReplicaIndices []int
	// currentlyRecreating is the replica that was deleted last, until it has recovered from its recreate.
	currentlyRecreating *grovecorev1alpha1.PodCliqueSetReplicaRecreateProgress
	// waitingForNewShape reports that the next stale replica is available and is not deleted, because no
	// replica that already has the current gang shape is available.
	waitingForNewShape bool
	// recreateTask deletes the next stale replica so that it is recreated with the current gang shape. It is
	// nil while a replica is recovering from its recreate, while waitingForNewShape is set, or when no replica
	// is stale.
	recreateTask *utils.Task
	// cancelUpdate reports that recreateTask deletes the replica under the rolling update, which cancels the
	// update of that replica.
	cancelUpdate bool
}

// inProgress reports whether a replica is stale or is still recovering from its recreate.
func (w gangShapeWork) inProgress() bool {
	return len(w.staleReplicaIndices) > 0 || w.currentlyRecreating != nil
}

// replicaGangShapeState is the gang-shape state of one PodCliqueSet replica.
type replicaGangShapeState struct {
	index int
	// excluded reports that the replica is being gang-terminated in this reconcile. Its PodCliques are deleted
	// and its PCSGs reset, so it is rebuilt with the current gang shape, and it is not stale.
	excluded bool
	// stale reports whether a PodClique or PodCliqueScalingGroup of the replica that is not terminating has a
	// spec.minAvailable other than the one in the PodCliqueSet template.
	stale bool
	// complete reports whether every expected standalone PodClique and PodCliqueScalingGroup exists and is not
	// terminating.
	complete bool
	// breached reports whether a standalone PodClique or PodCliqueScalingGroup has MinAvailableBreached=True.
	breached bool
	// ready reports whether every standalone PodClique has at least spec.minAvailable ready pods and every
	// PodCliqueScalingGroup at least spec.minAvailable available replicas.
	ready bool
}

// available reports whether the replica serves with its gang shape: it is complete, none of its children has
// breached MinAvailable, and each of them has at least its spec.minAvailable ready.
func (s replicaGangShapeState) available() bool {
	return !s.excluded && s.complete && !s.breached && s.ready
}

// getGangShapeWork finds the stale replicas and the task that recreates the next one. Replicas are recreated one
// at a time under every update strategy. The next one is the first of:
//  1. stale replicas that are not available, lowest index first, with the replica under a rolling update ahead
//     of the others. A replica is not available when it is missing a child, or a child is terminating or has
//     MinAvailableBreached=True. The replica under a rolling update is also not available when its update is
//     Coherent, or a child has less than spec.minAvailable ready, since its update has taken it down already.
//  2. the stale replica under a rolling update;
//  3. the other stale replicas, lowest index first.
//
// Recreating a replica that is not available costs no availability, so it is done first. The replica recreated
// last is recorded in the status. No other replica is deleted until it is complete, if it was not available when
// it was deleted, or available, if it was. A stale replica that is available is only deleted when no replica has
// the current gang shape yet, which makes it the canary, or when one that has is available. This way a gang shape
// that cannot be scheduled takes down at most one replica.
//
// pcsIndicesToTerminate are being gang-terminated in this reconcile, so they are rebuilt with the current gang
// shape anyway: they are not stale and not available.
func (r _resource) getGangShapeWork(ctx context.Context, logger logr.Logger, pcs *grovecorev1alpha1.PodCliqueSet, pcsIndicesToTerminate []int) (*gangShapeWork, error) {
	expectedPCSReplicaIndices := lo.Range(int(pcs.Spec.Replicas))
	states, err := r.getReplicaGangShapeStates(ctx, pcs, expectedPCSReplicaIndices, pcsIndicesToTerminate)
	if err != nil {
		return nil, err
	}
	return computeGangShapeWork(logger, pcs, expectedPCSReplicaIndices, states, r.createRecreateTask), nil
}

// recreateTaskFn creates the task that deletes a PodCliqueSet replica so that it is recreated.
type recreateTaskFn func(logger logr.Logger, pcs *grovecorev1alpha1.PodCliqueSet, pcsReplicaIndex int) utils.Task

// computeGangShapeWork computes the gang-shape work from the state of every expected replica. See
// getGangShapeWork for the order and pacing of the recreate.
func computeGangShapeWork(logger logr.Logger, pcs *grovecorev1alpha1.PodCliqueSet, expectedPCSReplicaIndices []int, states map[int]replicaGangShapeState, createTask recreateTaskFn) *gangShapeWork {
	work := &gangShapeWork{}
	updatingReplicaIndex := getCurrentlyUpdatingReplicaIndex(pcs)

	staleStates := make([]replicaGangShapeState, 0, len(expectedPCSReplicaIndices))
	for _, replicaIndex := range expectedPCSReplicaIndices {
		if state := states[replicaIndex]; state.stale {
			staleStates = append(staleStates, state)
		}
	}
	slices.SortStableFunc(staleStates, func(a, b replicaGangShapeState) int {
		return recreatePriority(pcs, updatingReplicaIndex, a) - recreatePriority(pcs, updatingReplicaIndex, b)
	})
	work.staleReplicaIndices = lo.Map(staleStates, func(state replicaGangShapeState, _ int) int { return state.index })

	var next *grovecorev1alpha1.PodCliqueSetReplicaRecreateProgress
	if previous := getCurrentlyRecreating(pcs, expectedPCSReplicaIndices); previous != nil {
		state := states[int(previous.ReplicaIndex)]
		switch {
		case state.stale:
			// The template changed again while the replica was recovering, so it is recreated again. It is the
			// replica that is down already, so it goes first and keeps the availability it had before its first
			// recreate.
			next = previous
		case hasRecovered(previous, state):
			logger.Info("PodCliqueSet replica recovered from its gang-shape recreate", "replicaIndex", previous.ReplicaIndex)
		default:
			work.currentlyRecreating = previous
			return work
		}
	}

	if next == nil {
		if len(staleStates) == 0 {
			return work
		}
		candidate := staleStates[0]
		if candidate.available() && !isNewShapeAvailable(expectedPCSReplicaIndices, states) {
			logger.Info("Waiting for a PodCliqueSet replica with the current gang shape to be available before recreating the next replica", "staleReplicaIndices", work.staleReplicaIndices)
			work.waitingForNewShape = true
			return work
		}
		next = &grovecorev1alpha1.PodCliqueSetReplicaRecreateProgress{
			ReplicaIndex: int32(candidate.index),
			WasAvailable: candidate.available(),
			StartedAt:    metav1.Now(),
		}
	}

	nextIndex := int(next.ReplicaIndex)
	work.staleReplicaIndices = lo.Without(work.staleReplicaIndices, nextIndex)
	work.currentlyRecreating = next
	work.cancelUpdate = updatingReplicaIndex != nil && *updatingReplicaIndex == nextIndex
	task := createTask(logger, pcs, nextIndex)
	work.recreateTask = &task
	return work
}

// createRecreateTask creates the task that deletes a stale PodCliqueSet replica so that it is recreated with the
// current gang shape.
func (r _resource) createRecreateTask(logger logr.Logger, pcs *grovecorev1alpha1.PodCliqueSet, pcsReplicaIndex int) utils.Task {
	reason := fmt.Sprintf("Recreate PodCliqueSet %v replica %d because the MinAvailable of a PodClique or PodCliqueScalingGroup changed", client.ObjectKeyFromObject(pcs), pcsReplicaIndex)
	return r.createPCSReplicaDeleteTask(logger, pcs, pcsReplicaIndex, constants.ReasonGangShapeChanged, reason)
}

// hasRecovered reports whether the replica recreated last has recovered from its recreate: it is available if it
// was available when it was deleted, and complete otherwise.
func hasRecovered(recreating *grovecorev1alpha1.PodCliqueSetReplicaRecreateProgress, state replicaGangShapeState) bool {
	if state.excluded {
		return false
	}
	if recreating.WasAvailable {
		return state.available()
	}
	return state.complete
}

// isNewShapeAvailable reports whether a stale replica that is available may be deleted: no replica has the
// current gang shape yet, or one that has is available.
func isNewShapeAvailable(expectedPCSReplicaIndices []int, states map[int]replicaGangShapeState) bool {
	newShapeStates := lo.FilterMap(expectedPCSReplicaIndices, func(replicaIndex int, _ int) (replicaGangShapeState, bool) {
		state := states[replicaIndex]
		return state, !state.stale
	})
	return len(newShapeStates) == 0 || lo.SomeBy(newShapeStates, replicaGangShapeState.available)
}

// recreatePriority ranks a stale replica for the recreate order; lower goes first. Ties keep index order.
func recreatePriority(pcs *grovecorev1alpha1.PodCliqueSet, updatingReplicaIndex *int, state replicaGangShapeState) int {
	isUpdating := updatingReplicaIndex != nil && *updatingReplicaIndex == state.index
	unavailable := !state.complete || state.breached ||
		(isUpdating && (componentutils.IsCoherentStrategy(pcs) || !state.ready))
	switch {
	case unavailable && isUpdating:
		return 0
	case unavailable:
		return 1
	case isUpdating:
		return 2
	default:
		return 3
	}
}

// getCurrentlyUpdatingReplicaIndex returns the replica under a rolling update, or nil when there is none.
func getCurrentlyUpdatingReplicaIndex(pcs *grovecorev1alpha1.PodCliqueSet) *int {
	if pcs.Status.UpdateProgress == nil || len(pcs.Status.UpdateProgress.CurrentlyUpdating) == 0 {
		return nil
	}
	return ptr.To(int(pcs.Status.UpdateProgress.CurrentlyUpdating[0].ReplicaIndex))
}

// getCurrentlyRecreating returns the replica that the recreate deleted last, according to the PodCliqueSet
// status, or nil when there is none or the replica is no longer expected.
func getCurrentlyRecreating(pcs *grovecorev1alpha1.PodCliqueSet, expectedPCSReplicaIndices []int) *grovecorev1alpha1.PodCliqueSetReplicaRecreateProgress {
	if pcs.Status.GangShapeRecreate == nil || pcs.Status.GangShapeRecreate.CurrentlyRecreating == nil {
		return nil
	}
	recreating := pcs.Status.GangShapeRecreate.CurrentlyRecreating
	if !slices.Contains(expectedPCSReplicaIndices, int(recreating.ReplicaIndex)) {
		return nil
	}
	return recreating.DeepCopy()
}

// getReplicaGangShapeStates returns the gang-shape state of every expected replica, keyed by replica index.
func (r _resource) getReplicaGangShapeStates(ctx context.Context, pcs *grovecorev1alpha1.PodCliqueSet, expectedPCSReplicaIndices, pcsIndicesToTerminate []int) (map[int]replicaGangShapeState, error) {
	pclqs, err := componentutils.ListPCLQsMatchingLabels(ctx, r.client, pcs.Namespace, apicommon.GetDefaultLabelsForPodCliqueSetManagedResources(pcs.Name))
	if err != nil {
		return nil, groveerr.WrapError(err, errCodeListPCLQs, component.OperationSync,
			fmt.Sprintf("could not list PCLQs for PCS: %v", client.ObjectKeyFromObject(pcs)))
	}
	pcsgs, err := componentutils.GetPCSGsForPCS(ctx, r.client, pcs.ObjectMeta)
	if err != nil {
		return nil, groveerr.WrapError(err, errCodeListPCSGs, component.OperationSync,
			fmt.Sprintf("could not list PCSGs for PCS: %v", client.ObjectKeyFromObject(pcs)))
	}
	// Exclude PodCliques of an older PodCliqueSet of the same name, as GetPCSGsForPCS does for PCSGs.
	pclqs = lo.Filter(pclqs, func(pclq grovecorev1alpha1.PodClique, _ int) bool {
		return metav1.IsControlledBy(&pclq, pcs) ||
			lo.SomeBy(pcsgs, func(pcsg grovecorev1alpha1.PodCliqueScalingGroup) bool { return metav1.IsControlledBy(&pclq, &pcsg) })
	})
	return computeReplicaGangShapeStates(pcs, expectedPCSReplicaIndices, pcsIndicesToTerminate, pclqs, pcsgs), nil
}

// computeReplicaGangShapeStates computes the gang-shape state of every expected replica from the PodCliques and
// PodCliqueScalingGroups of the PodCliqueSet, keyed by replica index.
func computeReplicaGangShapeStates(pcs *grovecorev1alpha1.PodCliqueSet, expectedPCSReplicaIndices, pcsIndicesToTerminate []int, pclqs []grovecorev1alpha1.PodClique, pcsgs []grovecorev1alpha1.PodCliqueScalingGroup) map[int]replicaGangShapeState {
	pclqsByReplica := groupByPCSReplicaIndex(pclqs, func(pclq grovecorev1alpha1.PodClique) metav1.ObjectMeta { return pclq.ObjectMeta })
	pcsgsByReplica := groupByPCSReplicaIndex(pcsgs, func(pcsg grovecorev1alpha1.PodCliqueScalingGroup) metav1.ObjectMeta { return pcsg.ObjectMeta })

	states := make(map[int]replicaGangShapeState, len(expectedPCSReplicaIndices))
	for _, replicaIndex := range expectedPCSReplicaIndices {
		if slices.Contains(pcsIndicesToTerminate, replicaIndex) {
			states[replicaIndex] = replicaGangShapeState{index: replicaIndex, excluded: true}
			continue
		}
		replicaPCLQs := lo.Filter(pclqsByReplica[replicaIndex], func(pclq grovecorev1alpha1.PodClique, _ int) bool {
			return !k8sutils.IsResourceTerminating(pclq.ObjectMeta)
		})
		replicaPCSGs := lo.Filter(pcsgsByReplica[replicaIndex], func(pcsg grovecorev1alpha1.PodCliqueScalingGroup, _ int) bool {
			return !k8sutils.IsResourceTerminating(pcsg.ObjectMeta)
		})
		standalonePCLQs := lo.Filter(replicaPCLQs, func(pclq grovecorev1alpha1.PodClique, _ int) bool {
			_, isPCSGMember := pclq.Labels[apicommon.LabelPodCliqueScalingGroup]
			return !isPCSGMember
		})
		states[replicaIndex] = replicaGangShapeState{
			index: replicaIndex,
			stale: lo.SomeBy(replicaPCLQs, func(pclq grovecorev1alpha1.PodClique) bool { return isPCLQGangShapeStale(pcs, &pclq) }) ||
				lo.SomeBy(replicaPCSGs, func(pcsg grovecorev1alpha1.PodCliqueScalingGroup) bool {
					return isPCSGGangShapeStale(pcs, replicaIndex, &pcsg)
				}),
			complete: isReplicaComplete(pcs, replicaIndex, standalonePCLQs, replicaPCSGs),
			breached: lo.SomeBy(standalonePCLQs, func(pclq grovecorev1alpha1.PodClique) bool {
				return meta.IsStatusConditionTrue(pclq.Status.Conditions, apiconstants.ConditionTypeMinAvailableBreached)
			}) || lo.SomeBy(replicaPCSGs, func(pcsg grovecorev1alpha1.PodCliqueScalingGroup) bool {
				return meta.IsStatusConditionTrue(pcsg.Status.Conditions, apiconstants.ConditionTypeMinAvailableBreached)
			}),
			ready: lo.EveryBy(standalonePCLQs, isPCLQReady) && lo.EveryBy(replicaPCSGs, isPCSGReady),
		}
	}
	return states
}

// isPCLQGangShapeStale reports whether the spec.minAvailable of the PodClique differs from the one in its clique
// template. A PodClique without spec.minAvailable, or whose template is gone, is not stale.
func isPCLQGangShapeStale(pcs *grovecorev1alpha1.PodCliqueSet, pclq *grovecorev1alpha1.PodClique) bool {
	if pclq.Spec.MinAvailable == nil {
		return false
	}
	cliqueName, err := componentutils.GetPodCliqueNameFromPodCliqueFQN(pclq.ObjectMeta)
	if err != nil {
		return false
	}
	pclqTemplateSpec := componentutils.FindPodCliqueTemplateSpecByName(pcs, cliqueName)
	if pclqTemplateSpec == nil || pclqTemplateSpec.Spec.MinAvailable == nil {
		return false
	}
	return *pclq.Spec.MinAvailable != *pclqTemplateSpec.Spec.MinAvailable
}

// isPCSGGangShapeStale reports whether the spec.minAvailable of the PodCliqueScalingGroup differs from the one in
// its PodCliqueScalingGroupConfig. A PodCliqueScalingGroup without spec.minAvailable, or whose config is gone, is
// not stale.
func isPCSGGangShapeStale(pcs *grovecorev1alpha1.PodCliqueSet, pcsReplicaIndex int, pcsg *grovecorev1alpha1.PodCliqueScalingGroup) bool {
	if pcsg.Spec.MinAvailable == nil {
		return false
	}
	pcsgConfig, ok := lo.Find(pcs.Spec.Template.PodCliqueScalingGroupConfigs, func(pcsgConfig grovecorev1alpha1.PodCliqueScalingGroupConfig) bool {
		return apicommon.GeneratePodCliqueScalingGroupName(apicommon.ResourceNameReplica{Name: pcs.Name, Replica: pcsReplicaIndex}, pcsgConfig.Name) == pcsg.Name
	})
	if !ok || pcsgConfig.MinAvailable == nil {
		return false
	}
	return *pcsg.Spec.MinAvailable != *pcsgConfig.MinAvailable
}

// isReplicaComplete reports whether every expected standalone PodClique and PodCliqueScalingGroup of the replica
// is among the given ones, which are not terminating.
func isReplicaComplete(pcs *grovecorev1alpha1.PodCliqueSet, pcsReplicaIndex int, standalonePCLQs []grovecorev1alpha1.PodClique, pcsgs []grovecorev1alpha1.PodCliqueScalingGroup) bool {
	pclqNames := lo.Map(standalonePCLQs, func(pclq grovecorev1alpha1.PodClique, _ int) string { return pclq.Name })
	pcsgNames := lo.Map(pcsgs, func(pcsg grovecorev1alpha1.PodCliqueScalingGroup, _ int) string { return pcsg.Name })
	expectedPCSGNames := lo.Map(pcs.Spec.Template.PodCliqueScalingGroupConfigs, func(pcsgConfig grovecorev1alpha1.PodCliqueScalingGroupConfig, _ int) string {
		return apicommon.GeneratePodCliqueScalingGroupName(apicommon.ResourceNameReplica{Name: pcs.Name, Replica: pcsReplicaIndex}, pcsgConfig.Name)
	})
	return lo.Every(pclqNames, componentutils.GetPodCliqueFQNsForPCSReplicaNotInPCSG(pcs, pcsReplicaIndex)) &&
		lo.Every(pcsgNames, expectedPCSGNames)
}

// isPCLQReady reports whether the PodClique has at least spec.minAvailable ready pods.
func isPCLQReady(pclq grovecorev1alpha1.PodClique) bool {
	return pclq.Spec.Replicas == 0 || pclq.Status.ReadyReplicas >= ptr.Deref(pclq.Spec.MinAvailable, pclq.Spec.Replicas)
}

// isPCSGReady reports whether the PodCliqueScalingGroup has at least spec.minAvailable available replicas.
func isPCSGReady(pcsg grovecorev1alpha1.PodCliqueScalingGroup) bool {
	return pcsg.Spec.Replicas == 0 || pcsg.Status.AvailableReplicas >= ptr.Deref(pcsg.Spec.MinAvailable, pcsg.Spec.Replicas)
}

// groupByPCSReplicaIndex groups items by the PodCliqueSet replica index label. Items without a valid label are
// left out.
func groupByPCSReplicaIndex[T any](items []T, objectMetaOf func(T) metav1.ObjectMeta) map[int][]T {
	grouped := make(map[int][]T)
	for _, item := range items {
		replicaIndex, err := strconv.Atoi(objectMetaOf(item).Labels[apicommon.LabelPodCliqueSetReplicaIndex])
		if err != nil {
			continue
		}
		grouped[replicaIndex] = append(grouped[replicaIndex], item)
	}
	return grouped
}

// patchGangShapeRecreateStatus writes status.gangShapeRecreate and the GangShapeRecreateInProgress condition for
// work. It runs before the recreate task, so the replica being deleted is recorded first. When the recreate task
// deletes the replica under the rolling update, the same patch clears it from status.updateProgress, which
// cancels its update. The condition is only written once a recreate has started, so a PodCliqueSet whose gang
// shape never changed gets none.
func (r _resource) patchGangShapeRecreateStatus(ctx context.Context, logger logr.Logger, pcs *grovecorev1alpha1.PodCliqueSet, work *gangShapeWork) error {
	original := pcs.DeepCopy()
	mutateGangShapeRecreateStatus(pcs, work, metav1.Now())
	if equality.Semantic.DeepEqual(original.Status, pcs.Status) {
		return nil
	}
	if err := r.client.Status().Patch(ctx, pcs, client.MergeFrom(original)); err != nil {
		return groveerr.WrapError(err, errCodeUpdatePCSStatus, component.OperationSync,
			fmt.Sprintf("could not patch the gang-shape recreate status of PCS: %v", client.ObjectKeyFromObject(pcs)))
	}
	logger.V(1).Info("Updated the PodCliqueSet status with gang-shape recreate progress", "gangShapeRecreate", pcs.Status.GangShapeRecreate)
	return nil
}

// mutateGangShapeRecreateStatus sets status.gangShapeRecreate and the GangShapeRecreateInProgress condition of
// the PodCliqueSet from work, and clears the replica under the rolling update when work cancels its update.
func mutateGangShapeRecreateStatus(pcs *grovecorev1alpha1.PodCliqueSet, work *gangShapeWork, now metav1.Time) {
	if !work.inProgress() {
		if pcs.Status.GangShapeRecreate == nil && meta.FindStatusCondition(pcs.Status.Conditions, apiconstants.ConditionTypeGangShapeRecreateInProgress) == nil {
			return
		}
		pcs.Status.GangShapeRecreate = nil
		meta.SetStatusCondition(&pcs.Status.Conditions, metav1.Condition{
			Type:    apiconstants.ConditionTypeGangShapeRecreateInProgress,
			Status:  metav1.ConditionFalse,
			Reason:  apiconstants.ConditionReasonGangShapeCurrent,
			Message: "Every PodCliqueSet replica has the current gang shape",
		})
		return
	}

	progress := &grovecorev1alpha1.GangShapeRecreateProgress{StartedAt: now}
	if pcs.Status.GangShapeRecreate != nil {
		progress.StartedAt = pcs.Status.GangShapeRecreate.StartedAt
	}
	progress.CurrentlyRecreating = work.currentlyRecreating.DeepCopy()
	pcs.Status.GangShapeRecreate = progress

	if work.cancelUpdate {
		pcs.Status.UpdateProgress.CurrentlyUpdating = nil
	}

	condition := metav1.Condition{
		Type:   apiconstants.ConditionTypeGangShapeRecreateInProgress,
		Status: metav1.ConditionTrue,
	}
	switch {
	case work.currentlyRecreating != nil:
		condition.Reason = apiconstants.ConditionReasonRecreatingReplica
		awaited := "complete"
		if work.currentlyRecreating.WasAvailable {
			awaited = "available"
		}
		condition.Message = fmt.Sprintf("Recreating PodCliqueSet replica %d with the current gang shape and waiting for it to be %s", work.currentlyRecreating.ReplicaIndex, awaited)
		if len(work.staleReplicaIndices) > 0 {
			condition.Message += fmt.Sprintf("; replicas %v are recreated next", work.staleReplicaIndices)
		}
	case work.waitingForNewShape:
		condition.Reason = apiconstants.ConditionReasonWaitingForNewShapeAvailable
		condition.Message = fmt.Sprintf("Waiting for a PodCliqueSet replica with the current gang shape to be available before recreating replicas %v", work.staleReplicaIndices)
	}
	meta.SetStatusCondition(&pcs.Status.Conditions, condition)
}
