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
	"context"
	"fmt"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/ai-dynamo/grove/operator/api/common"
	apiconstants "github.com/ai-dynamo/grove/operator/api/common/constants"
	grovev1alpha1 "github.com/ai-dynamo/grove/operator/api/core/v1alpha1"
	"github.com/ai-dynamo/grove/operator/e2e/grove/podgang"
	"github.com/ai-dynamo/grove/operator/e2e/k8s/kwok"
	"github.com/ai-dynamo/grove/operator/e2e/setup"
	"github.com/ai-dynamo/grove/operator/e2e/testctx"
	"github.com/ai-dynamo/grove/operator/e2e/tests"

	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/sets"
	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/client-go/util/retry"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

const (
	testNamespace = "default"
	workerClique  = "worker"
	memberClique  = "member"
	scalingGroup  = "sg"
	// recreateTimeout bounds a wait for a recreate or a rolling update to finish.
	recreateTimeout = 5 * time.Minute
	pollInterval    = 1 * time.Second
	// sampleInterval is how often an observer samples state. It is well below the time a replica takes to come
	// back, so no step of a recreate is missed.
	sampleInterval = 500 * time.Millisecond
	// holdDelay keeps a pod NotReady for longer than any test runs.
	holdDelay = time.Hour
	// terminationDelay keeps gang termination out of every test.
	terminationDelay = time.Hour
	// updateTriggerEnv is the container env var that a pod template change sets.
	updateTriggerEnv = "UPDATE_TRIGGER"
)

// cliqueSpec describes a clique template of a test PodCliqueSet.
type cliqueSpec struct {
	name         string
	replicas     int32
	minAvailable int32
}

// scalingGroupSpec describes a PodCliqueScalingGroup config of a test PodCliqueSet.
type scalingGroupSpec struct {
	name         string
	cliqueNames  []string
	replicas     int32
	minAvailable int32
}

// newPCS returns a PodCliqueSet whose pods each fill one KWOK node.
func newPCS(name string, replicas int32, strategy grovev1alpha1.UpdateStrategyType, cliques []cliqueSpec, groups ...scalingGroupSpec) *grovev1alpha1.PodCliqueSet {
	pcs := &grovev1alpha1.PodCliqueSet{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: testNamespace},
		Spec: grovev1alpha1.PodCliqueSetSpec{
			Replicas:       replicas,
			UpdateStrategy: &grovev1alpha1.PodCliqueSetUpdateStrategy{Type: strategy},
			Template: grovev1alpha1.PodCliqueSetTemplateSpec{
				TerminationDelay: &metav1.Duration{Duration: terminationDelay},
			},
		},
	}
	for _, c := range cliques {
		pcs.Spec.Template.Cliques = append(pcs.Spec.Template.Cliques, &grovev1alpha1.PodCliqueTemplateSpec{
			Name:   c.name,
			Labels: map[string]string{"kai.scheduler/queue": "test"},
			Spec: grovev1alpha1.PodCliqueSpec{
				RoleName:     c.name,
				Replicas:     c.replicas,
				MinAvailable: ptr.To(c.minAvailable),
				PodSpec:      newPodSpec(c.name),
			},
		})
	}
	for _, g := range groups {
		pcs.Spec.Template.PodCliqueScalingGroupConfigs = append(pcs.Spec.Template.PodCliqueScalingGroupConfigs, grovev1alpha1.PodCliqueScalingGroupConfig{
			Name:         g.name,
			CliqueNames:  g.cliqueNames,
			Replicas:     ptr.To(g.replicas),
			MinAvailable: ptr.To(g.minAvailable),
		})
	}
	return pcs
}

func newPodSpec(containerName string) corev1.PodSpec {
	return corev1.PodSpec{
		TerminationGracePeriodSeconds: ptr.To[int64](0),
		SchedulerName:                 "kai-scheduler",
		Affinity: &corev1.Affinity{NodeAffinity: &corev1.NodeAffinity{
			RequiredDuringSchedulingIgnoredDuringExecution: &corev1.NodeSelector{NodeSelectorTerms: []corev1.NodeSelectorTerm{{
				MatchExpressions: []corev1.NodeSelectorRequirement{{
					Key:      setup.WorkerNodeLabelKey,
					Operator: corev1.NodeSelectorOpIn,
					Values:   []string{setup.WorkerNodeLabelValue},
				}},
			}}},
		}},
		Tolerations: []corev1.Toleration{{
			Key:      setup.WorkerNodeLabelKey,
			Operator: corev1.TolerationOpEqual,
			Value:    setup.WorkerNodeLabelValue,
			Effect:   corev1.TaintEffectNoSchedule,
		}},
		Containers: []corev1.Container{{
			Name:      containerName,
			Image:     "registry:5001/busybox:latest",
			Command:   []string{"sleep", "infinity"},
			Resources: corev1.ResourceRequirements{Requests: corev1.ResourceList{corev1.ResourceMemory: resource.MustParse("80Mi")}},
		}},
	}
}

// expectedPods returns the number of pods pcs runs.
func expectedPods(pcs *grovev1alpha1.PodCliqueSet) int {
	memberOf := map[string]int32{}
	for _, g := range pcs.Spec.Template.PodCliqueScalingGroupConfigs {
		for _, name := range g.CliqueNames {
			memberOf[name] = ptr.Deref(g.Replicas, 1)
		}
	}
	perReplica := 0
	for _, c := range pcs.Spec.Template.Cliques {
		groupReplicas, ok := memberOf[c.Name]
		if !ok {
			groupReplicas = 1
		}
		perReplica += int(c.Spec.Replicas * groupReplicas)
	}
	return int(pcs.Spec.Replicas) * perReplica
}

// prepare prepares the cluster with workerNodes schedulable nodes for a test of the PodCliqueSet pcsName.
func prepare(t *testing.T, workerNodes int, pcsName string) (*testctx.TestContext, func()) {
	t.Helper()
	return testctx.PrepareTest(t.Context(), t, workerNodes,
		testctx.WithWorkload(&testctx.WorkloadConfig{Name: pcsName, Namespace: testNamespace}),
		testctx.WithTimeout(recreateTimeout),
		testctx.WithInterval(pollInterval),
	)
}

// deploy creates pcs and waits until all of its pods are ready.
func deploy(t *testing.T, tc *testctx.TestContext, pcs *grovev1alpha1.PodCliqueSet) {
	t.Helper()
	pods := expectedPods(pcs)
	require.NoError(t, tc.Client.Create(tc.Ctx, pcs), "creating PodCliqueSet %s", pcs.Name)
	waitForReadyPods(t, tc, pcs.Name, pods)
}

// listLivePods returns the pods of the PodCliqueSet pcsName that are not being deleted.
func listLivePods(ctx context.Context, tc *testctx.TestContext, pcsName string) ([]corev1.Pod, error) {
	podList := &corev1.PodList{}
	if err := tc.Client.List(ctx, podList, client.InNamespace(testNamespace), client.MatchingLabels{common.LabelPartOfKey: pcsName}); err != nil {
		return nil, err
	}
	return slices.DeleteFunc(podList.Items, func(pod corev1.Pod) bool { return pod.DeletionTimestamp != nil }), nil
}

// waitForReadyPods waits until the PodCliqueSet pcsName has exactly count live pods and all of them are ready.
func waitForReadyPods(t *testing.T, tc *testctx.TestContext, pcsName string, count int) {
	t.Helper()
	err := wait.PollUntilContextTimeout(tc.Ctx, pollInterval, recreateTimeout, true, func(ctx context.Context) (bool, error) {
		pods, err := listLivePods(ctx, tc, pcsName)
		if err != nil {
			return false, err
		}
		return len(pods) == count && !slices.ContainsFunc(pods, func(pod corev1.Pod) bool { return !isPodReady(&pod) }), nil
	})
	require.NoError(t, err, "%s did not reach %d ready pods", pcsName, count)
}

func isPodReady(pod *corev1.Pod) bool {
	for _, cond := range pod.Status.Conditions {
		if cond.Type == corev1.PodReady {
			return cond.Status == corev1.ConditionTrue
		}
	}
	return false
}

// pcsMutation changes a PodCliqueSet before it is updated.
type pcsMutation func(pcs *grovev1alpha1.PodCliqueSet)

// requireAccepted applies mutations to the PodCliqueSet pcsName, retrying on conflict, and fails the test when
// the update is rejected.
func requireAccepted(t *testing.T, tc *testctx.TestContext, pcsName, what string, mutations ...pcsMutation) {
	t.Helper()
	err := retry.RetryOnConflict(retry.DefaultRetry, func() error {
		pcs := &grovev1alpha1.PodCliqueSet{}
		if err := tc.Client.Get(tc.Ctx, types.NamespacedName{Namespace: testNamespace, Name: pcsName}, pcs); err != nil {
			return err
		}
		for _, mutate := range mutations {
			mutate(pcs)
		}
		return tc.Client.Update(tc.Ctx, pcs)
	})
	require.NoError(t, err, "%s must be accepted", what)
}

func cliqueTemplate(pcs *grovev1alpha1.PodCliqueSet, name string) *grovev1alpha1.PodCliqueTemplateSpec {
	for _, c := range pcs.Spec.Template.Cliques {
		if c.Name == name {
			return c
		}
	}
	panic(fmt.Sprintf("clique %s not found in PodCliqueSet %s", name, pcs.Name))
}

func scalingGroupConfig(pcs *grovev1alpha1.PodCliqueSet, name string) *grovev1alpha1.PodCliqueScalingGroupConfig {
	for i := range pcs.Spec.Template.PodCliqueScalingGroupConfigs {
		if pcs.Spec.Template.PodCliqueScalingGroupConfigs[i].Name == name {
			return &pcs.Spec.Template.PodCliqueScalingGroupConfigs[i]
		}
	}
	panic(fmt.Sprintf("scaling group %s not found in PodCliqueSet %s", name, pcs.Name))
}

func setCliqueMinAvailable(name string, minAvailable int32) pcsMutation {
	return func(pcs *grovev1alpha1.PodCliqueSet) {
		cliqueTemplate(pcs, name).Spec.MinAvailable = ptr.To(minAvailable)
	}
}

func setGroupMinAvailable(name string, minAvailable int32) pcsMutation {
	return func(pcs *grovev1alpha1.PodCliqueSet) {
		scalingGroupConfig(pcs, name).MinAvailable = ptr.To(minAvailable)
	}
}

// newRevision returns a value for changePodTemplate that no earlier change used.
func newRevision() string {
	return strconv.FormatInt(time.Now().UnixNano(), 10)
}

// changePodTemplate sets the UPDATE_TRIGGER env var of the named cliques to value, which starts a rolling update.
func changePodTemplate(value string, cliqueNames ...string) pcsMutation {
	return func(pcs *grovev1alpha1.PodCliqueSet) {
		for _, name := range cliqueNames {
			container := &cliqueTemplate(pcs, name).Spec.PodSpec.Containers[0]
			container.Env = slices.DeleteFunc(container.Env, func(env corev1.EnvVar) bool { return env.Name == updateTriggerEnv })
			container.Env = append(container.Env, corev1.EnvVar{Name: updateTriggerEnv, Value: value})
		}
	}
}

// podRevision returns the UPDATE_TRIGGER value of the pod's first container, or "" when it has none.
func podRevision(pod *corev1.Pod) string {
	for _, env := range pod.Spec.Containers[0].Env {
		if env.Name == updateTriggerEnv {
			return env.Value
		}
	}
	return ""
}

// waitForRollingUpdateComplete waits until the update of the PodCliqueSet pcsName has ended with every replica
// updated and every live pod ready, and every pod of workerClique carries revision.
func waitForRollingUpdateComplete(t *testing.T, tc *testctx.TestContext, pcsName, revision string) {
	t.Helper()
	waitForPCS(t, tc, pcsName, "the rolling update to finish", func(pcs *grovev1alpha1.PodCliqueSet) bool {
		progress := pcs.Status.UpdateProgress
		if pcs.Status.UpdatedReplicas != pcs.Spec.Replicas || (progress != nil && progress.UpdateEndedAt == nil) {
			return false
		}
		pods, err := listLivePods(tc.Ctx, tc, pcsName)
		return err == nil && len(pods) == expectedPods(pcs) &&
			!slices.ContainsFunc(pods, func(pod corev1.Pod) bool {
				inClique := strings.HasSuffix(pod.Labels[common.LabelPodClique], "-"+workerClique)
				return !isPodReady(&pod) || (inClique && podRevision(&pod) != revision)
			})
	})
}

func pclqName(pcsName string, replicaIndex int, cliqueName string) string {
	return fmt.Sprintf("%s-%d-%s", pcsName, replicaIndex, cliqueName)
}

func pcsgName(pcsName string, replicaIndex int, groupName string) string {
	return fmt.Sprintf("%s-%d-%s", pcsName, replicaIndex, groupName)
}

func getPCS(t *testing.T, tc *testctx.TestContext, pcsName string) *grovev1alpha1.PodCliqueSet {
	t.Helper()
	pcs := &grovev1alpha1.PodCliqueSet{}
	require.NoError(t, tc.Client.Get(tc.Ctx, types.NamespacedName{Namespace: testNamespace, Name: pcsName}, pcs))
	return pcs
}

func getPCLQ(t *testing.T, tc *testctx.TestContext, name string) *grovev1alpha1.PodClique {
	t.Helper()
	pclq := &grovev1alpha1.PodClique{}
	require.NoError(t, tc.Client.Get(tc.Ctx, types.NamespacedName{Namespace: testNamespace, Name: name}, pclq))
	return pclq
}

func getPCSG(t *testing.T, tc *testctx.TestContext, name string) *grovev1alpha1.PodCliqueScalingGroup {
	t.Helper()
	pcsg := &grovev1alpha1.PodCliqueScalingGroup{}
	require.NoError(t, tc.Client.Get(tc.Ctx, types.NamespacedName{Namespace: testNamespace, Name: name}, pcsg))
	return pcsg
}

// waitForPCS waits until predicate holds for the PodCliqueSet pcsName and returns it.
func waitForPCS(t *testing.T, tc *testctx.TestContext, pcsName, what string, predicate func(*grovev1alpha1.PodCliqueSet) bool) *grovev1alpha1.PodCliqueSet {
	t.Helper()
	var pcs *grovev1alpha1.PodCliqueSet
	err := wait.PollUntilContextTimeout(tc.Ctx, pollInterval, recreateTimeout, true, func(ctx context.Context) (bool, error) {
		current := &grovev1alpha1.PodCliqueSet{}
		if err := tc.Client.Get(ctx, types.NamespacedName{Namespace: testNamespace, Name: pcsName}, current); err != nil {
			return false, client.IgnoreNotFound(err)
		}
		pcs = current
		return predicate(current), nil
	})
	require.NoError(t, err, "%s: timed out waiting for %s", pcsName, what)
	return pcs
}

// waitForPCLQ waits until predicate holds for the PodClique name and returns it.
func waitForPCLQ(t *testing.T, tc *testctx.TestContext, name, what string, predicate func(*grovev1alpha1.PodClique) bool) *grovev1alpha1.PodClique {
	t.Helper()
	var pclq *grovev1alpha1.PodClique
	err := wait.PollUntilContextTimeout(tc.Ctx, pollInterval, recreateTimeout, true, func(ctx context.Context) (bool, error) {
		current := &grovev1alpha1.PodClique{}
		if err := tc.Client.Get(ctx, types.NamespacedName{Namespace: testNamespace, Name: name}, current); err != nil {
			return false, client.IgnoreNotFound(err)
		}
		pclq = current
		return predicate(current), nil
	})
	require.NoError(t, err, "%s: timed out waiting for %s", name, what)
	return pclq
}

// requireStable fails the test if check reports a change at any point during d.
func requireStable(t *testing.T, d time.Duration, what string, check func() error) {
	t.Helper()
	deadline := time.Now().Add(d)
	for time.Now().Before(deadline) {
		require.NoError(t, check(), what)
		time.Sleep(pollInterval)
	}
}

// workerUIDs returns the UID of the worker PodClique in each PodCliqueSet replica, or "" where it is missing.
func workerUIDs(t *testing.T, tc *testctx.TestContext, pcsName string, replicas int) []types.UID {
	t.Helper()
	uids := make([]types.UID, replicas)
	for i := range replicas {
		pclq := &grovev1alpha1.PodClique{}
		if err := tc.Client.Get(tc.Ctx, types.NamespacedName{Namespace: testNamespace, Name: pclqName(pcsName, i, workerClique)}, pclq); err == nil {
			uids[i] = pclq.UID
		}
	}
	return uids
}

// requireAllChanged fails the test unless every UID in after differs from the one at the same index in before.
func requireAllChanged(t *testing.T, before, after []types.UID) {
	t.Helper()
	for i := range before {
		require.NotEqual(t, before[i], after[i], "the worker PodClique of replica %d must have been recreated", i)
	}
}

// livePodsOfReplica returns the live pods of the PodCliqueSet pcsName in the given replica.
func livePodsOfReplica(t *testing.T, tc *testctx.TestContext, pcsName string, replicaIndex int) []corev1.Pod {
	t.Helper()
	pods, err := listLivePods(tc.Ctx, tc, pcsName)
	require.NoError(t, err)
	return slices.DeleteFunc(pods, func(pod corev1.Pod) bool {
		return pod.Labels[common.LabelPodCliqueSetReplicaIndex] != strconv.Itoa(replicaIndex)
	})
}

func deletePods(t *testing.T, tc *testctx.TestContext, pods ...corev1.Pod) {
	t.Helper()
	for i := range pods {
		require.NoError(t, client.IgnoreNotFound(tc.Client.Delete(tc.Ctx, &pods[i])), "deleting pod %s", pods[i].Name)
	}
}

func gangShapeCondition(pcs *grovev1alpha1.PodCliqueSet) *metav1.Condition {
	return meta.FindStatusCondition(pcs.Status.Conditions, apiconstants.ConditionTypeGangShapeRecreateInProgress)
}

func isBreached(pclq *grovev1alpha1.PodClique) bool {
	return meta.IsStatusConditionTrue(pclq.Status.Conditions, apiconstants.ConditionTypeMinAvailableBreached)
}

// currentlyRecreatingIndex returns the replica that the PodCliqueSet reports as being recreated, or nil.
func currentlyRecreatingIndex(pcs *grovev1alpha1.PodCliqueSet) *int {
	if pcs.Status.GangShapeRecreate == nil || pcs.Status.GangShapeRecreate.CurrentlyRecreating == nil {
		return nil
	}
	return ptr.To(int(pcs.Status.GangShapeRecreate.CurrentlyRecreating.ReplicaIndex))
}

// waitForGangShapeCurrent waits until every replica of the PodCliqueSet pcsName has been recreated to its current
// gang shape: the condition is False with reason GangShapeCurrent, status.gangShapeRecreate is cleared, every
// PodClique and PodCliqueScalingGroup has the MinAvailable of the PodCliqueSet template, and every pod is ready.
// It returns the PodCliqueSet.
func waitForGangShapeCurrent(t *testing.T, tc *testctx.TestContext, pcsName string) *grovev1alpha1.PodCliqueSet {
	t.Helper()
	pcs := waitForPCS(t, tc, pcsName, "the gang-shape recreate to finish", func(pcs *grovev1alpha1.PodCliqueSet) bool {
		cond := gangShapeCondition(pcs)
		if pcs.Status.GangShapeRecreate != nil || cond == nil || cond.Status != metav1.ConditionFalse || cond.Reason != apiconstants.ConditionReasonGangShapeCurrent {
			return false
		}
		stale, err := staleChildren(tc.Ctx, tc, pcs)
		return err == nil && len(stale) == 0
	})
	waitForReadyPods(t, tc, pcsName, expectedPods(pcs))
	return pcs
}

// staleChildren returns the live PodCliques and PodCliqueScalingGroups of pcs whose spec.minAvailable is not the
// one in the PodCliqueSet template.
func staleChildren(ctx context.Context, tc *testctx.TestContext, pcs *grovev1alpha1.PodCliqueSet) ([]string, error) {
	c, err := listChildren(ctx, tc, pcs.Name)
	if err != nil {
		return nil, err
	}
	var stale []string
	for _, pclq := range c.pclqs {
		cliqueName := pclq.Name[strings.LastIndex(pclq.Name, "-")+1:]
		if pclq.DeletionTimestamp == nil && !ptr.Equal(pclq.Spec.MinAvailable, cliqueTemplate(pcs, cliqueName).Spec.MinAvailable) {
			stale = append(stale, pclq.Name)
		}
	}
	for _, pcsg := range c.pcsgs {
		groupName := pcsg.Name[strings.LastIndex(pcsg.Name, "-")+1:]
		if pcsg.DeletionTimestamp == nil && !ptr.Equal(pcsg.Spec.MinAvailable, scalingGroupConfig(pcs, groupName).MinAvailable) {
			stale = append(stale, pcsg.Name)
		}
	}
	return stale, nil
}

// onceEach returns the deletion count of each replica in replicaIndices as 1.
func onceEach(replicaIndices ...int) map[int]int {
	deletions := make(map[int]int, len(replicaIndices))
	for _, i := range replicaIndices {
		deletions[i] = 1
	}
	return deletions
}

// requirePodGroupMinReplicas waits until every PodGang pod group of the PodClique cliqueName in the PodCliqueSet
// pcsName asks for minReplicas.
func requirePodGroupMinReplicas(t *testing.T, tc *testctx.TestContext, pcsName, cliqueName string, minReplicas int32) {
	t.Helper()
	verifier := podgang.NewVerifier(tc.Client, tests.Logger)
	nsName := types.NamespacedName{Namespace: testNamespace, Name: pcsName}
	suffix := "-" + cliqueName
	err := wait.PollUntilContextTimeout(tc.Ctx, pollInterval, recreateTimeout, true, func(ctx context.Context) (bool, error) {
		podGangs, err := verifier.List(ctx, nsName)
		if err != nil {
			return false, err
		}
		found := false
		for _, pg := range podGangs {
			for _, group := range pg.Spec.PodGroups {
				if !strings.HasSuffix(group.Name, suffix) {
					continue
				}
				found = true
				if group.MinReplicas != minReplicas {
					return false, nil
				}
			}
		}
		return found, nil
	})
	require.NoError(t, err, "PodGang pod groups of %s in %s did not reach minReplicas %d", cliqueName, pcsName, minReplicas)
}

// recreateObserver samples a PodCliqueSet while its replicas are recreated. It records the order in which
// replicas were picked, the condition reasons that were reported, the largest number of replicas that were
// unavailable at the same time, and every PodClique UID seen.
type recreateObserver struct {
	tc      *testctx.TestContext
	pcsName string
	cancel  context.CancelFunc
	done    chan struct{}

	mu             sync.Mutex
	order          []int
	reasons        sets.Set[string]
	maxUnavailable int
	pclqUIDs       map[string]sets.Set[types.UID]
}

func observeRecreate(tc *testctx.TestContext, pcsName string) *recreateObserver {
	ctx, cancel := context.WithCancel(tc.Ctx)
	o := &recreateObserver{
		tc: tc, pcsName: pcsName, cancel: cancel, done: make(chan struct{}),
		reasons:  sets.New[string](),
		pclqUIDs: map[string]sets.Set[types.UID]{},
	}
	go func() {
		defer close(o.done)
		ticker := time.NewTicker(sampleInterval)
		defer ticker.Stop()
		for {
			o.sample(ctx)
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
			}
		}
	}()
	return o
}

// stop stops sampling. The recorded fields can be read once it returns.
func (o *recreateObserver) stop() {
	o.cancel()
	<-o.done
}

func (o *recreateObserver) sample(ctx context.Context) {
	pcs := &grovev1alpha1.PodCliqueSet{}
	if err := o.tc.Client.Get(ctx, types.NamespacedName{Namespace: testNamespace, Name: o.pcsName}, pcs); err != nil {
		return
	}
	c, err := listChildren(ctx, o.tc, pcs.Name)
	if err != nil {
		return
	}
	o.mu.Lock()
	defer o.mu.Unlock()
	for _, pclq := range c.pclqs {
		if o.pclqUIDs[pclq.Name] == nil {
			o.pclqUIDs[pclq.Name] = sets.New[types.UID]()
		}
		o.pclqUIDs[pclq.Name].Insert(pclq.UID)
	}
	if index := currentlyRecreatingIndex(pcs); index != nil {
		if len(o.order) == 0 || o.order[len(o.order)-1] != *index {
			o.order = append(o.order, *index)
		}
	}
	if cond := gangShapeCondition(pcs); cond != nil && cond.Status == metav1.ConditionTrue {
		o.reasons.Insert(cond.Reason)
	}
	o.maxUnavailable = max(o.maxUnavailable, unavailableReplicas(pcs, c))
}

// workerDeletions returns how many times the observer saw the worker PodClique of each of the first replicas
// replicas deleted, leaving out replicas that were never deleted.
func (o *recreateObserver) workerDeletions(replicas int) map[int]int {
	o.mu.Lock()
	defer o.mu.Unlock()
	deletions := map[int]int{}
	for i := range replicas {
		if n := o.pclqUIDs[pclqName(o.pcsName, i, workerClique)].Len() - 1; n > 0 {
			deletions[i] = n
		}
	}
	return deletions
}

// children holds the PodCliques and PodCliqueScalingGroups of a PodCliqueSet.
type children struct {
	pclqs []grovev1alpha1.PodClique
	pcsgs []grovev1alpha1.PodCliqueScalingGroup
}

func listChildren(ctx context.Context, tc *testctx.TestContext, pcsName string) (*children, error) {
	selector := client.MatchingLabels{common.LabelPartOfKey: pcsName}
	pclqs := &grovev1alpha1.PodCliqueList{}
	if err := tc.Client.List(ctx, pclqs, client.InNamespace(testNamespace), selector); err != nil {
		return nil, err
	}
	pcsgs := &grovev1alpha1.PodCliqueScalingGroupList{}
	if err := tc.Client.List(ctx, pcsgs, client.InNamespace(testNamespace), selector); err != nil {
		return nil, err
	}
	return &children{pclqs: pclqs.Items, pcsgs: pcsgs.Items}, nil
}

// unavailableReplicas counts the replicas of pcs that miss a standalone PodClique or a PodCliqueScalingGroup, have
// one being deleted, or have one with less than spec.minAvailable ready. It uses the operator's definition of an
// available replica.
func unavailableReplicas(pcs *grovev1alpha1.PodCliqueSet, c *children) int {
	members := sets.New[string]()
	for _, g := range pcs.Spec.Template.PodCliqueScalingGroupConfigs {
		members.Insert(g.CliqueNames...)
	}
	available := map[string]bool{}
	for _, pclq := range c.pclqs {
		available[pclq.Name] = pclq.DeletionTimestamp == nil && pclq.Status.ReadyReplicas >= ptr.Deref(pclq.Spec.MinAvailable, pclq.Spec.Replicas)
	}
	for _, pcsg := range c.pcsgs {
		available[pcsg.Name] = pcsg.DeletionTimestamp == nil && pcsg.Status.AvailableReplicas >= ptr.Deref(pcsg.Spec.MinAvailable, pcsg.Spec.Replicas)
	}
	count := 0
	for i := range int(pcs.Spec.Replicas) {
		ok := true
		for _, clique := range pcs.Spec.Template.Cliques {
			if !members.Has(clique.Name) {
				ok = ok && available[pclqName(pcs.Name, i, clique.Name)]
			}
		}
		for _, g := range pcs.Spec.Template.PodCliqueScalingGroupConfigs {
			ok = ok && available[pcsgName(pcs.Name, i, g.Name)]
		}
		if !ok {
			count++
		}
	}
	return count
}

// podStage is a KWOK Stage that takes over the fresh pods of some replicas of one PodCliqueSet.
type podStage struct {
	name     string
	pcsName  string
	replicas []int
	// crashLoop makes the pods report a container that exited non-zero, which breaches MinAvailable. Otherwise
	// the pods stay NotReady for delay, which leaves the replica unavailable without a breach.
	crashLoop bool
	delay     time.Duration
}

const stageManifest = `apiVersion: kwok.x-k8s.io/v1alpha1
kind: Stage
metadata:
  name: %s
spec:
  resourceRef:
    apiGroup: v1
    kind: Pod
  selector:
    matchExpressions:
      - key: .metadata.deletionTimestamp
        operator: DoesNotExist
      - key: .status.podIP
        operator: DoesNotExist
      - key: '.metadata.labels["app.kubernetes.io/part-of"]'
        operator: In
        values: [%q]
      - key: '.metadata.labels["grove.io/podcliqueset-replica-index"]'
        operator: In
        values: [%s]
  weight: 30
  delay:
    durationMilliseconds: %d
  next:
    statusSubresource: status
    statusTemplate: |
%s`

const readyStatusTemplate = `      {{ $now := Now }}
      conditions:
      - lastTransitionTime: {{ $now | Quote }}
        status: "True"
        type: Initialized
      - lastTransitionTime: {{ $now | Quote }}
        status: "True"
        type: Ready
      - lastTransitionTime: {{ $now | Quote }}
        status: "True"
        type: ContainersReady
      containerStatuses:
      {{ range .spec.containers }}
      - image: {{ .image | Quote }}
        name: {{ .name | Quote }}
        ready: true
        restartCount: 0
        state:
          running:
            startedAt: {{ $now | Quote }}
      {{ end }}
      hostIP: {{ NodeIPWith .spec.nodeName | Quote }}
      podIP: {{ PodIPWith .spec.nodeName ( or .spec.hostNetwork false ) ( or .metadata.uid "" ) ( or .metadata.name "" ) ( or .metadata.namespace "" ) | Quote }}
      phase: Running
      startTime: {{ $now | Quote }}
`

const crashLoopStatusTemplate = `      {{ $now := Now }}
      conditions:
      - lastTransitionTime: {{ $now | Quote }}
        status: "True"
        type: Initialized
      - lastTransitionTime: {{ $now | Quote }}
        status: "False"
        reason: ContainersNotReady
        type: Ready
      - lastTransitionTime: {{ $now | Quote }}
        status: "False"
        reason: ContainersNotReady
        type: ContainersReady
      containerStatuses:
      {{ range .spec.containers }}
      - image: {{ .image | Quote }}
        name: {{ .name | Quote }}
        ready: false
        restartCount: 3
        state:
          waiting:
            reason: CrashLoopBackOff
            message: "simulated crash loop"
        lastState:
          terminated:
            exitCode: 1
            reason: Error
            startedAt: {{ $now | Quote }}
            finishedAt: {{ $now | Quote }}
      {{ end }}
      hostIP: {{ NodeIPWith .spec.nodeName | Quote }}
      podIP: {{ PodIPWith .spec.nodeName ( or .spec.hostNetwork false ) ( or .metadata.uid "" ) ( or .metadata.name "" ) ( or .metadata.namespace "" ) | Quote }}
      phase: Running
      startTime: {{ $now | Quote }}
`

// holdStage keeps the fresh pods of the given replicas NotReady until it is released.
func holdStage(pcsName string, replicas ...int) podStage {
	return podStage{name: pcsName + "-hold", pcsName: pcsName, replicas: replicas, delay: holdDelay}
}

// crashLoopStage makes the fresh pods of the given replicas crash-loop until it is released.
func crashLoopStage(pcsName string, replicas ...int) podStage {
	return podStage{name: pcsName + "-crashloop", pcsName: pcsName, replicas: replicas, crashLoop: true}
}

func (s podStage) manifest() string {
	values := make([]string, 0, len(s.replicas))
	for _, i := range s.replicas {
		values = append(values, strconv.Quote(strconv.Itoa(i)))
	}
	statusTemplate := readyStatusTemplate
	if s.crashLoop {
		statusTemplate = crashLoopStatusTemplate
	}
	return fmt.Sprintf(stageManifest, s.name, s.pcsName, strings.Join(values, ", "), s.delay.Milliseconds(), statusTemplate)
}

// apply applies the stage and deletes it when the test ends.
func (s podStage) apply(t *testing.T, tc *testctx.TestContext) {
	t.Helper()
	require.NoError(t, kwok.ApplyStageManifest(tc.Ctx, tc.Client, s.manifest()), "applying KWOK stage %s", s.name)
	t.Cleanup(func() {
		if err := kwok.DeleteStage(context.WithoutCancel(tc.Ctx), tc.Client, s.name); err != nil {
			t.Errorf("deleting KWOK stage %s: %v", s.name, err)
		}
	})
}

// release deletes the stage, then deletes every not-ready pod of its replicas, so their replacements become ready
// through the default pod-ready stage. KWOK does not re-evaluate a pod that a removed stage still holds, and a
// crash-looping pod already has a podIP, which the default stage does not match.
func (s podStage) release(t *testing.T, tc *testctx.TestContext) {
	t.Helper()
	require.NoError(t, kwok.DeleteStage(tc.Ctx, tc.Client, s.name), "deleting KWOK stage %s", s.name)
	for _, i := range s.replicas {
		pods := livePodsOfReplica(t, tc, s.pcsName, i)
		deletePods(t, tc, slices.DeleteFunc(pods, func(pod corev1.Pod) bool { return isPodReady(&pod) })...)
	}
}
