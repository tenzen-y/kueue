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

package job

import (
	"encoding/json"
	"fmt"
	"time"

	"github.com/onsi/ginkgo/v2"
	"github.com/onsi/gomega"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	apimeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/sets"
	"sigs.k8s.io/controller-runtime/pkg/client"

	configapi "sigs.k8s.io/kueue/apis/config/v1beta2"
	kueue "sigs.k8s.io/kueue/apis/kueue/v1beta2"
	pkgconstants "sigs.k8s.io/kueue/pkg/constants"
	"sigs.k8s.io/kueue/pkg/controller/jobframework"
	workloadjob "sigs.k8s.io/kueue/pkg/controller/jobs/job"
	"sigs.k8s.io/kueue/pkg/features"
	utiltestingapi "sigs.k8s.io/kueue/pkg/util/testing/v1beta2"
	testingjob "sigs.k8s.io/kueue/pkg/util/testingjobs/job"
	testingpod "sigs.k8s.io/kueue/pkg/util/testingjobs/pod"
	"sigs.k8s.io/kueue/pkg/workload"
	workloadevict "sigs.k8s.io/kueue/pkg/workload/evict"
	workloadpatching "sigs.k8s.io/kueue/pkg/workload/patching"
	"sigs.k8s.io/kueue/test/util"
)

const (
	trackerFieldManager = pkgconstants.KueueName + "-unschedulable-pods-tracker"

	evictionMargin = 5 * time.Second
)

var (
	unscheduledPods = metav1.Condition{
		Type:    kueue.WorkloadPodsScheduled,
		Status:  metav1.ConditionFalse,
		Reason:  kueue.WorkloadWaitForScheduling,
		Message: "At least one required pod is not scheduled",
	}
	allPodsScheduled = metav1.Condition{
		Type:    kueue.WorkloadPodsScheduled,
		Status:  metav1.ConditionTrue,
		Reason:  kueue.WorkloadAllRequiredPodsScheduled,
		Message: "All required pods were scheduled or succeeded",
	}
	podsReadyWaitForScheduling = metav1.Condition{
		Type:    kueue.WorkloadPodsReady,
		Status:  metav1.ConditionFalse,
		Reason:  kueue.WorkloadWaitForScheduling,
		Message: workload.PodsNotReadyMessage,
	}
	podsReadyWaitForStart = metav1.Condition{
		Type:    kueue.WorkloadPodsReady,
		Status:  metav1.ConditionFalse,
		Reason:  kueue.WorkloadWaitForStart,
		Message: workload.PodsNotReadyMessage,
	}
	podsReadyWaitForRecovery = metav1.Condition{
		Type:    kueue.WorkloadPodsReady,
		Status:  metav1.ConditionFalse,
		Reason:  kueue.WorkloadWaitForRecovery,
		Message: "At least one pod has failed, waiting for recovery",
	}
	podsReadyStarted = metav1.Condition{
		Type:    kueue.WorkloadPodsReady,
		Status:  metav1.ConditionTrue,
		Reason:  kueue.WorkloadStarted,
		Message: "All pods reached readiness and the workload is running",
	}
)

func podsScheduledFieldManagers(wl *kueue.Workload) (sets.Set[string], error) {
	managers := sets.New[string]()
	for _, entry := range wl.ManagedFields {
		if entry.FieldsV1 == nil {
			continue
		}
		var fields map[string]any
		if err := json.Unmarshal(entry.FieldsV1.GetRawBytes(), &fields); err != nil {
			return nil, fmt.Errorf("decoding the managed fields of %s: %w", entry.Manager, err)
		}
		status, _ := fields["f:status"].(map[string]any)
		conditions, _ := status["f:conditions"].(map[string]any)
		if _, owned := conditions[`k:{"type":"PodsScheduled"}`]; owned {
			managers.Insert(entry.Manager)
		}
	}
	return managers, nil
}

var _ = ginkgo.Describe("Job controller with waitForPodsReady unschedulableTimeout", ginkgo.Ordered, ginkgo.ContinueOnFailure, func() {
	const shortUnschedulableTimeout = 8 * time.Second

	var (
		backoffBaseSeconds   int32
		timeout              time.Duration
		unschedulableTimeout *metav1.Duration
		ns                   *corev1.Namespace
		fl                   *kueue.ResourceFlavor
		cq                   *kueue.ClusterQueue
		lq                   *kueue.LocalQueue
		ac                   *kueue.AdmissionCheck
		jobKey               types.NamespacedName
		wlKey                types.NamespacedName
		admission            *kueue.Admission
	)

	ginkgo.JustBeforeEach(func() {
		waitForPodsReady := &configapi.WaitForPodsReady{
			BlockAdmission: new(true),
			Timeout:        metav1.Duration{Duration: timeout},
			RequeuingStrategy: &configapi.RequeuingStrategy{
				Timestamp:          new(configapi.EvictionTimestamp),
				BackoffBaseSeconds: new(backoffBaseSeconds),
			},
			RecoveryTimeout:      &metav1.Duration{},
			UnschedulableTimeout: unschedulableTimeout,
		}
		fwk.StartManager(ctx, cfg, managerAndControllersSetup(
			false,
			false,
			&configapi.Configuration{WaitForPodsReady: waitForPodsReady},
			jobframework.WithWaitForPodsReady(waitForPodsReady),
		))

		ns = util.CreateNamespaceFromPrefixWithLog(ctx, k8sClient, "unschedulable-")

		fl = utiltestingapi.MakeResourceFlavor("fl").Obj()
		util.MustCreate(ctx, k8sClient, fl)

		cq = utiltestingapi.MakeClusterQueue("cq").
			ResourceGroup(*utiltestingapi.MakeFlavorQuotas(fl.Name).Resource(corev1.ResourceCPU, "10").Obj()).Obj()
		util.MustCreate(ctx, k8sClient, cq)

		lq = utiltestingapi.MakeLocalQueue("lq", ns.Name).ClusterQueue(cq.Name).Obj()
		util.MustCreate(ctx, k8sClient, lq)

		ginkgo.By("creating the job")
		job := testingjob.MakeJob("job", ns.Name).Queue(kueue.LocalQueueName(lq.Name)).Request(corev1.ResourceCPU, "2").Obj()
		util.MustCreate(ctx, k8sClient, job)
		jobKey = client.ObjectKeyFromObject(job)
		wlKey = types.NamespacedName{Name: workloadjob.GetWorkloadNameForJob(job.Name, job.UID), Namespace: job.Namespace}
		admission = utiltestingapi.MakeAdmission(kueue.ClusterQueueReference(cq.Name)).
			PodSets(utiltestingapi.MakePodSetAssignment(kueue.DefaultPodSetName).
				Assignment(corev1.ResourceCPU, kueue.ResourceFlavorReference(fl.Name), "2").Obj()).
			Obj()
	})

	ginkgo.JustAfterEach(func() {
		gomega.Expect(util.DeleteNamespace(ctx, k8sClient, ns)).To(gomega.Succeed())
		util.ExpectObjectToBeDeleted(ctx, k8sClient, cq, true)
		util.ExpectObjectToBeDeleted(ctx, k8sClient, ac, true)
		ac = nil
		util.ExpectObjectToBeDeleted(ctx, k8sClient, fl, true)
		fwk.StopManager(ctx)
	})

	evictionMessage := func() string {
		return fmt.Sprintf("Exceeded the PodsReady timeout %s", wlKey.String())
	}

	getWorkload := func(g gomega.Gomega) *kueue.Workload {
		wl := &kueue.Workload{}
		g.Expect(k8sClient.Get(ctx, wlKey, wl)).To(gomega.Succeed())
		return wl
	}

	createLinkedPod := func() *corev1.Pod {
		ginkgo.GinkgoHelper()
		var wl *kueue.Workload
		gomega.Eventually(func(g gomega.Gomega) {
			wl = getWorkload(g)
		}, util.Timeout, util.Interval).Should(gomega.Succeed())
		pod := testingpod.MakePod("job-pod", ns.Name).
			Annotation(kueue.WorkloadAnnotation, wlKey.Name).
			Annotation(kueue.WorkloadUIDAnnotation, string(wl.UID)).
			Label(pkgconstants.PodSetLabel, string(kueue.DefaultPodSetName)).
			Obj()
		util.MustCreate(ctx, k8sClient, pod)
		return pod
	}

	markScheduled := func(pod *corev1.Pod) {
		ginkgo.GinkgoHelper()
		gomega.Eventually(func(g gomega.Gomega) {
			g.Expect(k8sClient.Get(ctx, client.ObjectKeyFromObject(pod), pod)).To(gomega.Succeed())
			pod.Status.Conditions = []corev1.PodCondition{{Type: corev1.PodScheduled, Status: corev1.ConditionTrue}}
			g.Expect(k8sClient.Status().Update(ctx, pod)).To(gomega.Succeed())
		}, util.Timeout, util.Interval).Should(gomega.Succeed())
	}

	admitWorkload := func() {
		ginkgo.GinkgoHelper()
		util.SetQuotaReservation(ctx, k8sClient, wlKey, admission)
		util.ExpectJobUnsuspended(ctx, k8sClient, jobKey)
	}

	updateJobStatus := func(update func(job *batchv1.Job)) {
		ginkgo.GinkgoHelper()
		gomega.Eventually(func(g gomega.Gomega) {
			job := &batchv1.Job{}
			g.Expect(k8sClient.Get(ctx, jobKey, job)).To(gomega.Succeed())
			update(job)
			g.Expect(k8sClient.Status().Update(ctx, job)).To(gomega.Succeed())
		}, util.Timeout, util.Interval).Should(gomega.Succeed())
	}

	expectWorkloadConditions := func(want ...metav1.Condition) {
		ginkgo.GinkgoHelper()
		matchers := make([]any, 0, len(want))
		for _, c := range want {
			matchers = append(matchers, gomega.BeComparableTo(c, util.IgnoreConditionTimestampsAndObservedGeneration))
		}
		gomega.Eventually(func(g gomega.Gomega) {
			g.Expect(getWorkload(g).Status.Conditions).To(gomega.ContainElements(matchers...))
		}, util.Timeout, util.Interval).Should(gomega.Succeed())
	}

	expectCurrentObservation := func(want metav1.Condition) (metav1.Condition, time.Time) {
		ginkgo.GinkgoHelper()
		var observation metav1.Condition
		var admittedAt time.Time
		gomega.Eventually(func(g gomega.Gomega) {
			wl := getWorkload(g)
			admitted := apimeta.FindStatusCondition(wl.Status.Conditions, kueue.WorkloadAdmitted)
			g.Expect(admitted).To(gomega.HaveValue(gomega.HaveField("Status", metav1.ConditionTrue)))
			observed := apimeta.FindStatusCondition(wl.Status.Conditions, kueue.WorkloadPodsScheduled)
			g.Expect(observed).To(gomega.HaveValue(gomega.BeComparableTo(want, util.IgnoreConditionTimestampsAndObservedGeneration)))
			g.Expect(observed.LastTransitionTime.Time).To(gomega.BeTemporally(">", admitted.LastTransitionTime.Time))
			observation = *observed
			admittedAt = admitted.LastTransitionTime.Time
		}, util.Timeout, util.Interval).Should(gomega.Succeed())
		return observation, admittedAt
	}

	expectPodsScheduledToEqual := func(want metav1.Condition) {
		ginkgo.GinkgoHelper()
		got := apimeta.FindStatusCondition(getWorkload(gomega.Default).Status.Conditions, kueue.WorkloadPodsScheduled)
		gomega.Expect(got).To(gomega.HaveValue(gomega.BeComparableTo(want, util.IgnoreConditionTimestamps)))
		gomega.Expect(got.LastTransitionTime.Time).To(gomega.BeTemporally("==", want.LastTransitionTime.Time))
	}

	expectEvicted := func(cause kueue.EvictionUnderlyingCause, count int32, budget time.Duration) metav1.Condition {
		ginkgo.GinkgoHelper()
		var evicted metav1.Condition
		gomega.Eventually(func(g gomega.Gomega) {
			wl := getWorkload(g)
			cond := apimeta.FindStatusCondition(wl.Status.Conditions, kueue.WorkloadEvicted)
			g.Expect(cond).To(gomega.HaveValue(gomega.BeComparableTo(metav1.Condition{
				Type:    kueue.WorkloadEvicted,
				Status:  metav1.ConditionTrue,
				Reason:  kueue.WorkloadEvictedByPodsReadyTimeout,
				Message: evictionMessage(),
			}, util.IgnoreConditionTimestampsAndObservedGeneration)))
			g.Expect(wl.Status.SchedulingStats).To(gomega.HaveValue(gomega.HaveField("Evictions", gomega.ContainElement(
				kueue.WorkloadSchedulingStatsEviction{
					Reason:          kueue.WorkloadEvictedByPodsReadyTimeout,
					UnderlyingCause: cause,
					Count:           count,
				},
			))))
			evicted = *cond
		}, budget, util.Interval).Should(gomega.Succeed())
		util.ExpectEvictedWorkloadsTotalMetric(cq.Name, kueue.WorkloadEvictedByPodsReadyTimeout, string(cause), "", int(count))
		return evicted
	}

	expectNotEvicted := func(duration time.Duration) {
		ginkgo.GinkgoHelper()
		gomega.Consistently(func(g gomega.Gomega) {
			wl := getWorkload(g)
			g.Expect(workloadevict.IsEvicted(wl)).To(gomega.BeFalse())
			g.Expect(workload.IsAdmitted(wl)).To(gomega.BeTrue())
		}, duration, util.Interval).Should(gomega.Succeed())
	}

	requeuedByTest := func(message string) workloadpatching.UpdateFunc {
		return func(wl *kueue.Workload) (bool, error) {
			return workload.SetRequeuedCondition(wl, "ByTest", message, true), nil
		}
	}

	expectRequeuedMessage := func(message string) {
		ginkgo.GinkgoHelper()
		gomega.Expect(apimeta.FindStatusCondition(getWorkload(gomega.Default).Status.Conditions, kueue.WorkloadRequeued)).
			To(gomega.HaveValue(gomega.HaveField("Message", message)))
	}

	ginkgo.When("waitForPodsReady is configured with a short unschedulableTimeout", func() {
		ginkgo.BeforeEach(func() {
			features.SetFeatureGateDuringTest(ginkgo.GinkgoTB(), features.TopologyAwareScheduling, false)
			features.SetFeatureGateDuringTest(ginkgo.GinkgoTB(), features.SchedulerLibraryIntegration, false)
			backoffBaseSeconds = 10
			timeout = 5 * time.Minute
			unschedulableTimeout = &metav1.Duration{Duration: shortUnschedulableTimeout}
		})

		ginkgo.It("should evict the workload when a required pod is not scheduled within the unschedulableTimeout, and again after a re-admission", func() {
			ginkgo.By("creating an unscheduled pod linked to the workload")
			createLinkedPod()

			ginkgo.By("admitting the workload")
			admitWorkload()

			ginkgo.By("checking the workload annotations are injected into the pod template of the job")
			gomega.Eventually(func(g gomega.Gomega) {
				wl := getWorkload(g)
				job := &batchv1.Job{}
				g.Expect(k8sClient.Get(ctx, jobKey, job)).To(gomega.Succeed())
				g.Expect(job.Spec.Template.Annotations).To(gomega.HaveKeyWithValue(kueue.WorkloadAnnotation, wlKey.Name))
				g.Expect(job.Spec.Template.Annotations).To(gomega.HaveKeyWithValue(kueue.WorkloadUIDAnnotation, string(wl.UID)))
			}, util.Timeout, util.Interval).Should(gomega.Succeed())

			ginkgo.By("checking the tracker reports the unscheduled pod and the job framework propagates it")
			expectCurrentObservation(unscheduledPods)
			expectWorkloadConditions(podsReadyWaitForScheduling)

			ginkgo.By("checking the workload is evicted with the WaitForScheduling cause")
			expectEvicted(kueue.WorkloadWaitForScheduling, 1, util.Timeout+shortUnschedulableTimeout)

			ginkgo.By("checking the tracker resets the PodsScheduled condition to the eviction reason")
			expectWorkloadConditions(metav1.Condition{
				Type:    kueue.WorkloadPodsScheduled,
				Status:  metav1.ConditionFalse,
				Reason:  kueue.WorkloadEvictedByPodsReadyTimeout,
				Message: evictionMessage(),
			})

			ginkgo.By("checking the quota is released and the PodsReady condition is reset")
			gomega.Eventually(func(g gomega.Gomega) {
				wl := getWorkload(g)
				g.Expect(workload.HasQuotaReservation(wl)).To(gomega.BeFalse())
				g.Expect(wl.Status.Conditions).To(gomega.ContainElement(
					gomega.BeComparableTo(podsReadyWaitForStart, util.IgnoreConditionTimestampsAndObservedGeneration)))
			}, util.Timeout, util.Interval).Should(gomega.Succeed())

			ginkgo.By("admitting the workload again while the pod is still unscheduled")
			admitWorkload()

			ginkgo.By("checking the tracker observes the pod again for the new admission")
			expectCurrentObservation(unscheduledPods)
			expectWorkloadConditions(podsReadyWaitForScheduling)

			ginkgo.By("checking the workload is evicted again with the WaitForScheduling cause")
			expectEvicted(kueue.WorkloadWaitForScheduling, 2, util.Timeout+shortUnschedulableTimeout)
		})

		ginkgo.It("should keep the PodsScheduled condition of the tracker across server-side apply admission patches", func() {
			ginkgo.By("creating an unscheduled pod linked to the workload and admitting the workload")
			pod := createLinkedPod()
			admitWorkload()
			expectCurrentObservation(unscheduledPods)
			expectWorkloadConditions(podsReadyWaitForScheduling)

			ginkgo.By("taking copies of the workload before the tracker reports the pod as scheduled")
			looseCopy := getWorkload(gomega.Default)
			strictCopy := looseCopy.DeepCopy()

			ginkgo.By("scheduling the pod")
			markScheduled(pod)
			observation, _ := expectCurrentObservation(allPodsScheduled)
			expectWorkloadConditions(podsReadyWaitForStart)

			ginkgo.By("applying a loose admission patch from a stale copy")
			gomega.Expect(workloadpatching.PatchAdmissionStatus(ctx, k8sClient, looseCopy, util.RealClock,
				requeuedByTest("loose patch from a stale copy"), workloadpatching.WithLooseOnApply())).To(gomega.Succeed())
			expectPodsScheduledToEqual(observation)
			expectRequeuedMessage("loose patch from a stale copy")
			expectWorkloadConditions(podsReadyWaitForStart)

			ginkgo.By("applying a strict admission patch from a stale copy")
			err := workloadpatching.PatchAdmissionStatus(ctx, k8sClient, strictCopy, util.RealClock,
				requeuedByTest("strict patch from a stale copy"))
			gomega.Expect(apierrors.IsConflict(err)).To(gomega.BeTrue(), "expected a conflict, got: %v", err)
			expectPodsScheduledToEqual(observation)

			ginkgo.By("applying a strict admission patch from a fresh copy")
			gomega.Eventually(func(g gomega.Gomega) {
				g.Expect(workloadpatching.PatchAdmissionStatus(ctx, k8sClient, getWorkload(g), util.RealClock,
					requeuedByTest("strict patch from a fresh copy"))).To(gomega.Succeed())
			}, util.Timeout, util.Interval).Should(gomega.Succeed())
			expectPodsScheduledToEqual(observation)
			expectRequeuedMessage("strict patch from a fresh copy")

			ginkgo.By("checking the tracker is the only field manager of the PodsScheduled condition")
			managers, err := podsScheduledFieldManagers(getWorkload(gomega.Default))
			gomega.Expect(err).NotTo(gomega.HaveOccurred())
			gomega.Expect(managers).To(gomega.Equal(sets.New(trackerFieldManager)))
		})

		ginkgo.It("should keep the PodsScheduled condition of the tracker across merge patch admission patches", func() {
			features.SetFeatureGateDuringTest(ginkgo.GinkgoTB(), features.WorkloadRequestUseMergePatch, true)

			ginkgo.By("creating an unscheduled pod linked to the workload and admitting the workload")
			pod := createLinkedPod()
			admitWorkload()
			expectCurrentObservation(unscheduledPods)
			expectWorkloadConditions(podsReadyWaitForScheduling)

			ginkgo.By("taking copies of the workload before the tracker reports the pod as scheduled")
			looseCopy := getWorkload(gomega.Default)
			retriedCopy := looseCopy.DeepCopy()

			ginkgo.By("scheduling the pod")
			markScheduled(pod)
			observation, _ := expectCurrentObservation(allPodsScheduled)
			expectWorkloadConditions(podsReadyWaitForStart)

			ginkgo.By("applying a loose admission patch from a stale copy")
			err := workloadpatching.PatchAdmissionStatus(ctx, k8sClient, looseCopy, util.RealClock,
				requeuedByTest("loose patch from a stale copy"), workloadpatching.WithLooseOnApply())
			gomega.Expect(apierrors.IsConflict(err)).To(gomega.BeTrue(), "expected a conflict, got: %v", err)
			expectPodsScheduledToEqual(observation)

			ginkgo.By("applying a retried admission patch from a stale copy")
			gomega.Expect(workloadpatching.PatchAdmissionStatus(ctx, k8sClient, retriedCopy, util.RealClock,
				requeuedByTest("retried patch from a stale copy"),
				workloadpatching.WithLooseOnApply(), workloadpatching.WithRetryOnConflict())).To(gomega.Succeed())
			expectPodsScheduledToEqual(observation)
			expectRequeuedMessage("retried patch from a stale copy")

			ginkgo.By("applying an admission patch from a fresh copy")
			gomega.Eventually(func(g gomega.Gomega) {
				g.Expect(workloadpatching.PatchAdmissionStatus(ctx, k8sClient, getWorkload(g), util.RealClock,
					requeuedByTest("patch from a fresh copy"))).To(gomega.Succeed())
			}, util.Timeout, util.Interval).Should(gomega.Succeed())
			expectPodsScheduledToEqual(observation)
			expectRequeuedMessage("patch from a fresh copy")
		})

		ginkgo.It("should not apply the unschedulableTimeout once all the required pods were scheduled, even after a pod disappears", func() {
			ginkgo.By("creating a scheduled pod linked to the workload")
			pod := createLinkedPod()
			markScheduled(pod)

			ginkgo.By("admitting the workload")
			admitWorkload()

			ginkgo.By("checking the tracker reports all the pods scheduled while the workload waits for the pods to be ready")
			observation, _ := expectCurrentObservation(allPodsScheduled)
			expectWorkloadConditions(podsReadyWaitForStart)

			ginkgo.By("checking the workload is not evicted after the unschedulableTimeout")
			expectNotEvicted(shortUnschedulableTimeout + util.ShortTimeout)

			ginkgo.By("deleting the scheduled pod")
			util.ExpectObjectToBeDeleted(ctx, k8sClient, pod, true)

			ginkgo.By("checking the observation of the admission is kept")
			gomega.Consistently(func(g gomega.Gomega) {
				got := apimeta.FindStatusCondition(getWorkload(g).Status.Conditions, kueue.WorkloadPodsScheduled)
				g.Expect(got).To(gomega.HaveValue(gomega.BeComparableTo(observation, util.IgnoreConditionTimestamps)))
				g.Expect(got.LastTransitionTime.Time).To(gomega.BeTemporally("==", observation.LastTransitionTime.Time))
			}, pkgconstants.UpdatesBatchPeriod+util.ShortTimeout, util.Interval).Should(gomega.Succeed())
		})

		ginkgo.It("should keep the PodsScheduled observation while the workload waits for recovery", func() {
			ginkgo.By("creating a scheduled pod linked to the workload")
			pod := createLinkedPod()
			markScheduled(pod)

			ginkgo.By("admitting the workload")
			admitWorkload()
			observation, _ := expectCurrentObservation(allPodsScheduled)

			ginkgo.By("setting all job's pods to be ready")
			updateJobStatus(func(job *batchv1.Job) {
				job.Status.Active = 1
				job.Status.Ready = new(int32(1))
			})
			expectWorkloadConditions(podsReadyStarted)

			ginkgo.By("failing the pod and deleting it")
			updateJobStatus(func(job *batchv1.Job) {
				job.Status.Active = 0
				job.Status.Ready = new(int32(0))
				job.Status.Failed = 1
			})
			util.ExpectObjectToBeDeleted(ctx, k8sClient, pod, true)

			ginkgo.By("checking the workload waits for recovery with the observation of the admission kept")
			expectWorkloadConditions(podsReadyWaitForRecovery)
			expectPodsScheduledToEqual(observation)

			ginkgo.By("checking the workload is not evicted")
			expectNotEvicted(shortUnschedulableTimeout + util.ShortTimeout)
			expectPodsScheduledToEqual(observation)
		})

		ginkgo.It("should apply only the timeout when no pod is linked to the workload", func() {
			ginkgo.By("admitting the workload")
			admitWorkload()
			expectWorkloadConditions(podsReadyWaitForStart)

			ginkgo.By("checking the workload keeps waiting without a PodsScheduled condition")
			expectNotEvicted(shortUnschedulableTimeout + util.ShortTimeout)
			gomega.Expect(getWorkload(gomega.Default).Status.Conditions).NotTo(gomega.ContainElement(gomega.HaveField("Type", kueue.WorkloadPodsScheduled)))
		})

		ginkgo.It("should restart the job with the UID of the replacement workload when the workload is recreated", func() {
			ginkgo.By("admitting the workload")
			admitWorkload()
			original := getWorkload(gomega.Default)
			gomega.Eventually(func(g gomega.Gomega) {
				job := &batchv1.Job{}
				g.Expect(k8sClient.Get(ctx, jobKey, job)).To(gomega.Succeed())
				g.Expect(job.Spec.Template.Annotations).To(gomega.HaveKeyWithValue(kueue.WorkloadUIDAnnotation, string(original.UID)))
			}, util.Timeout, util.Interval).Should(gomega.Succeed())

			ginkgo.By("deleting the workload without the graceful stop of the job")
			gomega.Eventually(func(g gomega.Gomega) {
				deleted, err := workload.Delete(ctx, k8sClient, getWorkload(g))
				g.Expect(err).NotTo(gomega.HaveOccurred())
				g.Expect(deleted).To(gomega.BeTrue())
			}, util.Timeout, util.Interval).Should(gomega.Succeed())

			ginkgo.By("waiting for the job to be suspended with the stale annotations and the replacement workload to be created")
			gomega.Eventually(func(g gomega.Gomega) {
				job := &batchv1.Job{}
				g.Expect(k8sClient.Get(ctx, jobKey, job)).To(gomega.Succeed())
				g.Expect(job.Spec.Suspend).To(gomega.HaveValue(gomega.BeTrue()))
				g.Expect(job.Spec.Template.Annotations).To(gomega.HaveKeyWithValue(kueue.WorkloadUIDAnnotation, string(original.UID)))
			}, util.Timeout, util.Interval).Should(gomega.Succeed())
			var replacement *kueue.Workload
			gomega.Eventually(func(g gomega.Gomega) {
				replacement = getWorkload(g)
				g.Expect(replacement.UID).NotTo(gomega.Equal(original.UID))
			}, util.Timeout, util.Interval).Should(gomega.Succeed())

			ginkgo.By("admitting the replacement workload")
			admitWorkload()

			ginkgo.By("checking the pod template carries the UID of the replacement workload")
			gomega.Eventually(func(g gomega.Gomega) {
				job := &batchv1.Job{}
				g.Expect(k8sClient.Get(ctx, jobKey, job)).To(gomega.Succeed())
				g.Expect(job.Spec.Template.Annotations).To(gomega.HaveKeyWithValue(kueue.WorkloadAnnotation, wlKey.Name))
				g.Expect(job.Spec.Template.Annotations).To(gomega.HaveKeyWithValue(kueue.WorkloadUIDAnnotation, string(replacement.UID)))
			}, util.Timeout, util.Interval).Should(gomega.Succeed())
		})
	})

	ginkgo.When("waitForPodsReady is configured without unschedulableTimeout", func() {
		const podsReadyTimeout = 10 * time.Second

		ginkgo.BeforeEach(func() {
			backoffBaseSeconds = 10
			timeout = podsReadyTimeout
			unschedulableTimeout = nil
		})

		ginkgo.It("should evict the workload at the timeout with the WaitForScheduling cause", func() {
			ginkgo.By("creating an unscheduled pod linked to the workload and admitting the workload")
			createLinkedPod()
			admitWorkload()

			ginkgo.By("checking the tracker reports the unscheduled pod and the job framework propagates it")
			_, admittedAt := expectCurrentObservation(unscheduledPods)
			expectWorkloadConditions(podsReadyWaitForScheduling)

			ginkgo.By("checking the workload is evicted at the timeout")
			evicted := expectEvicted(kueue.WorkloadWaitForScheduling, 1, util.Timeout+podsReadyTimeout)
			gomega.Expect(evicted.LastTransitionTime.Time).To(gomega.BeTemporally(">=", admittedAt.Add(podsReadyTimeout)))
		})
	})

	ginkgo.When("unschedulableTimeout is equal to the timeout", func() {
		const podsReadyTimeout = 10 * time.Second

		ginkgo.BeforeEach(func() {
			backoffBaseSeconds = 10
			timeout = podsReadyTimeout
			unschedulableTimeout = &metav1.Duration{Duration: podsReadyTimeout}
		})

		ginkgo.It("should evict the workload at the timeout and not before", func() {
			ginkgo.By("creating an unscheduled pod linked to the workload and admitting the workload")
			createLinkedPod()
			admitWorkload()

			ginkgo.By("checking the tracker reports the unscheduled pod and the job framework propagates it")
			_, admittedAt := expectCurrentObservation(unscheduledPods)
			expectWorkloadConditions(podsReadyWaitForScheduling)

			ginkgo.By("checking the workload is evicted at the timeout since the admission")
			evicted := expectEvicted(kueue.WorkloadWaitForScheduling, 1, util.Timeout+podsReadyTimeout)
			gomega.Expect(evicted.LastTransitionTime.Time).To(gomega.BeTemporally(">=", admittedAt.Add(podsReadyTimeout)))
		})
	})

	ginkgo.When("waitForPodsReady is configured with a short unschedulableTimeout and no requeuing backoff", func() {
		ginkgo.BeforeEach(func() {
			backoffBaseSeconds = 0
			timeout = 5 * time.Minute
			unschedulableTimeout = &metav1.Duration{Duration: shortUnschedulableTimeout}
		})

		ginkgo.It("should time the re-admitted workload from its new observation", func() {
			ginkgo.By("creating an unscheduled pod linked to the workload and admitting the workload")
			createLinkedPod()
			admitWorkload()
			first, _ := expectCurrentObservation(unscheduledPods)
			expectWorkloadConditions(podsReadyWaitForScheduling)

			ginkgo.By("checking the workload is evicted with the WaitForScheduling cause")
			expectEvicted(kueue.WorkloadWaitForScheduling, 1, util.Timeout+shortUnschedulableTimeout)

			ginkgo.By("re-admitting the workload as soon as its quota reservation is released")
			gomega.Eventually(func(g gomega.Gomega) {
				wl := getWorkload(g)
				g.Expect(workload.HasQuotaReservation(wl)).To(gomega.BeFalse())
				g.Expect(workloadpatching.PatchAdmissionStatus(ctx, k8sClient, wl, util.RealClock, func(wl *kueue.Workload) (bool, error) {
					return workload.SetQuotaReservation(wl, admission, util.RealClock), nil
				})).To(gomega.Succeed())
			}, util.Timeout, util.Interval).Should(gomega.Succeed())
			util.ExpectJobUnsuspended(ctx, k8sClient, jobKey)

			ginkgo.By("checking the tracker observes the pod again for the new admission")
			second, _ := expectCurrentObservation(unscheduledPods)
			gomega.Expect(second.LastTransitionTime.Time).To(gomega.BeTemporally(">", first.LastTransitionTime.Time))
			expectWorkloadConditions(podsReadyWaitForScheduling)

			ginkgo.By("checking the workload is evicted again, timed from the new observation")
			evicted := expectEvicted(kueue.WorkloadWaitForScheduling, 2, util.Timeout+shortUnschedulableTimeout)
			gomega.Expect(evicted.LastTransitionTime.Time).To(gomega.BeTemporally(">=", second.LastTransitionTime.Add(shortUnschedulableTimeout)))
		})
	})

	ginkgo.When("the workload gets delegated to a MultiKueue worker while its pods are tracked", func() {
		const unschedulableTimeoutOnDelegation = 30 * time.Second

		ginkgo.BeforeEach(func() {
			features.SetFeatureGateDuringTest(ginkgo.GinkgoTB(), features.MultiKueue, true)
			features.SetFeatureGateDuringTest(ginkgo.GinkgoTB(), features.WorkloadRequestUseMergePatch, true)
			backoffBaseSeconds = 10
			timeout = 5 * time.Minute
			unschedulableTimeout = &metav1.Duration{Duration: unschedulableTimeoutOnDelegation}
		})

		ginkgo.It("should drop the local PodsScheduled observation and not apply the scheduling deadline", func() {
			ginkgo.By("creating an active MultiKueue admission check, not yet used by the cluster queue")
			ac = utiltestingapi.MakeAdmissionCheck("multikueue").ControllerName(kueue.MultiKueueControllerName).Obj()
			util.MustCreate(ctx, k8sClient, ac)
			util.SetAdmissionCheckActive(ctx, k8sClient, ac, metav1.ConditionTrue)

			ginkgo.By("creating an unscheduled pod linked to the workload and admitting the workload")
			createLinkedPod()
			admitWorkload()

			ginkgo.By("checking the tracker reports the unscheduled pod with a merge patch and the job framework propagates it")
			observation, _ := expectCurrentObservation(unscheduledPods)
			expectWorkloadConditions(podsReadyWaitForScheduling)
			deadline := observation.LastTransitionTime.Add(unschedulableTimeoutOnDelegation)

			ginkgo.By("switching the tracker to server-side apply")
			features.SetFeatureGateDuringTest(ginkgo.GinkgoTB(), features.WorkloadRequestUseMergePatch, false)

			ginkgo.By("attaching the admission check to the cluster queue")
			gomega.Eventually(func(g gomega.Gomega) {
				g.Expect(k8sClient.Get(ctx, client.ObjectKeyFromObject(cq), cq)).To(gomega.Succeed())
				cq.Spec.AdmissionChecksStrategy = &kueue.AdmissionChecksStrategy{
					AdmissionChecks: []kueue.AdmissionCheckStrategyRule{{Name: kueue.AdmissionCheckReference(ac.Name)}},
				}
				g.Expect(k8sClient.Update(ctx, cq)).To(gomega.Succeed())
			}, util.Timeout, util.Interval).Should(gomega.Succeed())

			ginkgo.By("waiting for the workload controller to add the admission check to the admitted workload")
			gomega.Eventually(func(g gomega.Gomega) {
				g.Expect(getWorkload(g).Status.AdmissionChecks).To(gomega.ContainElement(gomega.HaveField("Name", kueue.AdmissionCheckReference(ac.Name))))
			}, util.Timeout, util.Interval).Should(gomega.Succeed())

			ginkgo.By("checking the tracker drops the observation and the job framework falls back to WaitForStart")
			gomega.Eventually(func(g gomega.Gomega) {
				wl := getWorkload(g)
				g.Expect(wl.Status.Conditions).NotTo(gomega.ContainElement(gomega.HaveField("Type", kueue.WorkloadPodsScheduled)))
				g.Expect(wl.Status.Conditions).To(gomega.ContainElement(
					gomega.BeComparableTo(podsReadyWaitForStart, util.IgnoreConditionTimestampsAndObservedGeneration)))
			}, util.Timeout, util.Interval).Should(gomega.Succeed())
			gomega.Expect(time.Now()).To(gomega.BeTemporally("<", deadline),
				"the delegation must complete before the scheduling deadline for the guard to be verified")

			ginkgo.By("checking the workload is not evicted past the scheduling deadline")
			gomega.Consistently(func(g gomega.Gomega) {
				wl := getWorkload(g)
				g.Expect(workloadevict.IsEvicted(wl)).To(gomega.BeFalse())
				g.Expect(workload.IsAdmitted(wl)).To(gomega.BeTrue())
				g.Expect(wl.Status.Conditions).NotTo(gomega.ContainElement(gomega.HaveField("Type", kueue.WorkloadPodsScheduled)))
			}, time.Until(deadline.Add(evictionMargin)), util.Interval).Should(gomega.Succeed())
		})
	})
})
