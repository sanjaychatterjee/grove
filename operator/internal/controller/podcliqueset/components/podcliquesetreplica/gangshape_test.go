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
	"errors"
	"strconv"
	"testing"
	"time"

	apicommon "github.com/ai-dynamo/grove/operator/api/common"
	apiconstants "github.com/ai-dynamo/grove/operator/api/common/constants"
	grovecorev1alpha1 "github.com/ai-dynamo/grove/operator/api/core/v1alpha1"
	"github.com/ai-dynamo/grove/operator/internal/constants"
	groveerr "github.com/ai-dynamo/grove/operator/internal/errors"
	"github.com/ai-dynamo/grove/operator/internal/utils"

	"github.com/go-logr/logr"
	"github.com/samber/lo"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/record"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

const (
	testGangShapePCSName   = "pcs"
	testGangShapeNamespace = "default"
)

// newGangShapeTestPCS returns a PodCliqueSet with a standalone clique "a" (replicas 2, minAvailable 2), and a
// PodCliqueScalingGroup "sg" (replicas 2, minAvailable 1) whose clique "b" has replicas 1 and minAvailable 1.
func newGangShapeTestPCS(replicas int32) *grovecorev1alpha1.PodCliqueSet {
	return &grovecorev1alpha1.PodCliqueSet{
		ObjectMeta: metav1.ObjectMeta{Name: testGangShapePCSName, Namespace: testGangShapeNamespace},
		Spec: grovecorev1alpha1.PodCliqueSetSpec{
			Replicas: replicas,
			Template: grovecorev1alpha1.PodCliqueSetTemplateSpec{
				TerminationDelay: &metav1.Duration{Duration: time.Hour},
				Cliques: []*grovecorev1alpha1.PodCliqueTemplateSpec{
					{Name: "a", Spec: grovecorev1alpha1.PodCliqueSpec{Replicas: 2, MinAvailable: ptr.To[int32](2)}},
					{Name: "b", Spec: grovecorev1alpha1.PodCliqueSpec{Replicas: 1, MinAvailable: ptr.To[int32](1)}},
				},
				PodCliqueScalingGroupConfigs: []grovecorev1alpha1.PodCliqueScalingGroupConfig{
					{Name: "sg", CliqueNames: []string{"b"}, Replicas: ptr.To[int32](2), MinAvailable: ptr.To[int32](1)},
				},
			},
		},
	}
}

func replicaLabels(pcsReplicaIndex int) map[string]string {
	return lo.Assign(
		apicommon.GetDefaultLabelsForPodCliqueSetManagedResources(testGangShapePCSName),
		map[string]string{apicommon.LabelPodCliqueSetReplicaIndex: strconv.Itoa(pcsReplicaIndex)},
	)
}

// newStandalonePCLQ returns the standalone PodClique "a" of the replica with the given minAvailable and ready
// pods.
func newStandalonePCLQ(pcsReplicaIndex int, minAvailable, readyReplicas int32) grovecorev1alpha1.PodClique {
	return grovecorev1alpha1.PodClique{
		ObjectMeta: metav1.ObjectMeta{
			Name:      apicommon.GeneratePodCliqueName(apicommon.ResourceNameReplica{Name: testGangShapePCSName, Replica: pcsReplicaIndex}, "a"),
			Namespace: testGangShapeNamespace,
			Labels:    replicaLabels(pcsReplicaIndex),
		},
		Spec:   grovecorev1alpha1.PodCliqueSpec{Replicas: 2, MinAvailable: ptr.To(minAvailable)},
		Status: grovecorev1alpha1.PodCliqueStatus{ReadyReplicas: readyReplicas},
	}
}

// newPCSG returns the PodCliqueScalingGroup "sg" of the replica with the given minAvailable and available
// replicas.
func newPCSG(pcsReplicaIndex int, minAvailable, availableReplicas int32) grovecorev1alpha1.PodCliqueScalingGroup {
	return grovecorev1alpha1.PodCliqueScalingGroup{
		ObjectMeta: metav1.ObjectMeta{
			Name:      apicommon.GeneratePodCliqueScalingGroupName(apicommon.ResourceNameReplica{Name: testGangShapePCSName, Replica: pcsReplicaIndex}, "sg"),
			Namespace: testGangShapeNamespace,
			Labels:    replicaLabels(pcsReplicaIndex),
		},
		Spec:   grovecorev1alpha1.PodCliqueScalingGroupSpec{Replicas: 2, MinAvailable: ptr.To(minAvailable)},
		Status: grovecorev1alpha1.PodCliqueScalingGroupStatus{AvailableReplicas: availableReplicas},
	}
}

// newPCSGMemberPCLQ returns the PodClique "b" of PCSG replica 0 in the replica with the given minAvailable.
func newPCSGMemberPCLQ(pcsReplicaIndex int, minAvailable int32) grovecorev1alpha1.PodClique {
	pcsgName := apicommon.GeneratePodCliqueScalingGroupName(apicommon.ResourceNameReplica{Name: testGangShapePCSName, Replica: pcsReplicaIndex}, "sg")
	return grovecorev1alpha1.PodClique{
		ObjectMeta: metav1.ObjectMeta{
			Name:      apicommon.GeneratePodCliqueName(apicommon.ResourceNameReplica{Name: pcsgName, Replica: 0}, "b"),
			Namespace: testGangShapeNamespace,
			Labels: lo.Assign(replicaLabels(pcsReplicaIndex), map[string]string{
				apicommon.LabelPodCliqueScalingGroup:             pcsgName,
				apicommon.LabelPodCliqueScalingGroupReplicaIndex: "0",
			}),
		},
		Spec: grovecorev1alpha1.PodCliqueSpec{Replicas: 1, MinAvailable: ptr.To(minAvailable)},
	}
}

func withCondition[T interface {
	grovecorev1alpha1.PodClique | grovecorev1alpha1.PodCliqueScalingGroup
}](obj T, conditionType string, status metav1.ConditionStatus) T {
	condition := metav1.Condition{Type: conditionType, Status: status, Reason: "Test"}
	switch o := any(&obj).(type) {
	case *grovecorev1alpha1.PodClique:
		meta.SetStatusCondition(&o.Status.Conditions, condition)
	case *grovecorev1alpha1.PodCliqueScalingGroup:
		meta.SetStatusCondition(&o.Status.Conditions, condition)
	}
	return obj
}

func TestComputeReplicaGangShapeStates(t *testing.T) {
	pcs := newGangShapeTestPCS(1)
	terminating := func(pclq grovecorev1alpha1.PodClique) grovecorev1alpha1.PodClique {
		pclq.DeletionTimestamp = ptr.To(metav1.Now())
		pclq.Finalizers = []string{"test"}
		return pclq
	}
	tests := []struct {
		name  string
		pclqs []grovecorev1alpha1.PodClique
		pcsgs []grovecorev1alpha1.PodCliqueScalingGroup
		want  replicaGangShapeState
	}{
		{
			name:  "current gang shape, every child ready",
			pclqs: []grovecorev1alpha1.PodClique{newStandalonePCLQ(0, 2, 2), newPCSGMemberPCLQ(0, 1)},
			pcsgs: []grovecorev1alpha1.PodCliqueScalingGroup{newPCSG(0, 1, 2)},
			want:  replicaGangShapeState{complete: true, ready: true},
		},
		{
			name:  "standalone PodClique minAvailable differs from its template",
			pclqs: []grovecorev1alpha1.PodClique{newStandalonePCLQ(0, 1, 2)},
			pcsgs: []grovecorev1alpha1.PodCliqueScalingGroup{newPCSG(0, 1, 2)},
			want:  replicaGangShapeState{stale: true, complete: true, ready: true},
		},
		{
			name:  "PCSG minAvailable differs from its config",
			pclqs: []grovecorev1alpha1.PodClique{newStandalonePCLQ(0, 2, 2)},
			pcsgs: []grovecorev1alpha1.PodCliqueScalingGroup{newPCSG(0, 2, 2)},
			want:  replicaGangShapeState{stale: true, complete: true, ready: true},
		},
		{
			name:  "PCSG member PodClique minAvailable differs from its template",
			pclqs: []grovecorev1alpha1.PodClique{newStandalonePCLQ(0, 2, 2), newPCSGMemberPCLQ(0, 2)},
			pcsgs: []grovecorev1alpha1.PodCliqueScalingGroup{newPCSG(0, 1, 2)},
			want:  replicaGangShapeState{stale: true, complete: true, ready: true},
		},
		{
			name: "child without minAvailable is not stale",
			pclqs: []grovecorev1alpha1.PodClique{func() grovecorev1alpha1.PodClique {
				pclq := newStandalonePCLQ(0, 2, 2)
				pclq.Spec.MinAvailable = nil
				return pclq
			}()},
			pcsgs: []grovecorev1alpha1.PodCliqueScalingGroup{newPCSG(0, 1, 2)},
			want:  replicaGangShapeState{complete: true, ready: true},
		},
		{
			name:  "terminating stale PodClique is ignored and leaves the replica incomplete",
			pclqs: []grovecorev1alpha1.PodClique{terminating(newStandalonePCLQ(0, 1, 2))},
			pcsgs: []grovecorev1alpha1.PodCliqueScalingGroup{newPCSG(0, 1, 2)},
			want:  replicaGangShapeState{ready: true},
		},
		{
			name:  "missing PCSG leaves the replica incomplete",
			pclqs: []grovecorev1alpha1.PodClique{newStandalonePCLQ(0, 2, 2)},
			want:  replicaGangShapeState{ready: true},
		},
		{
			name:  "breached PCSG",
			pclqs: []grovecorev1alpha1.PodClique{newStandalonePCLQ(0, 2, 2)},
			pcsgs: []grovecorev1alpha1.PodCliqueScalingGroup{withCondition(newPCSG(0, 1, 2), apiconstants.ConditionTypeMinAvailableBreached, metav1.ConditionTrue)},
			want:  replicaGangShapeState{complete: true, breached: true, ready: true},
		},
		{
			name:  "breached standalone PodClique",
			pclqs: []grovecorev1alpha1.PodClique{withCondition(newStandalonePCLQ(0, 2, 2), apiconstants.ConditionTypeMinAvailableBreached, metav1.ConditionTrue)},
			pcsgs: []grovecorev1alpha1.PodCliqueScalingGroup{newPCSG(0, 1, 2)},
			want:  replicaGangShapeState{complete: true, breached: true, ready: true},
		},
		{
			name:  "unknown MinAvailableBreached is not a breach",
			pclqs: []grovecorev1alpha1.PodClique{withCondition(newStandalonePCLQ(0, 2, 2), apiconstants.ConditionTypeMinAvailableBreached, metav1.ConditionUnknown)},
			pcsgs: []grovecorev1alpha1.PodCliqueScalingGroup{newPCSG(0, 1, 2)},
			want:  replicaGangShapeState{complete: true, ready: true},
		},
		{
			name:  "standalone PodClique below its minAvailable ready pods",
			pclqs: []grovecorev1alpha1.PodClique{newStandalonePCLQ(0, 2, 1)},
			pcsgs: []grovecorev1alpha1.PodCliqueScalingGroup{newPCSG(0, 1, 2)},
			want:  replicaGangShapeState{complete: true},
		},
		{
			name:  "PCSG below its minAvailable available replicas",
			pclqs: []grovecorev1alpha1.PodClique{newStandalonePCLQ(0, 2, 2)},
			pcsgs: []grovecorev1alpha1.PodCliqueScalingGroup{newPCSG(0, 1, 0)},
			want:  replicaGangShapeState{complete: true},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			states := computeReplicaGangShapeStates(pcs, []int{0}, nil, tc.pclqs, tc.pcsgs)
			assert.Equal(t, tc.want, states[0])
		})
	}

	t.Run("gang-terminated replica is excluded and not stale", func(t *testing.T) {
		states := computeReplicaGangShapeStates(pcs, []int{0}, []int{0},
			[]grovecorev1alpha1.PodClique{newStandalonePCLQ(0, 1, 2)},
			[]grovecorev1alpha1.PodCliqueScalingGroup{newPCSG(0, 2, 2)})
		assert.Equal(t, replicaGangShapeState{excluded: true}, states[0])
		assert.False(t, states[0].available())
	})

	t.Run("sparse replica indices", func(t *testing.T) {
		pcs := newGangShapeTestPCS(4)
		states := computeReplicaGangShapeStates(pcs, []int{0, 2, 3}, nil,
			[]grovecorev1alpha1.PodClique{newStandalonePCLQ(0, 2, 2), newStandalonePCLQ(2, 1, 2), newStandalonePCLQ(3, 2, 2)},
			[]grovecorev1alpha1.PodCliqueScalingGroup{newPCSG(0, 1, 2), newPCSG(2, 1, 2), newPCSG(3, 1, 2)})
		assert.Len(t, states, 3)
		assert.NotContains(t, states, 1)
		assert.Equal(t, replicaGangShapeState{index: 2, stale: true, complete: true, ready: true}, states[2])
		assert.False(t, states[0].stale)
		assert.False(t, states[3].stale)
	})
}

// state helpers for computeGangShapeWork tests.
func availableState(index int, stale bool) replicaGangShapeState {
	return replicaGangShapeState{index: index, stale: stale, complete: true, ready: true}
}

func staleBreachedState(index int) replicaGangShapeState {
	return replicaGangShapeState{index: index, stale: true, complete: true, breached: true, ready: true}
}

func incompleteState(index int, stale bool) replicaGangShapeState {
	return replicaGangShapeState{index: index, stale: stale}
}

func notReadyState(index int, stale bool) replicaGangShapeState {
	return replicaGangShapeState{index: index, stale: stale, complete: true}
}

func statesOf(states ...replicaGangShapeState) map[int]replicaGangShapeState {
	return lo.SliceToMap(states, func(state replicaGangShapeState) (int, replicaGangShapeState) { return state.index, state })
}

func withUpdatingReplica(pcs *grovecorev1alpha1.PodCliqueSet, strategy grovecorev1alpha1.UpdateStrategyType, replicaIndex int32) *grovecorev1alpha1.PodCliqueSet {
	pcs.Spec.UpdateStrategy = &grovecorev1alpha1.PodCliqueSetUpdateStrategy{Type: strategy}
	pcs.Status.UpdateProgress = &grovecorev1alpha1.PodCliqueSetUpdateProgress{
		UpdateStartedAt:   metav1.Now(),
		CurrentlyUpdating: []grovecorev1alpha1.PodCliqueSetReplicaUpdateProgress{{ReplicaIndex: replicaIndex, UpdateStartedAt: metav1.Now()}},
	}
	return pcs
}

func withCurrentlyRecreating(pcs *grovecorev1alpha1.PodCliqueSet, replicaIndex int32, wasAvailable bool) *grovecorev1alpha1.PodCliqueSet {
	pcs.Status.GangShapeRecreate = &grovecorev1alpha1.GangShapeRecreateProgress{
		StartedAt:           metav1.NewTime(time.Now().Add(-time.Hour)),
		CurrentlyRecreating: &grovecorev1alpha1.PodCliqueSetReplicaRecreateProgress{ReplicaIndex: replicaIndex, WasAvailable: wasAvailable, StartedAt: metav1.NewTime(time.Now().Add(-time.Minute))},
	}
	return pcs
}

// recordingTaskFn returns a recreateTaskFn that records the replica of the task it creates.
func recordingTaskFn(recreated *[]int) recreateTaskFn {
	return func(_ logr.Logger, _ *grovecorev1alpha1.PodCliqueSet, pcsReplicaIndex int) utils.Task {
		*recreated = append(*recreated, pcsReplicaIndex)
		return utils.Task{Name: strconv.Itoa(pcsReplicaIndex), Fn: func(context.Context) error { return nil }}
	}
}

func TestComputeGangShapeWork(t *testing.T) {
	tests := []struct {
		name   string
		pcs    *grovecorev1alpha1.PodCliqueSet
		states map[int]replicaGangShapeState
		// wantRecreate is the replica that is deleted, or nil when none is.
		wantRecreate *int
		// wantStale are the remaining stale replicas in recreate order.
		wantStale               []int
		wantCurrentlyRecreating *grovecorev1alpha1.PodCliqueSetReplicaRecreateProgress
		wantWaitingForNewShape  bool
		wantCancelUpdate        bool
		wantInProgress          bool
	}{
		{
			name:   "no stale replica",
			pcs:    newGangShapeTestPCS(2),
			states: statesOf(availableState(0, false), availableState(1, false)),
		},
		{
			name:                    "every replica stale and available: the lowest index is the canary",
			pcs:                     newGangShapeTestPCS(3),
			states:                  statesOf(availableState(0, true), availableState(1, true), availableState(2, true)),
			wantRecreate:            ptr.To(0),
			wantStale:               []int{1, 2},
			wantCurrentlyRecreating: &grovecorev1alpha1.PodCliqueSetReplicaRecreateProgress{ReplicaIndex: 0, WasAvailable: true},
			wantInProgress:          true,
		},
		{
			name:                    "unavailable stale replicas go first, lowest index first",
			pcs:                     newGangShapeTestPCS(4),
			states:                  statesOf(availableState(0, true), staleBreachedState(1), notReadyState(2, true), incompleteState(3, true)),
			wantRecreate:            ptr.To(1),
			wantStale:               []int{3, 0, 2},
			wantCurrentlyRecreating: &grovecorev1alpha1.PodCliqueSetReplicaRecreateProgress{ReplicaIndex: 1},
			wantInProgress:          true,
		},
		{
			name:                    "available RollingRecreate replica under update goes after unavailable replicas and before the rest",
			pcs:                     withUpdatingReplica(newGangShapeTestPCS(4), grovecorev1alpha1.RollingRecreateStrategy, 3),
			states:                  statesOf(availableState(0, true), availableState(1, true), staleBreachedState(2), availableState(3, true)),
			wantRecreate:            ptr.To(2),
			wantStale:               []int{3, 0, 1},
			wantCurrentlyRecreating: &grovecorev1alpha1.PodCliqueSetReplicaRecreateProgress{ReplicaIndex: 2},
			wantInProgress:          true,
		},
		{
			name:                    "unavailable replica under update goes ahead of the other unavailable replicas",
			pcs:                     withUpdatingReplica(newGangShapeTestPCS(4), grovecorev1alpha1.RollingRecreateStrategy, 3),
			states:                  statesOf(availableState(0, true), staleBreachedState(1), availableState(2, true), staleBreachedState(3)),
			wantRecreate:            ptr.To(3),
			wantStale:               []int{1, 0, 2},
			wantCurrentlyRecreating: &grovecorev1alpha1.PodCliqueSetReplicaRecreateProgress{ReplicaIndex: 3},
			wantCancelUpdate:        true,
			wantInProgress:          true,
		},
		{
			name:                    "RollingRecreate replica under update below minAvailable ready is unavailable",
			pcs:                     withUpdatingReplica(newGangShapeTestPCS(3), grovecorev1alpha1.RollingRecreateStrategy, 2),
			states:                  statesOf(staleBreachedState(0), availableState(1, true), notReadyState(2, true)),
			wantRecreate:            ptr.To(2),
			wantStale:               []int{0, 1},
			wantCurrentlyRecreating: &grovecorev1alpha1.PodCliqueSetReplicaRecreateProgress{ReplicaIndex: 2},
			wantCancelUpdate:        true,
			wantInProgress:          true,
		},
		{
			name:                    "replica under Coherent update is unavailable even when it is ready",
			pcs:                     withUpdatingReplica(newGangShapeTestPCS(3), grovecorev1alpha1.CoherentStrategy, 1),
			states:                  statesOf(staleBreachedState(0), availableState(1, true), availableState(2, true)),
			wantRecreate:            ptr.To(1),
			wantStale:               []int{0, 2},
			wantCurrentlyRecreating: &grovecorev1alpha1.PodCliqueSetReplicaRecreateProgress{ReplicaIndex: 1, WasAvailable: true},
			wantCancelUpdate:        true,
			wantInProgress:          true,
		},
		{
			name:                   "available stale replica waits until a replica with the current gang shape is available",
			pcs:                    newGangShapeTestPCS(2),
			states:                 statesOf(notReadyState(0, false), availableState(1, true)),
			wantStale:              []int{1},
			wantWaitingForNewShape: true,
			wantInProgress:         true,
		},
		{
			name:                   "a gang-terminated replica counts as having the current gang shape",
			pcs:                    newGangShapeTestPCS(2),
			states:                 statesOf(replicaGangShapeState{index: 0, excluded: true}, availableState(1, true)),
			wantStale:              []int{1},
			wantWaitingForNewShape: true,
			wantInProgress:         true,
		},
		{
			name:                    "available stale replica is deleted once one replica with the current gang shape is available",
			pcs:                     newGangShapeTestPCS(3),
			states:                  statesOf(notReadyState(0, false), availableState(1, false), availableState(2, true)),
			wantRecreate:            ptr.To(2),
			wantStale:               []int{},
			wantCurrentlyRecreating: &grovecorev1alpha1.PodCliqueSetReplicaRecreateProgress{ReplicaIndex: 2, WasAvailable: true},
			wantInProgress:          true,
		},
		{
			name:                    "unavailable stale replica is deleted even when no replica with the current gang shape is available",
			pcs:                     newGangShapeTestPCS(2),
			states:                  statesOf(notReadyState(0, false), staleBreachedState(1)),
			wantRecreate:            ptr.To(1),
			wantStale:               []int{},
			wantCurrentlyRecreating: &grovecorev1alpha1.PodCliqueSetReplicaRecreateProgress{ReplicaIndex: 1},
			wantInProgress:          true,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			var recreated []int
			work := computeGangShapeWork(logr.Discard(), tc.pcs, lo.Range(int(tc.pcs.Spec.Replicas)), tc.states, recordingTaskFn(&recreated))
			assertGangShapeWork(t, work, recreated, tc.wantRecreate, tc.wantStale, tc.wantCurrentlyRecreating)
			assert.Equal(t, tc.wantWaitingForNewShape, work.waitingForNewShape)
			assert.Equal(t, tc.wantCancelUpdate, work.cancelUpdate)
			assert.Equal(t, tc.wantInProgress, work.inProgress())
		})
	}
}

func TestComputeGangShapeWorkCurrentlyRecreating(t *testing.T) {
	tests := []struct {
		name                    string
		pcs                     *grovecorev1alpha1.PodCliqueSet
		states                  map[int]replicaGangShapeState
		wantRecreate            *int
		wantStale               []int
		wantCurrentlyRecreating *grovecorev1alpha1.PodCliqueSetReplicaRecreateProgress
	}{
		{
			name:                    "replica that was available waits until it is available again",
			pcs:                     withCurrentlyRecreating(newGangShapeTestPCS(3), 0, true),
			states:                  statesOf(notReadyState(0, false), availableState(1, true), staleBreachedState(2)),
			wantStale:               []int{2, 1},
			wantCurrentlyRecreating: &grovecorev1alpha1.PodCliqueSetReplicaRecreateProgress{ReplicaIndex: 0, WasAvailable: true},
		},
		{
			name:                    "replica that was available is done once it is available again",
			pcs:                     withCurrentlyRecreating(newGangShapeTestPCS(2), 0, true),
			states:                  statesOf(availableState(0, false), availableState(1, true)),
			wantRecreate:            ptr.To(1),
			wantStale:               []int{},
			wantCurrentlyRecreating: &grovecorev1alpha1.PodCliqueSetReplicaRecreateProgress{ReplicaIndex: 1, WasAvailable: true},
		},
		{
			name:                    "replica that was not available waits until it is complete",
			pcs:                     withCurrentlyRecreating(newGangShapeTestPCS(2), 0, false),
			states:                  statesOf(incompleteState(0, false), staleBreachedState(1)),
			wantStale:               []int{1},
			wantCurrentlyRecreating: &grovecorev1alpha1.PodCliqueSetReplicaRecreateProgress{ReplicaIndex: 0},
		},
		{
			name:                    "replica that was not available is done once it is complete",
			pcs:                     withCurrentlyRecreating(newGangShapeTestPCS(2), 0, false),
			states:                  statesOf(notReadyState(0, false), staleBreachedState(1)),
			wantRecreate:            ptr.To(1),
			wantStale:               []int{},
			wantCurrentlyRecreating: &grovecorev1alpha1.PodCliqueSetReplicaRecreateProgress{ReplicaIndex: 1},
		},
		{
			name:                    "replica that is gang-terminated while it recovers is still waited for",
			pcs:                     withCurrentlyRecreating(newGangShapeTestPCS(2), 0, false),
			states:                  statesOf(replicaGangShapeState{index: 0, excluded: true}, staleBreachedState(1)),
			wantStale:               []int{1},
			wantCurrentlyRecreating: &grovecorev1alpha1.PodCliqueSetReplicaRecreateProgress{ReplicaIndex: 0},
		},
		{
			name:                    "replica that is stale again is recreated again and keeps its availability",
			pcs:                     withCurrentlyRecreating(newGangShapeTestPCS(2), 1, true),
			states:                  statesOf(staleBreachedState(0), notReadyState(1, true)),
			wantRecreate:            ptr.To(1),
			wantStale:               []int{0},
			wantCurrentlyRecreating: &grovecorev1alpha1.PodCliqueSetReplicaRecreateProgress{ReplicaIndex: 1, WasAvailable: true},
		},
		{
			name:                    "replica that is no longer expected is dropped",
			pcs:                     withCurrentlyRecreating(newGangShapeTestPCS(2), 5, true),
			states:                  statesOf(availableState(0, true), availableState(1, true)),
			wantRecreate:            ptr.To(0),
			wantStale:               []int{1},
			wantCurrentlyRecreating: &grovecorev1alpha1.PodCliqueSetReplicaRecreateProgress{ReplicaIndex: 0, WasAvailable: true},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			var recreated []int
			work := computeGangShapeWork(logr.Discard(), tc.pcs, lo.Range(int(tc.pcs.Spec.Replicas)), tc.states, recordingTaskFn(&recreated))
			assertGangShapeWork(t, work, recreated, tc.wantRecreate, tc.wantStale, tc.wantCurrentlyRecreating)
			assert.True(t, work.inProgress())
		})
	}

	t.Run("re-recreated replica keeps the time of its first recreate", func(t *testing.T) {
		pcs := withCurrentlyRecreating(newGangShapeTestPCS(1), 0, true)
		work := computeGangShapeWork(logr.Discard(), pcs, []int{0}, statesOf(notReadyState(0, true)), recordingTaskFn(new([]int)))
		require.NotNil(t, work.currentlyRecreating)
		assert.Equal(t, pcs.Status.GangShapeRecreate.CurrentlyRecreating.StartedAt, work.currentlyRecreating.StartedAt)
	})

	t.Run("recovered replica and no stale replica left ends the recreate", func(t *testing.T) {
		pcs := withCurrentlyRecreating(newGangShapeTestPCS(2), 1, true)
		var recreated []int
		work := computeGangShapeWork(logr.Discard(), pcs, []int{0, 1}, statesOf(availableState(0, false), availableState(1, false)), recordingTaskFn(&recreated))
		assert.Empty(t, recreated)
		assert.False(t, work.inProgress())
	})
}

// TestGangShapeRecreateKeepsAReplicaAvailable walks a two-replica PodCliqueSet through its recreate and checks
// that one replica is available at every step, including when the new gang shape is slow to become available.
func TestGangShapeRecreateKeepsAReplicaAvailable(t *testing.T) {
	pcs := newGangShapeTestPCS(2)
	expected := []int{0, 1}
	step := func(states map[int]replicaGangShapeState) (*gangShapeWork, []int) {
		var recreated []int
		work := computeGangShapeWork(logr.Discard(), pcs, expected, states, recordingTaskFn(&recreated))
		mutateGangShapeRecreateStatus(pcs, work, metav1.Now())
		return work, recreated
	}

	// Both replicas are stale and available: replica 0 is the canary.
	_, recreated := step(statesOf(availableState(0, true), availableState(1, true)))
	assert.Equal(t, []int{0}, recreated)

	// Replica 0 is rebuilt, then its pods start. Replica 1 is not deleted meanwhile.
	for _, state0 := range []replicaGangShapeState{incompleteState(0, false), notReadyState(0, false)} {
		work, recreated := step(statesOf(state0, availableState(1, true)))
		assert.Empty(t, recreated)
		assert.Equal(t, apiconstants.ConditionReasonRecreatingReplica, meta.FindStatusCondition(pcs.Status.Conditions, apiconstants.ConditionTypeGangShapeRecreateInProgress).Reason)
		assert.True(t, work.inProgress())
	}

	// Replica 0 is available: replica 1 is deleted.
	_, recreated = step(statesOf(availableState(0, false), availableState(1, true)))
	assert.Equal(t, []int{1}, recreated)

	// Replica 1 is available again: the recreate is done.
	work, recreated := step(statesOf(availableState(0, false), availableState(1, false)))
	assert.Empty(t, recreated)
	assert.False(t, work.inProgress())
	assert.Nil(t, pcs.Status.GangShapeRecreate)
	condition := meta.FindStatusCondition(pcs.Status.Conditions, apiconstants.ConditionTypeGangShapeRecreateInProgress)
	require.NotNil(t, condition)
	assert.Equal(t, metav1.ConditionFalse, condition.Status)
	assert.Equal(t, apiconstants.ConditionReasonGangShapeCurrent, condition.Reason)
}

func assertGangShapeWork(t *testing.T, work *gangShapeWork, recreated []int, wantRecreate *int, wantStale []int, wantCurrentlyRecreating *grovecorev1alpha1.PodCliqueSetReplicaRecreateProgress) {
	t.Helper()
	if wantRecreate == nil {
		assert.Empty(t, recreated)
		assert.Nil(t, work.recreateTask)
	} else {
		assert.Equal(t, []int{*wantRecreate}, recreated)
		assert.NotNil(t, work.recreateTask)
	}
	if len(wantStale) == 0 {
		assert.Empty(t, work.staleReplicaIndices)
	} else {
		assert.Equal(t, wantStale, work.staleReplicaIndices)
	}
	if wantCurrentlyRecreating == nil {
		assert.Nil(t, work.currentlyRecreating)
		return
	}
	require.NotNil(t, work.currentlyRecreating)
	assert.Equal(t, wantCurrentlyRecreating.ReplicaIndex, work.currentlyRecreating.ReplicaIndex)
	assert.Equal(t, wantCurrentlyRecreating.WasAvailable, work.currentlyRecreating.WasAvailable)
	assert.False(t, work.currentlyRecreating.StartedAt.IsZero())
}

func TestMutateGangShapeRecreateStatus(t *testing.T) {
	now := metav1.Now()

	t.Run("no recreate and none before leaves the status alone", func(t *testing.T) {
		pcs := newGangShapeTestPCS(1)
		mutateGangShapeRecreateStatus(pcs, &gangShapeWork{}, now)
		assert.Nil(t, pcs.Status.GangShapeRecreate)
		assert.Empty(t, pcs.Status.Conditions)
	})

	t.Run("recreating a replica", func(t *testing.T) {
		pcs := newGangShapeTestPCS(3)
		work := &gangShapeWork{
			staleReplicaIndices: []int{2},
			currentlyRecreating: &grovecorev1alpha1.PodCliqueSetReplicaRecreateProgress{ReplicaIndex: 1, WasAvailable: true, StartedAt: now},
		}
		mutateGangShapeRecreateStatus(pcs, work, now)
		require.NotNil(t, pcs.Status.GangShapeRecreate)
		assert.Equal(t, now, pcs.Status.GangShapeRecreate.StartedAt)
		assert.Equal(t, work.currentlyRecreating, pcs.Status.GangShapeRecreate.CurrentlyRecreating)
		condition := meta.FindStatusCondition(pcs.Status.Conditions, apiconstants.ConditionTypeGangShapeRecreateInProgress)
		require.NotNil(t, condition)
		assert.Equal(t, metav1.ConditionTrue, condition.Status)
		assert.Equal(t, apiconstants.ConditionReasonRecreatingReplica, condition.Reason)
		assert.Contains(t, condition.Message, "replica 1")
		assert.Contains(t, condition.Message, "available")
	})

	t.Run("keeps the time the recreate started", func(t *testing.T) {
		pcs := withCurrentlyRecreating(newGangShapeTestPCS(2), 0, true)
		startedAt := pcs.Status.GangShapeRecreate.StartedAt
		mutateGangShapeRecreateStatus(pcs, &gangShapeWork{staleReplicaIndices: []int{1}, waitingForNewShape: true}, now)
		assert.Equal(t, startedAt, pcs.Status.GangShapeRecreate.StartedAt)
		assert.Nil(t, pcs.Status.GangShapeRecreate.CurrentlyRecreating)
		condition := meta.FindStatusCondition(pcs.Status.Conditions, apiconstants.ConditionTypeGangShapeRecreateInProgress)
		require.NotNil(t, condition)
		assert.Equal(t, apiconstants.ConditionReasonWaitingForNewShapeAvailable, condition.Reason)
	})

	t.Run("cancelling the update clears the replica under update", func(t *testing.T) {
		pcs := withUpdatingReplica(newGangShapeTestPCS(2), grovecorev1alpha1.RollingRecreateStrategy, 1)
		mutateGangShapeRecreateStatus(pcs, &gangShapeWork{
			currentlyRecreating: &grovecorev1alpha1.PodCliqueSetReplicaRecreateProgress{ReplicaIndex: 1, StartedAt: now},
			cancelUpdate:        true,
		}, now)
		require.NotNil(t, pcs.Status.UpdateProgress)
		assert.Empty(t, pcs.Status.UpdateProgress.CurrentlyUpdating)
	})

	t.Run("recreate done", func(t *testing.T) {
		pcs := withCurrentlyRecreating(newGangShapeTestPCS(2), 1, true)
		mutateGangShapeRecreateStatus(pcs, &gangShapeWork{}, now)
		assert.Nil(t, pcs.Status.GangShapeRecreate)
		condition := meta.FindStatusCondition(pcs.Status.Conditions, apiconstants.ConditionTypeGangShapeRecreateInProgress)
		require.NotNil(t, condition)
		assert.Equal(t, metav1.ConditionFalse, condition.Status)
		assert.Equal(t, apiconstants.ConditionReasonGangShapeCurrent, condition.Reason)
	})
}

// TestSyncRecreatesStaleReplica checks that Sync deletes the PodCliques of a stale replica, resets the
// minAvailable of its PCSG, records the replica in the status, cancels its rolling update, emits a
// GangShapeChanged event, and requeues.
func TestSyncRecreatesStaleReplica(t *testing.T) {
	scheme := runtime.NewScheme()
	require.NoError(t, grovecorev1alpha1.AddToScheme(scheme))

	pcs := withUpdatingReplica(newGangShapeTestPCS(2), grovecorev1alpha1.RollingRecreateStrategy, 1)
	pcs.Status.UpdateProgress.UpdateStartedAt = metav1.NewTime(time.Now().Add(-time.Minute).Truncate(time.Second))
	pcs.Spec.Template.PodCliqueScalingGroupConfigs[0].MinAvailable = ptr.To[int32](2)

	pcs.UID = "pcs-uid"

	objects := []client.Object{pcs}
	for _, pcsReplicaIndex := range []int{0, 1} {
		pclq := newStandalonePCLQ(pcsReplicaIndex, 2, 2)
		member := newPCSGMemberPCLQ(pcsReplicaIndex, 1)
		pcsg := newPCSG(pcsReplicaIndex, 1, 2)
		pcsg.UID = types.UID(pcsg.Name + "-uid")
		pclq.OwnerReferences = []metav1.OwnerReference{*metav1.NewControllerRef(pcs, grovecorev1alpha1.SchemeGroupVersion.WithKind(apiconstants.KindPodCliqueSet))}
		pcsg.OwnerReferences = []metav1.OwnerReference{*metav1.NewControllerRef(pcs, grovecorev1alpha1.SchemeGroupVersion.WithKind(apiconstants.KindPodCliqueSet))}
		member.OwnerReferences = []metav1.OwnerReference{*metav1.NewControllerRef(&pcsg, grovecorev1alpha1.SchemeGroupVersion.WithKind(apiconstants.KindPodCliqueScalingGroup))}
		objects = append(objects, &pclq, &member, &pcsg)
	}
	cl := fake.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(objects...).
		WithStatusSubresource(&grovecorev1alpha1.PodCliqueSet{}, &grovecorev1alpha1.PodCliqueScalingGroup{}).
		Build()
	recorder := record.NewFakeRecorder(10)
	r := _resource{client: cl, eventRecorder: recorder}

	ctx := context.Background()
	latest := &grovecorev1alpha1.PodCliqueSet{}
	require.NoError(t, cl.Get(ctx, client.ObjectKeyFromObject(pcs), latest))
	err := r.Sync(ctx, logr.Discard(), latest)
	var groveErr *groveerr.GroveError
	require.True(t, errors.As(err, &groveErr), "expected a GroveError, got %v", err)
	assert.Equal(t, groveerr.ErrCodeContinueReconcileAndRequeue, groveErr.Code)

	// Replica 1 is under the rolling update, and both are available, so replica 1 is recreated first.
	for pcsReplicaIndex, wantPCLQs := range map[int]int{0: 2, 1: 0} {
		pclqs := &grovecorev1alpha1.PodCliqueList{}
		require.NoError(t, cl.List(ctx, pclqs, client.InNamespace(testGangShapeNamespace), client.MatchingLabels(replicaLabels(pcsReplicaIndex))))
		assert.Len(t, pclqs.Items, wantPCLQs, "PodCliques of replica %d", pcsReplicaIndex)

		pcsg := &grovecorev1alpha1.PodCliqueScalingGroup{}
		pcsgTemplate := newPCSG(pcsReplicaIndex, 0, 0)
		require.NoError(t, cl.Get(ctx, client.ObjectKeyFromObject(&pcsgTemplate), pcsg))
		wantMinAvailable := map[int]int32{0: 1, 1: 2}[pcsReplicaIndex]
		assert.Equal(t, wantMinAvailable, *pcsg.Spec.MinAvailable, "PCSG minAvailable of replica %d", pcsReplicaIndex)
	}

	require.NoError(t, cl.Get(ctx, client.ObjectKeyFromObject(pcs), latest))
	require.NotNil(t, latest.Status.GangShapeRecreate)
	require.NotNil(t, latest.Status.GangShapeRecreate.CurrentlyRecreating)
	assert.Equal(t, int32(1), latest.Status.GangShapeRecreate.CurrentlyRecreating.ReplicaIndex)
	assert.True(t, latest.Status.GangShapeRecreate.CurrentlyRecreating.WasAvailable)
	require.NotNil(t, latest.Status.UpdateProgress)
	assert.Empty(t, latest.Status.UpdateProgress.CurrentlyUpdating)
	assert.True(t, meta.IsStatusConditionTrue(latest.Status.Conditions, apiconstants.ConditionTypeGangShapeRecreateInProgress))

	require.Len(t, recorder.Events, 1)
	assert.Contains(t, <-recorder.Events, constants.ReasonGangShapeChanged)
}
