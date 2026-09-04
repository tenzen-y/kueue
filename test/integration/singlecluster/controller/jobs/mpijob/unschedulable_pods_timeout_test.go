/*
Copyright The Kubernetes Authors.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package mpijob

import (
	"fmt"
	"time"

	kfmpi "github.com/kubeflow/mpi-operator/pkg/apis/kubeflow/v2beta1"
	"github.com/onsi/ginkgo/v2"
	"github.com/onsi/gomega"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"

	configapi "sigs.k8s.io/kueue/apis/config/v1beta2"
	kueue "sigs.k8s.io/kueue/apis/kueue/v1beta2"
	pkgconstants "sigs.k8s.io/kueue/pkg/constants"
	"sigs.k8s.io/kueue/pkg/controller/jobframework"
	workloadmpijob "sigs.k8s.io/kueue/pkg/controller/jobs/mpijob"
	utiltestingapi "sigs.k8s.io/kueue/pkg/util/testing/v1beta2"
	testingmpijob "sigs.k8s.io/kueue/pkg/util/testingjobs/mpijob"
	testingpod "sigs.k8s.io/kueue/pkg/util/testingjobs/pod"
	"sigs.k8s.io/kueue/test/util"
)

var _ = ginkgo.Describe("MPIJob controller interacting with Workload controller when waitForPodsReady is enabled", ginkgo.Ordered, ginkgo.ContinueOnFailure, func() {
	var (
		waitForPodsReady *configapi.WaitForPodsReady
		ns               *corev1.Namespace
		defaultFlavor    *kueue.ResourceFlavor
	)

	ginkgo.JustBeforeEach(func() {
		fwk.StartManager(ctx, cfg, managerSetupWithConfiguration(
			&configapi.Configuration{WaitForPodsReady: waitForPodsReady},
			false,
			jobframework.WithWaitForPodsReady(waitForPodsReady),
		))

		ns = util.CreateNamespaceFromPrefixWithLog(ctx, k8sClient, "core-")

		defaultFlavor = utiltestingapi.MakeResourceFlavor("default").NodeLabel(instanceKey, "default").Obj()
		util.MustCreate(ctx, k8sClient, defaultFlavor)
	})

	ginkgo.JustAfterEach(func() {
		gomega.Expect(util.DeleteNamespace(ctx, k8sClient, ns)).To(gomega.Succeed())
		util.ExpectObjectToBeDeleted(ctx, k8sClient, defaultFlavor, true)
		fwk.StopManager(ctx)
	})

	ginkgo.When("unschedulableTimeout is configured", func() {
		const (
			unschedulableTimeout = 30 * time.Second
			workerReplicas       = 2
		)

		ginkgo.BeforeEach(func() {
			waitForPodsReady = &configapi.WaitForPodsReady{
				Timeout:              metav1.Duration{Duration: 5 * time.Minute},
				UnschedulableTimeout: &metav1.Duration{Duration: unschedulableTimeout},
				RecoveryTimeout:      &metav1.Duration{},
				RequeuingStrategy: &configapi.RequeuingStrategy{
					Timestamp:          new(configapi.EvictionTimestamp),
					BackoffBaseSeconds: new(int32(10)),
				},
			}
		})

		ginkgo.It("should report the PodsScheduled condition from the launcher and worker pods", func() {
			ginkgo.By("creating an MPIJob with a launcher and two workers")
			job := testingmpijob.MakeMPIJob(jobName, ns.Name).
				Queue("test-queue").
				GenericLauncherAndWorker().
				Parallelism(workerReplicas).
				Obj()
			util.MustCreate(ctx, k8sClient, job)
			jobKey := client.ObjectKeyFromObject(job)
			wlKey := types.NamespacedName{Name: workloadmpijob.GetWorkloadNameForMPIJob(job.Name, job.UID), Namespace: ns.Name}

			ginkgo.By("admitting the workload created for the MPIJob")
			wl := &kueue.Workload{}
			gomega.Eventually(func(g gomega.Gomega) {
				g.Expect(k8sClient.Get(ctx, wlKey, wl)).To(gomega.Succeed())
				g.Expect(wl.Spec.PodSets).To(gomega.HaveLen(2))
			}, util.Timeout, util.Interval).Should(gomega.Succeed())
			podSetAssignments := make([]kueue.PodSetAssignment, 0, len(wl.Spec.PodSets))
			for _, podSet := range wl.Spec.PodSets {
				podSetAssignments = append(podSetAssignments, kueue.PodSetAssignment{
					Name: podSet.Name,
					Flavors: map[corev1.ResourceName]kueue.ResourceFlavorReference{
						corev1.ResourceCPU: kueue.ResourceFlavorReference(defaultFlavor.Name),
					},
					Count: new(podSet.Count),
				})
			}
			util.SetQuotaReservation(ctx, k8sClient, wlKey, utiltestingapi.MakeAdmission("foo").PodSets(podSetAssignments...).Obj())
			util.SyncAdmittedConditionForWorkloads(ctx, k8sClient, wl)

			ginkgo.By("checking the MPIJob is unsuspended with the workload annotations and the PodSet label on the launcher and worker pod templates")
			createdJob := &kfmpi.MPIJob{}
			gomega.Eventually(func(g gomega.Gomega) {
				g.Expect(k8sClient.Get(ctx, jobKey, createdJob)).To(gomega.Succeed())
				g.Expect(createdJob.Spec.RunPolicy.Suspend).To(gomega.Equal(new(false)))
				for replicaType, replicaSpec := range createdJob.Spec.MPIReplicaSpecs {
					g.Expect(replicaSpec.Template.Annotations).To(gomega.HaveKeyWithValue(kueue.WorkloadAnnotation, wlKey.Name), string(replicaType))
					g.Expect(replicaSpec.Template.Annotations).To(gomega.HaveKeyWithValue(kueue.WorkloadUIDAnnotation, string(wl.UID)), string(replicaType))
					g.Expect(replicaSpec.Template.Labels).To(gomega.HaveKeyWithValue(pkgconstants.PodSetLabel, string(kueue.NewPodSetReference(string(replicaType)))), string(replicaType))
				}
			}, util.Timeout, util.Interval).Should(gomega.Succeed())

			ginkgo.By("creating unscheduled pods for the launcher and the workers from the pod templates")
			pods := make([]*corev1.Pod, 0, workerReplicas+1)
			for _, replicaSpec := range createdJob.Spec.MPIReplicaSpecs {
				template := replicaSpec.Template
				podSetName := template.Labels[pkgconstants.PodSetLabel]
				for i := range ptr.Deref(replicaSpec.Replicas, 1) {
					pod := testingpod.MakePod(fmt.Sprintf("%s-%d", podSetName, i), ns.Name).
						Annotation(kueue.WorkloadAnnotation, template.Annotations[kueue.WorkloadAnnotation]).
						Annotation(kueue.WorkloadUIDAnnotation, template.Annotations[kueue.WorkloadUIDAnnotation]).
						Label(pkgconstants.PodSetLabel, podSetName).
						Obj()
					util.MustCreate(ctx, k8sClient, pod)
					pods = append(pods, pod)
				}
			}
			gomega.Expect(pods).To(gomega.HaveLen(workerReplicas + 1))

			expectWorkloadCondition := func(want metav1.Condition) {
				ginkgo.GinkgoHelper()
				gomega.Eventually(func(g gomega.Gomega) {
					g.Expect(k8sClient.Get(ctx, wlKey, wl)).To(gomega.Succeed())
					g.Expect(wl.Status.Conditions).To(gomega.ContainElement(
						gomega.BeComparableTo(want, util.IgnoreConditionTimestampsAndObservedGeneration),
					))
				}, util.Timeout, util.Interval).Should(gomega.Succeed())
			}

			ginkgo.By("checking the PodsScheduled condition reports the unscheduled pods")
			unscheduled := metav1.Condition{
				Type:    kueue.WorkloadPodsScheduled,
				Status:  metav1.ConditionFalse,
				Reason:  kueue.WorkloadWaitForScheduling,
				Message: "At least one required pod is not scheduled",
			}
			expectWorkloadCondition(unscheduled)

			ginkgo.By("checking the PodsReady condition reports the unscheduled pods")
			expectWorkloadCondition(metav1.Condition{
				Type:    kueue.WorkloadPodsReady,
				Status:  metav1.ConditionFalse,
				Reason:  kueue.WorkloadWaitForScheduling,
				Message: "Not all pods are ready or succeeded",
			})

			ginkgo.By("binding all the pods but one to a node")
			util.BindPodWithNode(ctx, k8sClient, "node", pods[:len(pods)-1]...)

			ginkgo.By("checking the PodsScheduled condition keeps reporting the unscheduled pod")
			gomega.Consistently(func(g gomega.Gomega) {
				g.Expect(k8sClient.Get(ctx, wlKey, wl)).To(gomega.Succeed())
				g.Expect(wl.Status.Conditions).To(gomega.ContainElement(
					gomega.BeComparableTo(unscheduled, util.IgnoreConditionTimestampsAndObservedGeneration),
				))
			}, pkgconstants.UpdatesBatchPeriod+util.ShortTimeout, util.Interval).Should(gomega.Succeed())

			ginkgo.By("binding the last pod to a node")
			util.BindPodWithNode(ctx, k8sClient, "node", pods[len(pods)-1])

			ginkgo.By("checking the PodsScheduled condition reports all the pods scheduled")
			expectWorkloadCondition(metav1.Condition{
				Type:    kueue.WorkloadPodsScheduled,
				Status:  metav1.ConditionTrue,
				Reason:  kueue.WorkloadAllRequiredPodsScheduled,
				Message: "All required pods were scheduled or succeeded",
			})

			ginkgo.By("checking the PodsReady condition waits for the pods to start")
			expectWorkloadCondition(metav1.Condition{
				Type:    kueue.WorkloadPodsReady,
				Status:  metav1.ConditionFalse,
				Reason:  kueue.WorkloadWaitForStart,
				Message: "Not all pods are ready or succeeded",
			})
		})
	})
})
