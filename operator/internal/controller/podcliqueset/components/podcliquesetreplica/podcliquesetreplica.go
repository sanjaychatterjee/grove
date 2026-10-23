// Copyright 2025 The Grove Authors.
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
	"maps"
	"slices"

	grovecorev1alpha1 "github.com/ai-dynamo/grove/operator/api/core/v1alpha1"
	"github.com/ai-dynamo/grove/operator/internal/controller/common/component"
	groveerr "github.com/ai-dynamo/grove/operator/internal/errors"
	"github.com/ai-dynamo/grove/operator/internal/utils"
	componentutils "github.com/ai-dynamo/grove/operator/internal/utils/component"

	"github.com/go-logr/logr"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/tools/record"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

const (
	errCodeComputingPCSReplicaDeletionWork grovecorev1alpha1.ErrorCode = "ERR_COMPUTE_PCS_REPLICA_DELETION_WORK"
	errCodeComputingGangShapeWork          grovecorev1alpha1.ErrorCode = "ERR_COMPUTE_GANG_SHAPE_WORK"
	errCodeDeletePCSReplica                grovecorev1alpha1.ErrorCode = "ERR_DELETE_PCS_REPLICA"
	errCodeListPCLQs                       grovecorev1alpha1.ErrorCode = "ERR_LIST_PCLQs"
	errCodeListPCSGs                       grovecorev1alpha1.ErrorCode = "ERR_LIST_PCGS"
	errCodeUpdatePCSStatus                 grovecorev1alpha1.ErrorCode = "ERR_UPDATE_PCS_STATUS"
	errCodeGetPodGangMap                   grovecorev1alpha1.ErrorCode = "ERR_GET_PODGANGMAP"
	errCodeInvalidEpoch                    grovecorev1alpha1.ErrorCode = "ERR_INVALID_EPOCH"
)

type _resource struct {
	client        client.Client
	eventRecorder record.EventRecorder
}

// New creates a new instance of the PodCliqueSetReplica operator.
func New(client client.Client, eventRecorder record.EventRecorder) component.Operator[grovecorev1alpha1.PodCliqueSet] {
	return &_resource{
		client:        client,
		eventRecorder: eventRecorder,
	}
}

// GetExistingResourceNames is a no-op.
func (r _resource) GetExistingResourceNames(_ context.Context, _ logr.Logger, _ metav1.ObjectMeta) ([]string, error) {
	return []string{}, nil
}

// Sync orchestrates replica deletion, the recreate of replicas with a stale gang shape, and rolling updates for the
// PodCliqueSet.
func (r _resource) Sync(ctx context.Context, logger logr.Logger, pcs *grovecorev1alpha1.PodCliqueSet) error {
	pcsObjectKey := client.ObjectKeyFromObject(pcs)

	delWork, err := r.getPCSReplicaDeletionWork(ctx, logger, pcs)
	if err != nil {
		return groveerr.WrapError(err,
			errCodeComputingPCSReplicaDeletionWork,
			component.OperationSync,
			fmt.Sprintf("Could not compute pending replica deletion delWork for PCS: %v", pcsObjectKey))
	}

	if delWork.hasPendingPCSReplicaDeletion() {
		if runResult := utils.RunConcurrently(ctx, logger, delWork.deletionTasks); runResult.HasErrors() {
			return groveerr.WrapError(runResult.GetAggregatedError(),
				errCodeDeletePCSReplica,
				component.OperationSync,
				fmt.Sprintf("Error deleting PodCliques for PodCliqueSet: %v", pcsObjectKey),
			)
		}
	}

	gangShapeWork, err := r.getGangShapeWork(ctx, logger, pcs, delWork.pcsIndicesToTerminate)
	if err != nil {
		return groveerr.WrapError(err,
			errCodeComputingGangShapeWork,
			component.OperationSync,
			fmt.Sprintf("Could not compute gang-shape recreate work for PCS: %v", pcsObjectKey))
	}
	if err = r.patchGangShapeRecreateStatus(ctx, logger, pcs, gangShapeWork); err != nil {
		return err
	}
	if gangShapeWork.recreateTask != nil {
		if err = gangShapeWork.recreateTask.Fn(ctx); err != nil {
			return groveerr.WrapError(err,
				errCodeDeletePCSReplica,
				component.OperationSync,
				fmt.Sprintf("Error recreating a replica with a stale gang shape for PodCliqueSet: %v", pcsObjectKey),
			)
		}
	}
	// A rolling update waits while replicas are recreated with the current gang shape. The recreated replicas
	// get the current pod templates as well, so the update has less left to do once the recreate is done.
	if gangShapeWork.inProgress() {
		return groveerr.New(groveerr.ErrCodeContinueReconcileAndRequeue,
			component.OperationSync,
			"Requeuing to continue recreating PodCliqueSet replicas with a stale gang shape",
		)
	}

	// Orchestrate the rolling update (RollingRecreate or Coherent) while one is in progress.
	if componentutils.IsRollingUpdateInProgress(pcs) {
		minAvailableBreachedPCSReplicaIndices := slices.Collect(maps.Keys(delWork.minAvailableBreachedConstituents))
		if err := r.orchestrateRollingUpdate(ctx, logger, pcs, delWork.pcsIndicesToTerminate, minAvailableBreachedPCSReplicaIndices); err != nil {
			return err
		}
	}

	if delWork.shouldRequeue() {
		return groveerr.New(groveerr.ErrCodeContinueReconcileAndRequeue,
			component.OperationSync,
			"Requeuing to re-process PCS replicas that have breached MinAvailable but not crossed TerminationDelay",
		)
	}
	return nil
}

// Delete is a no-op
func (r _resource) Delete(_ context.Context, _ logr.Logger, _ metav1.ObjectMeta) error {
	return nil
}
