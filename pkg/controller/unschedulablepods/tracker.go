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

package unschedulablepods

import (
	"context"
	"fmt"
	"slices"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	apimeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/util/workqueue"
	"k8s.io/klog/v2"
	"k8s.io/utils/clock"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/builder"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller"
	"sigs.k8s.io/controller-runtime/pkg/event"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/predicate"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"
	"sigs.k8s.io/controller-runtime/pkg/source"

	configapi "sigs.k8s.io/kueue/apis/config/v1beta2"
	kueue "sigs.k8s.io/kueue/apis/kueue/v1beta2"
	"sigs.k8s.io/kueue/pkg/constants"
	"sigs.k8s.io/kueue/pkg/controller/core"
	"sigs.k8s.io/kueue/pkg/controller/core/indexer"
	podconstants "sigs.k8s.io/kueue/pkg/controller/jobs/pod/constants"
	"sigs.k8s.io/kueue/pkg/features"
	"sigs.k8s.io/kueue/pkg/util/admissioncheck"
	"sigs.k8s.io/kueue/pkg/util/api"
	clientutil "sigs.k8s.io/kueue/pkg/util/client"
	utilpod "sigs.k8s.io/kueue/pkg/util/pod"
	"sigs.k8s.io/kueue/pkg/util/roletracker"
	"sigs.k8s.io/kueue/pkg/workload"
	"sigs.k8s.io/kueue/pkg/workload/concurrentadmission"
	workloadevict "sigs.k8s.io/kueue/pkg/workload/evict"
	workloadfinish "sigs.k8s.io/kueue/pkg/workload/finish"
	workloadpatching "sigs.k8s.io/kueue/pkg/workload/patching"
	"sigs.k8s.io/kueue/pkg/workloadslicing"
)

const (
	controllerName = "UnschedulablePodsTracker"

	fieldOwner = constants.KueueName + "-unschedulable-pods-tracker"

	// Wait one second so LastTransitionTime is strictly after the second-granularity admission time.
	admissionSettle = time.Second

	quotaReleasedMessage = "Quota reservation released"

	unscheduledPodsMessage = "At least one required pod is not scheduled"

	allPodsScheduledMessage = "All required pods were scheduled or succeeded"
)

type option func(*Tracker)

func withClock(c clock.Clock) option {
	return func(t *Tracker) {
		t.clock = c
	}
}

type Tracker struct {
	client      client.Client
	clock       clock.Clock
	roleTracker *roletracker.RoleTracker
}

var _ reconcile.Reconciler = (*Tracker)(nil)
var _ predicate.TypedPredicate[*kueue.Workload] = (*Tracker)(nil)

// +kubebuilder:rbac:groups="",resources=pods,verbs=get;list;watch
// +kubebuilder:rbac:groups=kueue.x-k8s.io,resources=workloads,verbs=get;list;watch
// +kubebuilder:rbac:groups=kueue.x-k8s.io,resources=workloads/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=kueue.x-k8s.io,resources=admissionchecks,verbs=get;list;watch

func NewTracker(c client.Client, roleTracker *roletracker.RoleTracker, opts ...option) *Tracker {
	t := &Tracker{
		client:      c,
		clock:       clock.RealClock{},
		roleTracker: roleTracker,
	}
	for _, opt := range opts {
		opt(t)
	}
	return t
}

func (t *Tracker) SetupWithManager(mgr ctrl.Manager, cfg *configapi.Configuration) (string, error) {
	return controllerName, builder.TypedControllerManagedBy[reconcile.Request](mgr).
		Named("unschedulable_pods_tracker").
		WatchesRawSource(source.TypedKind(
			mgr.GetCache(),
			&kueue.Workload{},
			&handler.TypedEnqueueRequestForObject[*kueue.Workload]{},
			t,
		)).
		Watches(&corev1.Pod{}, &podHandler{}).
		WithOptions(controller.Options{
			NeedLeaderElection:      new(false),
			MaxConcurrentReconciles: mgr.GetControllerOptions().GroupKindConcurrency[kueue.SchemeGroupVersion.WithKind("Workload").GroupKind().String()],
		}).
		WithLogConstructor(roletracker.NewLogConstructor(t.roleTracker, controllerName)).
		Complete(core.WithLeadingManager(mgr, t, &kueue.Workload{}, cfg))
}

func (t *Tracker) Reconcile(ctx context.Context, req reconcile.Request) (reconcile.Result, error) {
	log := ctrl.LoggerFrom(ctx)
	log.V(4).Info("Reconcile UnschedulablePodsTracker")

	wl := &kueue.Workload{}
	if err := t.client.Get(ctx, req.NamespacedName, wl); err != nil {
		if !apierrors.IsNotFound(err) {
			return reconcile.Result{}, err
		}
		if !features.Enabled(features.ElasticJobsViaWorkloadSlices) {
			return reconcile.Result{}, nil
		}
		active, err := workloadslicing.FindLatestAdmittedWorkloadForSlice(ctx, t.client, req.Namespace, req.Name)
		if err != nil || active == nil {
			return reconcile.Result{}, err
		}
		wl = active
	}

	active, err := t.activeSlice(ctx, wl)
	if err != nil {
		return reconcile.Result{}, err
	}
	if active != nil && active.Name != wl.Name {
		wl = active
	}
	if workloadfinish.IsFinished(wl) {
		return reconcile.Result{}, nil
	}
	if features.Enabled(features.MultiKueue) {
		skip, err := admissioncheck.ShouldSkipLocalExecution(ctx, t.client, wl)
		if err != nil {
			return reconcile.Result{}, err
		}
		if skip {
			return reconcile.Result{}, t.removeCondition(ctx, wl)
		}
	}
	if !shouldTrack(wl) {
		if reason, message, ok := lifecycleTarget(wl); ok {
			log.V(3).Info("Resetting the PodsScheduled condition", "reason", reason)
			return reconcile.Result{}, t.patchCondition(ctx, wl, t.lifecycleCondition(wl, reason, message))
		}
		return reconcile.Result{}, nil
	}

	admittedAt := apimeta.FindStatusCondition(wl.Status.Conditions, kueue.WorkloadAdmitted).LastTransitionTime.Time
	now := t.clock.Now()
	if settleAt := admittedAt.Add(admissionSettle); now.Before(settleAt) {
		return reconcile.Result{RequeueAfter: settleAt.Sub(now)}, nil
	}

	current := workload.CurrentPodsScheduledCondition(wl, admittedAt)
	if current != nil && current.Status == metav1.ConditionTrue {
		return reconcile.Result{}, nil
	}
	pods, err := t.listPods(ctx, wl)
	if err != nil {
		return reconcile.Result{}, err
	}
	if len(pods) == 0 && current == nil {
		log.V(4).Info("No pods observed for the workload; leaving the PodsScheduled condition unset")
		return reconcile.Result{}, nil
	}

	summary := summarizeScheduling(wl, pods)
	if current == nil && summary.nonTerminal == 0 && !summary.succeededFillsGrant {
		log.V(4).Info("No live pods observed for the workload; leaving the PodsScheduled condition unset")
		return reconcile.Result{}, nil
	}
	if current != nil && !summary.allScheduled() {
		log.V(5).Info("PodsScheduled condition is up-to-date")
		return reconcile.Result{}, nil
	}
	condition := podsScheduledCondition(summary)
	condition.ObservedGeneration = wl.Generation
	condition.LastTransitionTime = metav1.NewTime(now)
	log.V(3).Info("Updating the PodsScheduled condition", "status", condition.Status, "reason", condition.Reason)
	return reconcile.Result{}, t.patchCondition(ctx, wl, condition)
}

func (t *Tracker) activeSlice(ctx context.Context, wl *kueue.Workload) (*kueue.Workload, error) {
	if !features.Enabled(features.ElasticJobsViaWorkloadSlices) || !workloadslicing.IsElasticWorkload(wl) {
		return nil, nil
	}
	return workloadslicing.FindLatestAdmittedWorkloadForSlice(ctx, t.client, wl.Namespace, workloadslicing.SliceName(wl))
}

func lifecycleTarget(wl *kueue.Workload) (reason, message string, ok bool) {
	if workloadfinish.IsFinished(wl) || (features.Enabled(features.ConcurrentAdmission) && concurrentadmission.IsVariant(wl)) {
		return "", "", false
	}
	cond := apimeta.FindStatusCondition(wl.Status.Conditions, kueue.WorkloadPodsScheduled)
	if cond == nil {
		return "", "", false
	}
	switch {
	case workloadevict.IsEvicted(wl):
		evicted := apimeta.FindStatusCondition(wl.Status.Conditions, kueue.WorkloadEvicted)
		reason, message = evicted.Reason, evicted.Message
	case !workload.HasQuotaReservation(wl):
		quotaReserved := apimeta.FindStatusCondition(wl.Status.Conditions, kueue.WorkloadQuotaReserved)
		if quotaReserved == nil {
			return "", "", false
		}
		reason, message = quotaReserved.Reason, quotaReserved.Message
	default:
		return "", "", false
	}
	if message == "" {
		message = quotaReleasedMessage
	}
	message = api.TruncateConditionMessage(message)
	if cond.Status == metav1.ConditionFalse && cond.Reason == reason && cond.Message == message {
		return "", "", false
	}
	return reason, message, true
}

func (t *Tracker) lifecycleCondition(wl *kueue.Workload, reason, message string) metav1.Condition {
	cond := apimeta.FindStatusCondition(wl.Status.Conditions, kueue.WorkloadPodsScheduled)
	transition := metav1.NewTime(t.clock.Now())
	if cond.Status == metav1.ConditionFalse {
		transition = cond.LastTransitionTime
	}
	return metav1.Condition{
		Type:               kueue.WorkloadPodsScheduled,
		Status:             metav1.ConditionFalse,
		Reason:             reason,
		Message:            message,
		ObservedGeneration: wl.Generation,
		LastTransitionTime: transition,
	}
}

func (t *Tracker) patchCondition(ctx context.Context, wl *kueue.Workload, condition metav1.Condition) error {
	return workloadpatching.PatchStatus(ctx, t.client, wl, client.FieldOwner(fieldOwner), func(wl *kueue.Workload) (bool, error) {
		apimeta.RemoveStatusCondition(&wl.Status.Conditions, condition.Type)
		apimeta.SetStatusCondition(&wl.Status.Conditions, condition)
		return true, nil
	})
}

// Use merge patch because apply cannot remove a condition the tracker does not own.
func (t *Tracker) removeCondition(ctx context.Context, wl *kueue.Workload) error {
	if apimeta.FindStatusCondition(wl.Status.Conditions, kueue.WorkloadPodsScheduled) == nil {
		return nil
	}
	ctrl.LoggerFrom(ctx).V(3).Info("Removing the PodsScheduled condition")
	return clientutil.PatchStatus(ctx, t.client, wl, func() (bool, error) {
		return apimeta.RemoveStatusCondition(&wl.Status.Conditions, kueue.WorkloadPodsScheduled), nil
	})
}

func (t *Tracker) listPods(ctx context.Context, wl *kueue.Workload) ([]corev1.Pod, error) {
	var pods corev1.PodList
	if err := t.client.List(ctx, &pods,
		client.InNamespace(wl.Namespace),
		client.MatchingFields{indexer.WorkloadSliceNameKey: workloadslicing.SliceName(wl)},
	); err != nil {
		return nil, fmt.Errorf("listing pods for workload %s: %w", klog.KObj(wl), err)
	}
	return slices.DeleteFunc(pods.Items, func(pod corev1.Pod) bool {
		uid, found := pod.Annotations[kueue.WorkloadUIDAnnotation]
		return found && uid != string(wl.UID) && !matchedByName(wl, &pod)
	}), nil
}

func matchedByName(wl *kueue.Workload, pod *corev1.Pod) bool {
	elastic := workloadslicing.IsElasticWorkload(wl) && pod.Annotations[kueue.WorkloadSliceNameAnnotation] == workloadslicing.SliceName(wl)
	group := wl.Annotations[podconstants.IsGroupWorkloadAnnotationKey] == podconstants.IsGroupWorkloadAnnotationValue
	return elastic || group
}

type schedulingSummary struct {
	scheduled           int64
	required            int64
	nonTerminal         int64
	succeededFillsGrant bool
}

func (s schedulingSummary) allScheduled() bool {
	return s.scheduled >= s.required
}

func summarizeScheduling(wl *kueue.Workload, pods []corev1.Pod) schedulingSummary {
	var summary schedulingSummary
	activeScheduled := make(map[kueue.PodSetReference]int64)
	succeeded := make(map[kueue.PodSetReference]int64)
	for i := range pods {
		pod := &pods[i]
		podSet := kueue.PodSetReference(pod.Labels[constants.PodSetLabel])
		switch {
		case pod.Status.Phase == corev1.PodSucceeded:
			succeeded[podSet]++
		case pod.Status.Phase == corev1.PodFailed:
		case pod.DeletionTimestamp != nil:
		default:
			summary.nonTerminal++
			if utilpod.IsScheduled(pod) {
				activeScheduled[podSet]++
			}
		}
	}
	reclaimable := make(map[kueue.PodSetReference]int64)
	if features.Enabled(features.ReclaimablePods) {
		for _, rp := range wl.Status.ReclaimablePods {
			reclaimable[rp.Name] = int64(rp.Count)
		}
	}
	var succeededCapped int64
	for name, granted := range workload.ExtractGrantedPodSetCounts(wl) {
		required := int64(granted)
		summary.required += required
		summary.scheduled += min(required, activeScheduled[name]+max(succeeded[name], reclaimable[name]))
		succeededCapped += min(required, succeeded[name])
	}
	summary.succeededFillsGrant = summary.required > 0 && succeededCapped == summary.required
	return summary
}

func podsScheduledCondition(summary schedulingSummary) metav1.Condition {
	condition := metav1.Condition{
		Type:    kueue.WorkloadPodsScheduled,
		Status:  metav1.ConditionFalse,
		Reason:  kueue.WorkloadWaitForScheduling,
		Message: unscheduledPodsMessage,
	}
	if summary.allScheduled() {
		condition.Status = metav1.ConditionTrue
		condition.Reason = kueue.WorkloadAllRequiredPodsScheduled
		condition.Message = allPodsScheduledMessage
	}
	return condition
}

func shouldTrack(wl *kueue.Workload) bool {
	if features.Enabled(features.ConcurrentAdmission) && concurrentadmission.IsVariant(wl) {
		return false
	}
	return workload.IsAdmitted(wl) && !workloadfinish.IsFinished(wl) && !workloadevict.IsEvicted(wl)
}

func hasPodsScheduledCondition(wl *kueue.Workload) bool {
	return apimeta.FindStatusCondition(wl.Status.Conditions, kueue.WorkloadPodsScheduled) != nil
}

func (t *Tracker) Create(e event.TypedCreateEvent[*kueue.Workload]) bool {
	return shouldTrack(e.Object) || hasPodsScheduledCondition(e.Object)
}

func (t *Tracker) Update(e event.TypedUpdateEvent[*kueue.Workload]) bool {
	return shouldTrack(e.ObjectNew) || hasPodsScheduledCondition(e.ObjectNew)
}

func (t *Tracker) Delete(event.TypedDeleteEvent[*kueue.Workload]) bool {
	return false
}

func (t *Tracker) Generic(event.TypedGenericEvent[*kueue.Workload]) bool {
	return false
}

var _ handler.EventHandler = (*podHandler)(nil)

type podHandler struct{}

func (h *podHandler) Create(ctx context.Context, e event.CreateEvent, q workqueue.TypedRateLimitingInterface[reconcile.Request]) {
	h.queueReconcileForPod(ctx, e.Object, q)
}

func (h *podHandler) Update(ctx context.Context, e event.UpdateEvent, q workqueue.TypedRateLimitingInterface[reconcile.Request]) {
	oldPod, isPod := e.ObjectOld.(*corev1.Pod)
	newPod, isNewPod := e.ObjectNew.(*corev1.Pod)
	if !isPod || !isNewPod || !schedulingChanged(oldPod, newPod) {
		return
	}
	oldKey, oldFound := workloadKeyForPod(oldPod)
	newKey, newFound := workloadKeyForPod(newPod)
	if oldFound && (!newFound || oldKey != newKey) {
		queueReconcile(ctx, oldPod, oldKey, q)
	}
	if newFound {
		queueReconcile(ctx, newPod, newKey, q)
	}
}

func (h *podHandler) Delete(ctx context.Context, e event.DeleteEvent, q workqueue.TypedRateLimitingInterface[reconcile.Request]) {
	h.queueReconcileForPod(ctx, e.Object, q)
}

func (h *podHandler) Generic(context.Context, event.GenericEvent, workqueue.TypedRateLimitingInterface[reconcile.Request]) {
}

func (h *podHandler) queueReconcileForPod(ctx context.Context, object client.Object, q workqueue.TypedRateLimitingInterface[reconcile.Request]) {
	pod, isPod := object.(*corev1.Pod)
	if !isPod {
		return
	}
	if key, found := workloadKeyForPod(pod); found {
		queueReconcile(ctx, pod, key, q)
	}
}

func queueReconcile(ctx context.Context, pod *corev1.Pod, key types.NamespacedName, q workqueue.TypedRateLimitingInterface[reconcile.Request]) {
	ctrl.LoggerFrom(ctx).V(5).Info("Queueing reconcile for workload", "pod", klog.KObj(pod), "workload", key.String())
	q.AddAfter(reconcile.Request{NamespacedName: key}, constants.UpdatesBatchPeriod)
}

func workloadKeyForPod(pod *corev1.Pod) (types.NamespacedName, bool) {
	if name, found := pod.Annotations[kueue.WorkloadSliceNameAnnotation]; found && name != "" {
		return types.NamespacedName{Namespace: pod.Namespace, Name: name}, true
	}
	if name, found := pod.Annotations[kueue.WorkloadAnnotation]; found && name != "" {
		return types.NamespacedName{Namespace: pod.Namespace, Name: name}, true
	}
	return types.NamespacedName{}, false
}

func schedulingChanged(oldPod, newPod *corev1.Pod) bool {
	return utilpod.IsScheduled(oldPod) != utilpod.IsScheduled(newPod) ||
		oldPod.Status.Phase != newPod.Status.Phase ||
		(oldPod.DeletionTimestamp == nil) != (newPod.DeletionTimestamp == nil) ||
		oldPod.Annotations[kueue.WorkloadAnnotation] != newPod.Annotations[kueue.WorkloadAnnotation] ||
		oldPod.Annotations[kueue.WorkloadSliceNameAnnotation] != newPod.Annotations[kueue.WorkloadSliceNameAnnotation] ||
		oldPod.Annotations[kueue.WorkloadUIDAnnotation] != newPod.Annotations[kueue.WorkloadUIDAnnotation] ||
		oldPod.Labels[constants.PodSetLabel] != newPod.Labels[constants.PodSetLabel]
}
