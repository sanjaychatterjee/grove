//go:build e2e

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

package minavailable

import (
	"fmt"
	"strings"
	"testing"
	"time"

	apiconstants "github.com/ai-dynamo/grove/operator/api/common/constants"
	grovev1alpha1 "github.com/ai-dynamo/grove/operator/api/core/v1alpha1"
	"github.com/ai-dynamo/grove/operator/e2e/tests"

	"github.com/stretchr/testify/require"
)

// newWorkerPCS returns a PodCliqueSet whose worker clique has replicas 2 and minAvailable 1. It runs 2 pods per
// replica.
func newWorkerPCS(name string, replicas int32, strategy grovev1alpha1.UpdateStrategyType) *grovev1alpha1.PodCliqueSet {
	return newPCS(name, replicas, strategy, []cliqueSpec{{name: workerClique, replicas: 2, minAvailable: 1}})
}

// Test_MA1_RaisingCliqueMinAvailableRecreatesReplicasOneAtATime verifies that raising the MinAvailable of a
// standalone clique recreates every replica exactly once, one at a time and in index order, and that the new gang
// shape reaches the PodCliques and the PodGangs.
func Test_MA1_RaisingCliqueMinAvailableRecreatesReplicasOneAtATime(t *testing.T) {
	const pcsName = "ma1"
	tc, cleanup := prepare(t, 10, pcsName)
	defer cleanup()
	deploy(t, tc, newWorkerPCS(pcsName, 3, grovev1alpha1.RollingRecreateStrategy))
	uidsBefore := workerUIDs(t, tc, pcsName, 3)

	tests.Logger.Info("1. Raise the worker MinAvailable from 1 to 2")
	observer := observeRecreate(tc, pcsName)
	requireAccepted(t, tc, pcsName, "worker minAvailable 2", setCliqueMinAvailable(workerClique, 2))
	waitForGangShapeCurrent(t, tc, pcsName)
	observer.stop()

	tests.Logger.Info("2. Every replica was recreated once, in index order, and never more than one was unavailable")
	requireAllChanged(t, uidsBefore, workerUIDs(t, tc, pcsName, 3))
	require.Equal(t, onceEach(0, 1, 2), observer.workerDeletions(3), "each replica must be deleted once")
	require.Equal(t, []int{0, 1, 2}, observer.order, "replicas must be recreated in index order")
	require.LessOrEqual(t, observer.maxUnavailable, 1, "at most one replica may be unavailable at a time")

	tests.Logger.Info("3. The recreated PodCliques and their PodGangs carry MinAvailable 2")
	for i := range 3 {
		require.Equal(t, int32(2), *getPCLQ(t, tc, pclqName(pcsName, i, workerClique)).Spec.MinAvailable)
	}
	requirePodGroupMinReplicas(t, tc, pcsName, workerClique, 2)
}

// Test_MA2_LoweringScalingGroupMinAvailableRecreatesReplicas verifies that lowering the MinAvailable of a
// PodCliqueScalingGroup recreates every replica once, one at a time, and that the PodCliqueScalingGroups carry
// the new value.
func Test_MA2_LoweringScalingGroupMinAvailableRecreatesReplicas(t *testing.T) {
	const pcsName = "ma2"
	tc, cleanup := prepare(t, 10, pcsName)
	defer cleanup()
	deploy(t, tc, newPCS(pcsName, 2, grovev1alpha1.RollingRecreateStrategy,
		[]cliqueSpec{
			{name: workerClique, replicas: 1, minAvailable: 1},
			{name: memberClique, replicas: 1, minAvailable: 1},
		},
		scalingGroupSpec{name: scalingGroup, cliqueNames: []string{memberClique}, replicas: 2, minAvailable: 2},
	))
	uidsBefore := workerUIDs(t, tc, pcsName, 2)

	tests.Logger.Info("1. Lower the scaling group MinAvailable from 2 to 1")
	observer := observeRecreate(tc, pcsName)
	requireAccepted(t, tc, pcsName, "scaling group minAvailable 1", setGroupMinAvailable(scalingGroup, 1))
	waitForGangShapeCurrent(t, tc, pcsName)
	observer.stop()

	tests.Logger.Info("2. Every replica was recreated once and never more than one was unavailable")
	requireAllChanged(t, uidsBefore, workerUIDs(t, tc, pcsName, 2))
	require.Equal(t, onceEach(0, 1), observer.workerDeletions(2))
	require.LessOrEqual(t, observer.maxUnavailable, 1, "at most one replica may be unavailable at a time")
	for i := range 2 {
		require.Equal(t, int32(1), *getPCSG(t, tc, pcsgName(pcsName, i, scalingGroup)).Spec.MinAvailable)
	}
}

// Test_MA3_RecreateUnderEveryUpdateStrategy verifies that a MinAvailable change recreates the replicas under every
// update strategy, including OnDelete.
func Test_MA3_RecreateUnderEveryUpdateStrategy(t *testing.T) {
	for _, strategy := range []grovev1alpha1.UpdateStrategyType{
		grovev1alpha1.RollingRecreateStrategy,
		grovev1alpha1.CoherentStrategy,
		grovev1alpha1.OnDeleteStrategy,
	} {
		t.Run(string(strategy), func(t *testing.T) {
			pcsName := "ma3-" + strings.ToLower(string(strategy))
			tc, cleanup := prepare(t, 8, pcsName)
			defer cleanup()
			deploy(t, tc, newWorkerPCS(pcsName, 2, strategy))
			uidsBefore := workerUIDs(t, tc, pcsName, 2)

			observer := observeRecreate(tc, pcsName)
			requireAccepted(t, tc, pcsName, "worker minAvailable 2", setCliqueMinAvailable(workerClique, 2))
			waitForGangShapeCurrent(t, tc, pcsName)
			observer.stop()

			requireAllChanged(t, uidsBefore, workerUIDs(t, tc, pcsName, 2))
			require.Equal(t, onceEach(0, 1), observer.workerDeletions(2))
		})
	}
}

// Test_MA4_RecreateOrderAndPacing verifies that a replica that breached MinAvailable is recreated first, that an
// available replica is only deleted once a replica with the new gang shape is available, and that the rest follow
// in index order.
func Test_MA4_RecreateOrderAndPacing(t *testing.T) {
	const pcsName = "ma4"
	tc, cleanup := prepare(t, 14, pcsName)
	defer cleanup()
	deploy(t, tc, newWorkerPCS(pcsName, 4, grovev1alpha1.RollingRecreateStrategy))

	tests.Logger.Info("1. Make replica 2 breach MinAvailable by crash-looping its pods")
	crashLoop := crashLoopStage(pcsName, 2)
	crashLoop.apply(t, tc)
	deletePods(t, tc, livePodsOfReplica(t, tc, pcsName, 2)...)
	waitForPCLQ(t, tc, pclqName(pcsName, 2, workerClique), "MinAvailableBreached", isBreached)
	uidsBefore := workerUIDs(t, tc, pcsName, 4)

	tests.Logger.Info("2. Raise the worker MinAvailable: replica 2 goes first")
	observer := observeRecreate(tc, pcsName)
	requireAccepted(t, tc, pcsName, "worker minAvailable 2", setCliqueMinAvailable(workerClique, 2))

	tests.Logger.Info("3. While recreated replica 2 cannot become available, no available replica is deleted")
	waitForPCS(t, tc, pcsName, "reason WaitingForNewShapeAvailable", func(pcs *grovev1alpha1.PodCliqueSet) bool {
		cond := gangShapeCondition(pcs)
		return cond != nil && cond.Reason == apiconstants.ConditionReasonWaitingForNewShapeAvailable
	})
	requireStable(t, 20*time.Second, "no other replica may be deleted", func() error {
		uids := workerUIDs(t, tc, pcsName, 4)
		for _, i := range []int{0, 1, 3} {
			if uids[i] != uidsBefore[i] {
				return fmt.Errorf("replica %d was deleted while no replica with the new gang shape was available", i)
			}
		}
		return nil
	})

	tests.Logger.Info("4. Release replica 2: the others are recreated in index order")
	crashLoop.release(t, tc)
	waitForGangShapeCurrent(t, tc, pcsName)
	observer.stop()
	require.Equal(t, []int{2, 0, 1, 3}, observer.order)
	require.Equal(t, onceEach(0, 1, 2, 3), observer.workerDeletions(4))
	require.LessOrEqual(t, observer.maxUnavailable, 1, "at most one replica may be unavailable at a time")
	requireAllChanged(t, uidsBefore, workerUIDs(t, tc, pcsName, 4))
}

// Test_MA5_MinAvailableChangedDuringRollingUpdate verifies that a MinAvailable change during a rolling update
// cancels the update of the replica under update, recreates that replica first, recreates the others once each,
// and that the update then finishes with every pod on the new pod template.
func Test_MA5_MinAvailableChangedDuringRollingUpdate(t *testing.T) {
	for _, strategy := range []grovev1alpha1.UpdateStrategyType{grovev1alpha1.RollingRecreateStrategy, grovev1alpha1.CoherentStrategy} {
		t.Run(string(strategy), func(t *testing.T) {
			pcsName := "ma5-" + strings.ToLower(string(strategy))
			tc, cleanup := prepare(t, 10, pcsName)
			defer cleanup()
			deploy(t, tc, newWorkerPCS(pcsName, 3, strategy))

			tests.Logger.Info("1. Start a rolling update and hold it on its first replica by keeping new pods NotReady")
			hold := holdStage(pcsName, 0, 1, 2)
			hold.apply(t, tc)
			revision := newRevision()
			requireAccepted(t, tc, pcsName, "pod template change", changePodTemplate(revision, workerClique))
			pcs := waitForPCS(t, tc, pcsName, "a replica to be under update", func(pcs *grovev1alpha1.PodCliqueSet) bool {
				return pcs.Status.UpdateProgress != nil && len(pcs.Status.UpdateProgress.CurrentlyUpdating) > 0
			})
			updating := int(pcs.Status.UpdateProgress.CurrentlyUpdating[0].ReplicaIndex)

			tests.Logger.Info("2. Raise the worker MinAvailable: the replica under update is recreated first and its update is cancelled")
			observer := observeRecreate(tc, pcsName)
			requireAccepted(t, tc, pcsName, "worker minAvailable 2", setCliqueMinAvailable(workerClique, 2))
			pcs = waitForPCS(t, tc, pcsName, "the replica under update to be recreated", func(pcs *grovev1alpha1.PodCliqueSet) bool {
				index := currentlyRecreatingIndex(pcs)
				return index != nil && *index == updating
			})
			require.True(t, pcs.Status.UpdateProgress == nil || len(pcs.Status.UpdateProgress.CurrentlyUpdating) == 0,
				"the update of replica %d must be cancelled", updating)

			tests.Logger.Info("3. Release the pods: every replica is recreated once and the update finishes")
			hold.release(t, tc)
			waitForGangShapeCurrent(t, tc, pcsName)
			waitForRollingUpdateComplete(t, tc, pcsName, revision)
			observer.stop()
			require.NotEmpty(t, observer.order)
			require.Equal(t, updating, observer.order[0], "the replica under update must be recreated first")
			require.Equal(t, onceEach(0, 1, 2), observer.workerDeletions(3), "each replica must be deleted once")
		})
	}
}
