/*


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

package controllers

import (
	"context"
	"fmt"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"

	kerrors "k8s.io/apimachinery/pkg/util/errors"

	"github.com/go-logr/logr"
	"github.com/pkg/errors"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/tools/record"
	clusterv1 "sigs.k8s.io/cluster-api/api/v1alpha3"
	capierrors "sigs.k8s.io/cluster-api/errors"
	"sigs.k8s.io/cluster-api/util"
	"sigs.k8s.io/cluster-api/util/annotations"
	"sigs.k8s.io/cluster-api/util/conditions"
	"sigs.k8s.io/cluster-api/util/patch"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/source"

	controlplanev1 "github.com/zawachte-msft/cluster-api-controlplane-provider-k3s/api/v1alpha3"
	k3s "github.com/zawachte-msft/cluster-api-controlplane-provider-k3s/pkg/k3s"
	"github.com/zawachte-msft/cluster-api-controlplane-provider-k3s/pkg/machinefilters"
)

// KThreesControlPlaneReconciler reconciles a KThreesControlPlane object
type KThreesControlPlaneReconciler struct {
	client.Client
	Log        logr.Logger
	Scheme     *runtime.Scheme
	controller controller.Controller
	recorder   record.EventRecorder

	managementCluster         k3s.ManagementCluster
	managementClusterUncached k3s.ManagementCluster
}

// +kubebuilder:rbac:groups=controlplane.controlplane.cluster.x-k8s.io,resources=kthreescontrolplanes,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=controlplane.controlplane.cluster.x-k8s.io,resources=kthreescontrolplanes/status,verbs=get;update;patch

func (r *KThreesControlPlaneReconciler) Reconcile(req ctrl.Request) (res ctrl.Result, reterr error) {
	logger := r.Log.WithValues("namespace", req.Namespace, "kthreesControlPlane", req.Name)
	ctx := context.Background()

	// Fetch the KThreesControlPlane instance.
	kcp := &controlplanev1.KThreesControlPlane{}
	if err := r.Client.Get(ctx, req.NamespacedName, kcp); err != nil {
		if apierrors.IsNotFound(err) {
			return ctrl.Result{}, nil
		}
		return ctrl.Result{Requeue: true}, nil
	}

	// Fetch the Cluster.
	cluster, err := util.GetOwnerCluster(ctx, r.Client, kcp.ObjectMeta)
	if err != nil {
		logger.Error(err, "Failed to retrieve owner Cluster from the API Server")
		return ctrl.Result{}, err
	}
	if cluster == nil {
		logger.Info("Cluster Controller has not yet set OwnerRef")
		return ctrl.Result{}, nil
	}
	logger = logger.WithValues("cluster", cluster.Name)

	if annotations.IsPaused(cluster, kcp) {
		logger.Info("Reconciliation is paused for this object")
		return ctrl.Result{}, nil
	}

	// Wait for the cluster infrastructure to be ready before creating machines
	if !cluster.Status.InfrastructureReady {
		return ctrl.Result{}, nil
	}

	// Initialize the patch helper.
	patchHelper, err := patch.NewHelper(kcp, r.Client)
	if err != nil {
		logger.Error(err, "Failed to configure the patch helper")
		return ctrl.Result{Requeue: true}, nil
	}

	// Add finalizer first if not exist to avoid the race condition between init and delete
	if !controllerutil.ContainsFinalizer(kcp, controlplanev1.KThreesControlPlaneFinalizer) {
		controllerutil.AddFinalizer(kcp, controlplanev1.KThreesControlPlaneFinalizer)

		// patch and return right away instead of reusing the main defer,
		// because the main defer may take too much time to get cluster status
		// Patch ObservedGeneration only if the reconciliation completed successfully
		patchOpts := []patch.Option{}
		if reterr == nil {
			patchOpts = append(patchOpts, patch.WithStatusObservedGeneration{})
		}
		if err := patchHelper.Patch(ctx, kcp, patchOpts...); err != nil {
			logger.Error(err, "Failed to patch KThreesControlPlane to add finalizer")
			return ctrl.Result{}, err
		}

		return ctrl.Result{}, nil
	}

	defer func() {
		if requeueErr, ok := errors.Cause(reterr).(capierrors.HasRequeueAfterError); ok {
			if res.RequeueAfter == 0 {
				res.RequeueAfter = requeueErr.GetRequeueAfter()
				reterr = nil
			}
		}

		// Always attempt to update status.
		if err := r.updateStatus(ctx, kcp, cluster); err != nil {
			var connFailure *internal.RemoteClusterConnectionError
			if errors.As(err, &connFailure) {
				logger.Info("Could not connect to workload cluster to fetch status", "err", err.Error())
			} else {
				logger.Error(err, "Failed to update KThreesControlPlane Status")
				reterr = kerrors.NewAggregate([]error{reterr, err})
			}
		}

		// Always attempt to Patch the KThreesControlPlane object and status after each reconciliation.
		if err := patchKThreesControlPlane(ctx, patchHelper, kcp); err != nil {
			logger.Error(err, "Failed to patch KThreesControlPlane")
			reterr = kerrors.NewAggregate([]error{reterr, err})
		}

		// TODO: remove this as soon as we have a proper remote cluster cache in place.
		// Make KCP to requeue in case status is not ready, so we can check for node status without waiting for a full resync (by default 10 minutes).
		// Only requeue if we are not going in exponential backoff due to error, or if we are not already re-queueing, or if the object has a deletion timestamp.
		if reterr == nil && !res.Requeue && !(res.RequeueAfter > 0) && kcp.ObjectMeta.DeletionTimestamp.IsZero() {
			if !kcp.Status.Ready {
				res = ctrl.Result{RequeueAfter: 20 * time.Second}
			}
		}
	}()

	if !kcp.ObjectMeta.DeletionTimestamp.IsZero() {
		// Handle deletion reconciliation loop.
		return r.reconcileDelete(ctx, cluster, kcp)
	}

	// Handle normal reconciliation loop.
	return r.reconcile(ctx, cluster, kcp)
}

func patchKThreesControlPlane(ctx context.Context, patchHelper *patch.Helper, kcp *controlplanev1.KThreesControlPlane) error {
	// Always update the readyCondition by summarizing the state of other conditions.
	conditions.SetSummary(kcp,
		conditions.WithConditions(
			controlplanev1.MachinesSpecUpToDateCondition,
			controlplanev1.ResizedCondition,
			controlplanev1.MachinesReadyCondition,
			controlplanev1.AvailableCondition,
			controlplanev1.CertificatesAvailableCondition,
		),
	)

	// Patch the object, ignoring conflicts on the conditions owned by this controller.
	return patchHelper.Patch(
		ctx,
		kcp,
		patch.WithOwnedConditions{Conditions: []clusterv1.ConditionType{
			clusterv1.ReadyCondition,
			controlplanev1.MachinesSpecUpToDateCondition,
			controlplanev1.ResizedCondition,
			controlplanev1.MachinesReadyCondition,
			controlplanev1.AvailableCondition,
			controlplanev1.CertificatesAvailableCondition,
		}},
	)
}

func (r *KThreesControlPlaneReconciler) SetupWithManager(mgr ctrl.Manager) error {

	c, err := ctrl.NewControllerManagedBy(mgr).
		For(&controlplanev1.KThreesControlPlane{}).
		Owns(&clusterv1.Machine{}).
		//	WithOptions(options).
		//	WithEventFilter(predicates.ResourceNotPaused(r.Log)).
		Build(r)
	if err != nil {
		return errors.Wrap(err, "failed setting up with a controller manager")
	}

	err = c.Watch(
		&source.Kind{Type: &clusterv1.Cluster{}},
		&handler.EnqueueRequestsFromMapFunc{
			ToRequests: handler.ToRequestsFunc(r.ClusterToKThreesControlPlane),
		},
	//	predicates.ClusterUnpausedAndInfrastructureReady(r.Log),
	)
	if err != nil {
		return errors.Wrap(err, "failed adding Watch for Clusters to controller manager")
	}

	r.Scheme = mgr.GetScheme()
	r.controller = c
	r.recorder = mgr.GetEventRecorderFor("k3s-control-plane-controller")

	if r.managementCluster == nil {
		r.managementCluster = &k3s.Management{Client: r.Client}
	}
	if r.managementClusterUncached == nil {
		r.managementClusterUncached = &k3s.Management{Client: mgr.GetAPIReader()}
	}

	return nil
}

// ClusterToKThreesControlPlane is a handler.ToRequestsFunc to be used to enqueue requests for reconciliation
// for KThreesControlPlane based on updates to a Cluster.
func (r *KThreesControlPlaneReconciler) ClusterToKThreesControlPlane(o handler.MapObject) []ctrl.Request {
	c, ok := o.Object.(*clusterv1.Cluster)
	if !ok {
		r.Log.Error(nil, fmt.Sprintf("Expected a Cluster but got a %T", o.Object))
		return nil
	}

	controlPlaneRef := c.Spec.ControlPlaneRef
	if controlPlaneRef != nil && controlPlaneRef.Kind == "KThreesControlPlane" {
		return []ctrl.Request{{NamespacedName: client.ObjectKey{Namespace: controlPlaneRef.Namespace, Name: controlPlaneRef.Name}}}
	}

	return nil
}

// updateStatus is called after every reconcilitation loop in a defer statement to always make sure we have the
// resource status subresourcs up-to-date.
func (r *KThreesControlPlaneReconciler) updateStatus(ctx context.Context, kcp *controlplanev1.KThreesControlPlane, cluster *clusterv1.Cluster) error {
	selector := machinefilters.ControlPlaneSelectorForCluster(cluster.Name)
	// Copy label selector to its status counterpart in string format.
	// This is necessary for CRDs including scale subresources.
	kcp.Status.Selector = selector.String()

	ownedMachines, err := r.managementCluster.GetMachinesForCluster(ctx, util.ObjectKey(cluster), machinefilters.OwnedMachines(kcp))
	if err != nil {
		return errors.Wrap(err, "failed to get list of owned machines")
	}

	logger := r.Log.WithValues("namespace", kcp.Namespace, "kubeadmControlPlane", kcp.Name, "cluster", cluster.Name)
	controlPlane, err := k3s.NewControlPlane(ctx, r.Client, cluster, kcp, ownedMachines)
	if err != nil {
		logger.Error(err, "failed to initialize control plane")
		return err
	}
	kcp.Status.UpdatedReplicas = int32(len(controlPlane.UpToDateMachines()))

	replicas := int32(len(ownedMachines))
	desiredReplicas := *kcp.Spec.Replicas

	// set basic data that does not require interacting with the workload cluster
	kcp.Status.Replicas = replicas
	kcp.Status.ReadyReplicas = 0
	kcp.Status.UnavailableReplicas = replicas

	// Return early if the deletion timestamp is set, because we don't want to try to connect to the workload cluster
	// and we don't want to report resize condition (because it is set to deleting into reconcile delete).
	if !kcp.DeletionTimestamp.IsZero() {
		return nil
	}

	switch {
	// We are scaling up
	case replicas < desiredReplicas:
		conditions.MarkFalse(kcp, controlplanev1.ResizedCondition, controlplanev1.ScalingUpReason, clusterv1.ConditionSeverityWarning, "Scaling up control plane to %d replicas (actual %d)", desiredReplicas, replicas)
	// We are scaling down
	case replicas > desiredReplicas:
		conditions.MarkFalse(kcp, controlplanev1.ResizedCondition, controlplanev1.ScalingDownReason, clusterv1.ConditionSeverityWarning, "Scaling down control plane to %d replicas (actual %d)", desiredReplicas, replicas)
	default:
		// make sure last resize operation is marked as completed.
		// NOTE: we are checking the number of machines ready so we report resize completed only when the machines
		// are actually provisioned (vs reporting completed immediately after the last machine object is created).
		readyMachines := ownedMachines.Filter(machinefilters.IsReady())
		if int32(len(readyMachines)) == replicas {
			conditions.MarkTrue(kcp, controlplanev1.ResizedCondition)
		}
	}

	workloadCluster, err := r.managementCluster.GetWorkloadCluster(ctx, util.ObjectKey(cluster))
	if err != nil {
		return errors.Wrap(err, "failed to create remote cluster client")
	}
	status, err := workloadCluster.ClusterStatus(ctx)
	if err != nil {
		return err
	}
	kcp.Status.ReadyReplicas = status.ReadyNodes
	kcp.Status.UnavailableReplicas = replicas - status.ReadyNodes

	// This only gets initialized once and does not change if the kubeadm config map goes away.
	if status.HasKubeadmConfig {
		kcp.Status.Initialized = true
		conditions.MarkTrue(kcp, controlplanev1.AvailableCondition)
	}

	if kcp.Status.ReadyReplicas > 0 {
		kcp.Status.Ready = true
	}

	return nil
}
