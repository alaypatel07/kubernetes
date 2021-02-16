/*
Copyright 2016 The Kubernetes Authors.

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

package cronjob

/*
I did not use watch or expectations.  Those add a lot of corner cases, and we aren't
expecting a large volume of jobs or cronJobs.  (We are favoring correctness
over scalability.  If we find a single controller thread is too slow because
there are a lot of Jobs or CronJobs, we can parallelize by Namespace.
If we find the load on the API server is too high, we can use a watch and
UndeltaStore.)

Just periodically list jobs and cronJobs, and then reconcile them.
*/

import (
	"context"
	"fmt"
	"reflect"
	"sort"
	"time"

	"k8s.io/klog/v2"

	batchv1 "k8s.io/api/batch/v1"
	batchv1beta1 "k8s.io/api/batch/v1beta1"
	"k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	"k8s.io/apimachinery/pkg/util/wait"
	batchv1informers "k8s.io/client-go/informers/batch/v1"
	batchv1beta1informers "k8s.io/client-go/informers/batch/v1beta1"
	clientset "k8s.io/client-go/kubernetes"
	"k8s.io/client-go/kubernetes/scheme"
	v1core "k8s.io/client-go/kubernetes/typed/core/v1"
	batchv1listers "k8s.io/client-go/listers/batch/v1"
	"k8s.io/client-go/listers/batch/v1beta1"
	"k8s.io/client-go/tools/cache"
	"k8s.io/client-go/tools/pager"
	"k8s.io/client-go/tools/record"
	ref "k8s.io/client-go/tools/reference"
	"k8s.io/client-go/util/workqueue"
	"k8s.io/component-base/metrics/prometheus/ratelimiter"
	"k8s.io/kubernetes/pkg/apis/batch"
	"k8s.io/kubernetes/pkg/controller"
)

// Utilities for dealing with Jobs and CronJobs and time.

// controllerKind contains the schema.GroupVersionKind for this controller type.
var controllerKind = batchv1beta1.SchemeGroupVersion.WithKind("CronJob")

// Controller is a controller for CronJobs.
type Controller struct {
	kubeClient clientset.Interface
	queue      workqueue.DelayingInterface
	recorder   record.EventRecorder

	jobControl     jobControlInterface
	cronJobControl cjControlInterface

	jobLister     batchv1listers.JobLister
	cronJobLister v1beta1.CronJobLister

	jobListerSynced     cache.InformerSynced
	cronJobListerSynced cache.InformerSynced
}

// NewController creates and initializes a new Controller.
func NewController(jobInformer batchv1informers.JobInformer, cronJobsInformer batchv1beta1informers.CronJobInformer, kubeClient clientset.Interface) (*Controller, error) {
	eventBroadcaster := record.NewBroadcaster()
	eventBroadcaster.StartStructuredLogging(0)
	eventBroadcaster.StartRecordingToSink(&v1core.EventSinkImpl{Interface: kubeClient.CoreV1().Events("")})

	if kubeClient != nil && kubeClient.CoreV1().RESTClient().GetRateLimiter() != nil {
		if err := ratelimiter.RegisterMetricAndTrackRateLimiterUsage("cronjob_controller", kubeClient.CoreV1().RESTClient().GetRateLimiter()); err != nil {
			return nil, err
		}
	}

	jm := &Controller{
		kubeClient: kubeClient,
		queue:      workqueue.NewNamedRateLimitingQueue(workqueue.NewItemExponentialFailureRateLimiter(DefaultCronJobBackOff, MaxCronJobBackOff), "cronjob"),
		recorder:   eventBroadcaster.NewRecorder(scheme.Scheme, v1.EventSource{Component: "cronjob-controller"}),

		jobControl:     realJobControl{KubeClient: kubeClient},
		cronJobControl: &realCJControl{KubeClient: kubeClient},

		jobLister:     jobInformer.Lister(),
		cronJobLister: cronJobsInformer.Lister(),

		jobListerSynced:     jobInformer.Informer().HasSynced,
		cronJobListerSynced: cronJobsInformer.Informer().HasSynced,
	}

	jobInformer.Informer().AddEventHandler(cache.ResourceEventHandlerFuncs{
		AddFunc:    jm.addJob,
		UpdateFunc: jm.updateJob,
		DeleteFunc: jm.deleteJob,
	})

	cronJobsInformer.Informer().AddEventHandler(cache.ResourceEventHandlerFuncs{
		AddFunc: func(obj interface{}) {
			jm.enqueueController(obj)
		},
		UpdateFunc: func(_, curr interface{}) {
			// TODO: @alpatel need to figure out if inspection and queueing of old cronjob
			//  is required.
			jm.enqueueController(curr)
		},
		DeleteFunc: func(obj interface{}) {
			jm.enqueueController(obj)
		},
	})

	return jm, nil
}

// Run starts the main goroutine responsible for watching and syncing jobs.
func (jm *Controller) Run(workers int, stopCh <-chan struct{}) {
	defer utilruntime.HandleCrash()
	klog.Infof("Starting cronjob controller")

	if !cache.WaitForNamedCacheSync("job", stopCh, jm.jobListerSynced, jm.cronJobListerSynced) {
		return
	}

	for i := 0; i < workers; i++ {
		go wait.Until(jm.worker, time.Second, stopCh)
	}

	<-stopCh
	klog.Infof("Shutting down CronJob Manager")
}

func (jm *Controller) worker() {
	for jm.processNextWorkItem() {
	}
}

func (jm *Controller) processNextWorkItem() bool {
	key, quit := jm.queue.Get()
	if quit {
		return false
	}
	defer jm.queue.Done(key)
	err, requeueAfter := jm.sync(key.(string))
	switch {
	case err != nil:
		utilruntime.HandleError(fmt.Errorf("Error syncing CronJobController %v, requeuing: %v", key.(string), err))
		jm.queue.Add(key)
	case requeueAfter != nil:
		jm.queue.AddAfter(key, *requeueAfter)
	default:
		jm.queue.Done(key)
	}
	return true
}

func (jm *Controller) sync(cronJobKey string) (error, *time.Duration) {
	ns, name, err := cache.SplitMetaNamespaceKey(cronJobKey)
	if err != nil {
		return err, nil
	}

	cronJob, err := jm.cronJobLister.CronJobs(ns).Get(name)
	switch {
	case errors.IsNotFound(err):
		// may be cronjob is deleted, dont need to requeue this key
		nameForLog := fmt.Sprintf("%s/%s", ns, name)
		klog.Warningf("cronjob not found, may be it is deleted", nameForLog, err)
		return nil, nil
	case err != nil:
		// for other transient apiserver error requeue with exponential backoff
		return err, nil
	}

	jobList, err := jm.jobLister.Jobs(ns).List(labels.Everything())
	if err != nil {
		return err, nil
	}

	jobsToBeReconciled := []batchv1.Job{}

	for _, job := range jobList {
		// If it has a ControllerRef, that's all that matters.
		if controllerRef := metav1.GetControllerOf(job); controllerRef != nil && controllerRef.Name == name {
			// this job is needs to be reconciled
			jobsToBeReconciled = append(jobsToBeReconciled, *job)
		}
	}

	err, requeueAfter := syncOne(cronJob, jobsToBeReconciled, time.Now(), jm.jobControl, jm.cronJobControl, jm.recorder)
	// TODO: @alpatel need to rework this function to handle re-queuing on errors
	cleanupFinishedJobs(cronJob, jobsToBeReconciled, jm.jobControl, jm.cronJobControl, jm.recorder)
	switch {
	case err != nil:
		return err, nil
	case requeueAfter != nil:
		return nil, requeueAfter
	default:
		// TODO: @alpatel, figure out how to handle this case
	}
	return nil, nil
}

// resolveControllerRef returns the controller referenced by a ControllerRef,
// or nil if the ControllerRef could not be resolved to a matching controller
// of the correct Kind.
func (jm *Controller) resolveControllerRef(namespace string, controllerRef *metav1.OwnerReference) *batchv1beta1.CronJob {
	// We can't look up by UID, so look up by Name and then verify UID.
	// Don't even try to look up by Name if it's the wrong Kind.
	if controllerRef.Kind != controllerKind.Kind {
		return nil
	}
	cronJob, err := jm.cronJobLister.CronJobs(namespace).Get(controllerRef.Name)
	if err != nil {
		return nil
	}
	if cronJob.UID != controllerRef.UID {
		// The controller we found with this Name is not the same one that the
		// ControllerRef points to.
		return nil
	}
	return cronJob
}

// When a job is created, enqueue the controller that manages it and update it's expectations.
func (jm *Controller) addJob(obj interface{}) {
	job := obj.(*batchv1.Job)
	if job.DeletionTimestamp != nil {
		// on a restart of the controller controller, it's possible a new job shows up in a state that
		// is already pending deletion. Prevent the job from being a creation observation.
		jm.deleteJob(job)
		return
	}

	// If it has a ControllerRef, that's all that matters.
	if controllerRef := metav1.GetControllerOf(job); controllerRef != nil {
		cronJob := jm.resolveControllerRef(job.Namespace, controllerRef)
		if cronJob == nil {
			return
		}
		jm.enqueueController(cronJob)
		return
	}
}

// updateJob figures out what CronJob(s) manage a Job when the Job
// is updated and wake them up. If the anything of the Job have changed, we need to
// awaken both the old and new CronJob. old and cur must be *batchv1.Job
// types.
func (jm *Controller) updateJob(old, cur interface{}) {
	curJob := cur.(*batch.Job)
	oldJob := old.(*batch.Job)
	if curJob.ResourceVersion == oldJob.ResourceVersion {
		// Periodic resync will send update events for all known jobs.
		// Two different versions of the same jobs will always have different RVs.
		return
	}

	curControllerRef := metav1.GetControllerOf(curJob)
	oldControllerRef := metav1.GetControllerOf(oldJob)
	controllerRefChanged := !reflect.DeepEqual(curControllerRef, oldControllerRef)
	if controllerRefChanged && oldControllerRef != nil {
		// The ControllerRef was changed. Sync the old controller, if any.
		if cronJob := jm.resolveControllerRef(oldJob.Namespace, oldControllerRef); cronJob != nil {
			jm.enqueueController(cronJob)
		}
	}

	// If it has a ControllerRef, that's all that matters.
	if curControllerRef != nil {
		cronJob := jm.resolveControllerRef(curJob.Namespace, curControllerRef)
		if cronJob == nil {
			return
		}
		jm.enqueueController(cronJob)
		return
	}
}

func (jm *Controller) deleteJob(obj interface{}) {
	job, ok := obj.(*batchv1.Job)

	// When a delete is dropped, the relist will notice a job in the store not
	// in the list, leading to the insertion of a tombstone object which contains
	// the deleted key/value. Note that this value might be stale.
	if !ok {
		tombstone, ok := obj.(cache.DeletedFinalStateUnknown)
		if !ok {
			utilruntime.HandleError(fmt.Errorf("couldn't get object from tombstone %#v", obj))
			return
		}
		job, ok = tombstone.Obj.(*batchv1.Job)
		if !ok {
			utilruntime.HandleError(fmt.Errorf("tombstone contained object that is not a ReplicaSet %#v", obj))
			return
		}
	}

	controllerRef := metav1.GetControllerOf(job)
	if controllerRef == nil {
		// No controller should care about orphans being deleted.
		return
	}
	cronJob := jm.resolveControllerRef(job.Namespace, controllerRef)
	if cronJob == nil {
		return
	}
	jm.enqueueController(cronJob)
}

func (jm *Controller) enqueueController(obj interface{}) {
	key, err := controller.KeyFunc(obj)
	if err != nil {
		utilruntime.HandleError(fmt.Errorf("couldn't get key for object %+v: %v", obj, err))
		return
	}

	jm.queue.Add(key)
}

// syncAll lists all the CronJobs and Jobs and reconciles them.
// TODO: @alpatel remove this function eventually since it is not used
func (jm *Controller) syncAll() {
	// List children (Jobs) before parents (CronJob).
	// This guarantees that if we see any Job that got orphaned by the GC orphan finalizer,
	// we must also see that the parent CronJob has non-nil DeletionTimestamp (see #42639).
	// Note that this only works because we are NOT using any caches here.
	jobListFunc := func(opts metav1.ListOptions) (runtime.Object, error) {
		return jm.kubeClient.BatchV1().Jobs(metav1.NamespaceAll).List(context.TODO(), opts)
	}

	js := make([]batchv1.Job, 0)
	err := pager.New(pager.SimplePageFunc(jobListFunc)).EachListItem(context.Background(), metav1.ListOptions{}, func(object runtime.Object) error {
		jobTmp, ok := object.(*batchv1.Job)
		if !ok {
			return fmt.Errorf("expected type *batchv1.Job, got type %T", jobTmp)
		}
		js = append(js, *jobTmp)
		return nil
	})
	if err != nil {
		utilruntime.HandleError(fmt.Errorf("Failed to extract job list: %v", err))
		return
	}
	klog.V(4).Infof("Found %d jobs", len(js))

	jobsByCj := groupJobsByParent(js)
	klog.V(4).Infof("Found %d groups", len(jobsByCj))

	err = pager.New(func(ctx context.Context, opts metav1.ListOptions) (runtime.Object, error) {
		return jm.kubeClient.BatchV1beta1().CronJobs(metav1.NamespaceAll).List(ctx, opts)
	}).EachListItem(context.Background(), metav1.ListOptions{}, func(object runtime.Object) error {
		cj, ok := object.(*batchv1beta1.CronJob)
		if !ok {
			return fmt.Errorf("expected type *batchv1beta1.CronJob, got type %T", cj)
		}
		syncOne(cj, jobsByCj[cj.UID], time.Now(), jm.jobControl, jm.cronJobControl, jm.recorder)
		cleanupFinishedJobs(cj, jobsByCj[cj.UID], jm.jobControl, jm.cronJobControl, jm.recorder)
		return nil
	})

	if err != nil {
		utilruntime.HandleError(fmt.Errorf("Failed to extract cronJobs list: %v", err))
		return
	}
}

// cleanupFinishedJobs cleanups finished jobs created by a CronJob
func cleanupFinishedJobs(cj *batchv1beta1.CronJob, js []batchv1.Job, jc jobControlInterface,
	cjc cjControlInterface, recorder record.EventRecorder) {
	// If neither limits are active, there is no need to do anything.
	if cj.Spec.FailedJobsHistoryLimit == nil && cj.Spec.SuccessfulJobsHistoryLimit == nil {
		return
	}

	failedJobs := []batchv1.Job{}
	successfulJobs := []batchv1.Job{}

	for _, job := range js {
		isFinished, finishedStatus := getFinishedStatus(&job)
		if isFinished && finishedStatus == batchv1.JobComplete {
			successfulJobs = append(successfulJobs, job)
		} else if isFinished && finishedStatus == batchv1.JobFailed {
			failedJobs = append(failedJobs, job)
		}
	}

	if cj.Spec.SuccessfulJobsHistoryLimit != nil {
		removeOldestJobs(cj,
			successfulJobs,
			jc,
			*cj.Spec.SuccessfulJobsHistoryLimit,
			recorder)
	}

	if cj.Spec.FailedJobsHistoryLimit != nil {
		removeOldestJobs(cj,
			failedJobs,
			jc,
			*cj.Spec.FailedJobsHistoryLimit,
			recorder)
	}

	// Update the CronJob, in case jobs were removed from the list.
	if _, err := cjc.UpdateStatus(cj); err != nil {
		nameForLog := fmt.Sprintf("%s/%s", cj.Namespace, cj.Name)
		klog.Infof("Unable to update status for %s (rv = %s): %v", nameForLog, cj.ResourceVersion, err)
	}
}

// removeOldestJobs removes the oldest jobs from a list of jobs
func removeOldestJobs(cj *batchv1beta1.CronJob, js []batchv1.Job, jc jobControlInterface, maxJobs int32, recorder record.EventRecorder) {
	numToDelete := len(js) - int(maxJobs)
	if numToDelete <= 0 {
		return
	}

	nameForLog := fmt.Sprintf("%s/%s", cj.Namespace, cj.Name)
	klog.V(4).Infof("Cleaning up %d/%d jobs from %s", numToDelete, len(js), nameForLog)

	sort.Sort(byJobStartTime(js))
	for i := 0; i < numToDelete; i++ {
		klog.V(4).Infof("Removing job %s from %s", js[i].Name, nameForLog)
		deleteJob(cj, &js[i], jc, recorder)
	}
}

// TODO: @alpatel we need to return errors from this function, in order to allow
//  for errors to propagate up the reconcile function and kick in queues exponential
//  backed off retries more details on inline code comments in the func

// syncOne reconciles a CronJob with a list of any Jobs that it created.
// All known jobs created by "cj" should be included in "js".
// The current time is passed in to facilitate testing.
// It has no receiver, to facilitate testing.
func syncOne(
	cj *batchv1beta1.CronJob,
	js []batchv1.Job,
	now time.Time,
	jc jobControlInterface,
	cjc cjControlInterface,
	recorder record.EventRecorder) (error, *time.Duration) {
	nameForLog := fmt.Sprintf("%s/%s", cj.Namespace, cj.Name)

	childrenJobs := make(map[types.UID]bool)
	for _, j := range js {
		childrenJobs[j.ObjectMeta.UID] = true
		found := inActiveList(*cj, j.ObjectMeta.UID)
		if !found && !IsJobFinished(&j) {
			recorder.Eventf(cj, v1.EventTypeWarning, "UnexpectedJob", "Saw a job that the controller did not create or forgot: %s", j.Name)
			// We found an unfinished job that has us as the parent, but it is not in our Active list.
			// This could happen if we crashed right after creating the Job and before updating the status,
			// or if our jobs list is newer than our cj status after a relist, or if someone intentionally created
			// a job that they wanted us to adopt.

			// TODO: maybe handle the adoption case?  Concurrency/suspend rules will not apply in that case, obviously, since we can't
			// stop users from creating jobs if they have permission.  It is assumed that if a
			// user has permission to create a job within a namespace, then they have permission to make any cronJob
			// in the same namespace "adopt" that job.  ReplicaSets and their Pods work the same way.
			// TBS: how to update cj.Status.LastScheduleTime if the adopted job is newer than any we knew about?
		} else if found && IsJobFinished(&j) {
			_, status := getFinishedStatus(&j)
			deleteFromActiveList(cj, j.ObjectMeta.UID)
			recorder.Eventf(cj, v1.EventTypeNormal, "SawCompletedJob", "Saw completed job: %s, status: %v", j.Name, status)
		}
	}

	// Remove any job reference from the active list if the corresponding job does not exist any more.
	// Otherwise, the cronjob may be stuck in active mode forever even though there is no matching
	// job running.
	for _, j := range cj.Status.Active {
		if found := childrenJobs[j.UID]; !found {
			recorder.Eventf(cj, v1.EventTypeNormal, "MissingJob", "Active job went missing: %v", j.Name)
			deleteFromActiveList(cj, j.UID)
		}
	}

	updatedCJ, err := cjc.UpdateStatus(cj)
	if err != nil {
		klog.Errorf("Unable to update status for %s (rv = %s): %v", nameForLog, cj.ResourceVersion, err)
		return err, nil
	}
	*cj = *updatedCJ

	if cj.DeletionTimestamp != nil {
		// The CronJob is being deleted.
		// Don't do anything other than updating status.
		return fmt.Errorf("cronjob %s/%s is being deleted", cj.Namespace, cj.Name), nil
	}

	if cj.Spec.Suspend != nil && *cj.Spec.Suspend {
		klog.V(4).Infof("Not starting job for %s because it is suspended", nameForLog)
		return nil, nil
	}

	times, err := getRecentUnmetScheduleTimes(*cj, now)
	switch {
	case err != nil && len(times) == 0:
		// too many missed jobs, schedule the next one on time and return
		recorder.Eventf(cj, v1.EventTypeWarning, "TooManyMissedTimes", "Too many missed times for the cronjob, will schedule the next one", err)
		klog.Errorf("Too many missed times for the cronjob, will schedule the next one", nameForLog, err)
		return nil, nil
	case err != nil:
		// return error
		recorder.Eventf(cj, v1.EventTypeWarning, "FailedNeedsStart", "Cannot determine if job needs to be started: %v", err)
		klog.Errorf("Cannot determine if %s needs to be started: %v", nameForLog, err)
		return err, nil
	case len(times) == 0:
		// no unmet start time, return
		klog.V(4).Infof("No unmet start times for %s", nameForLog)
		return nil, nil
	default:
		// multiple unmet start times, start the last one.
		klog.V(4).Infof("Multiple unmet start times for %s so only starting last one", nameForLog)
	}

	scheduledTime := times[len(times)-1]
	tooLate := false
	if cj.Spec.StartingDeadlineSeconds != nil {
		tooLate = scheduledTime.Add(time.Second * time.Duration(*cj.Spec.StartingDeadlineSeconds)).Before(now)
	}
	if tooLate {
		klog.V(4).Infof("Missed starting window for %s", nameForLog)
		recorder.Eventf(cj, v1.EventTypeWarning, "MissSchedule", "Missed scheduled time to start a job: %s", scheduledTime.Format(time.RFC1123Z))
		// TODO: Since we don't set LastScheduleTime when not scheduling, we are going to keep noticing
		// the miss every cycle.  In order to avoid sending multiple events, and to avoid processing
		// the cj again and again, we could set a Status.LastMissedTime when we notice a miss.
		// Then, when we call getRecentUnmetScheduleTimes, we can take max(creationTimestamp,
		// Status.LastScheduleTime, Status.LastMissedTime), and then so we won't generate
		// and event the next time we process it, and also so the user looking at the status
		// can see easily that there was a missed execution.
		t := time.Duration(int64(scheduledTime.Sub(now).Seconds()))
		return nil, &t
	}
	if cj.Spec.ConcurrencyPolicy == batchv1beta1.ForbidConcurrent && len(cj.Status.Active) > 0 {
		// Regardless which source of information we use for the set of active jobs,
		// there is some risk that we won't see an active job when there is one.
		// (because we haven't seen the status update to the SJ or the created pod).
		// So it is theoretically possible to have concurrency with Forbid.
		// As long the as the invocations are "far enough apart in time", this usually won't happen.
		//
		// TODO: for Forbid, we could use the same name for every execution, as a lock.
		// With replace, we could use a name that is deterministic per execution time.
		// But that would mean that you could not inspect prior successes or failures of Forbid jobs.
		klog.V(4).Infof("Not starting job for %s because of prior execution still running and concurrency policy is Forbid", nameForLog)
		// TODO: @alpatel requeue this cronjob for next scheduled time
		t := time.Duration(int64(scheduledTime.Sub(now).Seconds()))
		return nil, &t
	}
	if cj.Spec.ConcurrencyPolicy == batchv1beta1.ReplaceConcurrent {
		for _, j := range cj.Status.Active {
			klog.V(4).Infof("Deleting job %s of %s that was still running at next scheduled start time", j.Name, nameForLog)

			job, err := jc.GetJob(j.Namespace, j.Name)
			if err != nil {
				recorder.Eventf(cj, v1.EventTypeWarning, "FailedGet", "Get job: %v", err)
				return err, nil
			}
			if !deleteJob(cj, job, jc, recorder) {
				return fmt.Errorf("could not replace job %s/%s", job.Namespace, job.Name), nil
			}
		}
	}

	jobReq, err := getJobFromTemplate(cj, scheduledTime)
	if err != nil {
		klog.Errorf("Unable to make Job from template in %s: %v", nameForLog, err)
		return err, nil
	}
	jobResp, err := jc.CreateJob(cj.Namespace, jobReq)
	if err != nil {
		// If the namespace is being torn down, we can safely ignore
		// this error since all subsequent creations will fail.
		if !errors.HasStatusCause(err, v1.NamespaceTerminatingCause) {
			recorder.Eventf(cj, v1.EventTypeWarning, "FailedCreate", "Error creating job: %v", err)
		}
		return err, nil
	}
	klog.V(4).Infof("Created Job %s for %s", jobResp.Name, nameForLog)
	recorder.Eventf(cj, v1.EventTypeNormal, "SuccessfulCreate", "Created job %v", jobResp.Name)

	// ------------------------------------------------------------------ //

	// If this process restarts at this point (after posting a job, but
	// before updating the status), then we might try to start the job on
	// the next time.  Actually, if we re-list the SJs and Jobs on the next
	// iteration of syncAll, we might not see our own status update, and
	// then post one again.  So, we need to use the job name as a lock to
	// prevent us from making the job twice (name the job with hash of its
	// scheduled time).

	// Add the just-started job to the status list.
	jobRef, err := getRef(jobResp)
	if err != nil {
		klog.V(2).Infof("Unable to make object reference for job for %s", nameForLog)
		return fmt.Errorf("unable to make object reference for job for %s", nameForLog), nil
	} else {
		cj.Status.Active = append(cj.Status.Active, *jobRef)
	}
	cj.Status.LastScheduleTime = &metav1.Time{Time: scheduledTime}
	if _, err := cjc.UpdateStatus(cj); err != nil {
		klog.Infof("Unable to update status for %s (rv = %s): %v", nameForLog, cj.ResourceVersion, err)
		return fmt.Errorf("unable to update status for %s (rv = %s): %v", nameForLog, cj.ResourceVersion, err), nil
	}
	// TODO: @alpatel success requeue this cronjob for next scheduled time

	t := time.Duration(int64(scheduledTime.Sub(now).Seconds()))
	return nil, &t
}

// deleteJob reaps a job, deleting the job, the pods and the reference in the active list
func deleteJob(cj *batchv1beta1.CronJob, job *batchv1.Job, jc jobControlInterface, recorder record.EventRecorder) bool {
	nameForLog := fmt.Sprintf("%s/%s", cj.Namespace, cj.Name)

	// delete the job itself...
	if err := jc.DeleteJob(job.Namespace, job.Name); err != nil {
		recorder.Eventf(cj, v1.EventTypeWarning, "FailedDelete", "Deleted job: %v", err)
		klog.Errorf("Error deleting job %s from %s: %v", job.Name, nameForLog, err)
		return false
	}
	// ... and its reference from active list
	deleteFromActiveList(cj, job.ObjectMeta.UID)
	recorder.Eventf(cj, v1.EventTypeNormal, "SuccessfulDelete", "Deleted job %v", job.Name)

	return true
}

func getRef(object runtime.Object) (*v1.ObjectReference, error) {
	return ref.GetReference(scheme.Scheme, object)
}
