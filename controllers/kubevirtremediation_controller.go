/*
Copyright 2026 The Kubernetes Authors.

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
	gocontext "context"
	"fmt"
	"time"

	"github.com/go-logr/logr"
	"github.com/pkg/errors"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	kubevirtv1 "kubevirt.io/api/core/v1"
	clusterv1 "sigs.k8s.io/cluster-api/api/core/v1beta2"
	"sigs.k8s.io/cluster-api/util"
	"sigs.k8s.io/cluster-api/util/conditions"
	"sigs.k8s.io/cluster-api/util/patch"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"

	infrav1 "sigs.k8s.io/cluster-api-provider-kubevirt/api/v1alpha1"
	"sigs.k8s.io/cluster-api-provider-kubevirt/pkg/infracluster"
)

// KubevirtRemediationReconciler reconciles KubevirtRemediation objects created
// by the Cluster API MachineHealthCheck controller.
//
// Instead of letting CAPI delete and replace an unhealthy Machine, it restarts
// the VirtualMachineInstance backing it: with runStrategy Always the KubeVirt
// VM controller immediately recreates the VMI, which boots with the same name
// and disks. If the Machine is still unhealthy after the configured retries,
// the reconciler sets the OwnerRemediated condition to False, handing the
// Machine back to CAPI for the usual delete-and-replace.
//
// Success is not observed directly: when the node passes its health check
// again, the MachineHealthCheck controller deletes the KubevirtRemediation
// object and reconciliation simply stops.
type KubevirtRemediationReconciler struct {
	client.Client
	InfraCluster infracluster.InfraCluster
}

// +kubebuilder:rbac:groups=infrastructure.cluster.x-k8s.io,resources=kubevirtremediations,verbs=get;list;watch;update;patch
// +kubebuilder:rbac:groups=infrastructure.cluster.x-k8s.io,resources=kubevirtremediations/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=infrastructure.cluster.x-k8s.io,resources=kubevirtremediationtemplates,verbs=get;list;watch
// +kubebuilder:rbac:groups=cluster.x-k8s.io,resources=machines,verbs=get;list;watch;update;patch
// +kubebuilder:rbac:groups=kubevirt.io,resources=virtualmachines,verbs=get;list;watch
// +kubebuilder:rbac:groups=kubevirt.io,resources=virtualmachineinstances,verbs=get;list;watch;delete

// Reconcile drives one KubevirtRemediation through its phases.
func (r *KubevirtRemediationReconciler) Reconcile(ctx gocontext.Context, req ctrl.Request) (_ ctrl.Result, rerr error) {
	logger := log.FromContext(ctx)

	remediation := &infrav1.KubevirtRemediation{}
	if err := r.Get(ctx, req.NamespacedName, remediation); err != nil {
		if apierrors.IsNotFound(err) {
			// Deleted by the MachineHealthCheck controller once the Machine is
			// healthy again, or garbage-collected with its owner Machine.
			return ctrl.Result{}, nil
		}
		return ctrl.Result{}, err
	}

	if remediation.Status.Phase == infrav1.PhaseFailed {
		// Terminal: the Machine was handed back to CAPI. The MachineSet will
		// delete the Machine and this object goes with it via its owner ref.
		return ctrl.Result{}, nil
	}

	machine, err := util.GetOwnerMachine(ctx, r.Client, remediation.ObjectMeta)
	if err != nil {
		return ctrl.Result{}, errors.Wrap(err, "failed to get owner Machine")
	}
	if machine == nil {
		// The MachineHealthCheck controller always sets the owner reference at
		// creation time, so this only happens for hand-made objects.
		logger.Info("KubevirtRemediation has no owner Machine, ignoring")
		return ctrl.Result{}, nil
	}
	logger = logger.WithValues("machine", machine.Name)

	if !machine.DeletionTimestamp.IsZero() {
		// Something else is already replacing the Machine; restarting its VM
		// now would only race the deletion.
		return ctrl.Result{}, nil
	}

	patchHelper, err := patch.NewHelper(remediation, r.Client)
	if err != nil {
		return ctrl.Result{}, err
	}
	defer func() {
		if err := patchHelper.Patch(ctx, remediation); err != nil && rerr == nil {
			rerr = err
		}
	}()

	strategy := effectiveStrategy(remediation)
	if strategy.Type != infrav1.RebootRemediationType {
		// The enum on the CRD only admits Reboot; anything else means the API
		// grew a strategy this controller does not implement yet.
		return ctrl.Result{}, r.handOverToCAPI(ctx, logger, remediation, machine,
			fmt.Sprintf("unsupported remediation strategy %q", strategy.Type))
	}

	if remediation.Status.Phase == "" {
		remediation.Status.Phase = infrav1.PhaseRunning
	}

	timeout := time.Duration(strategy.TimeoutSeconds) * time.Second

	if remediation.Status.Phase == infrav1.PhaseWaiting {
		remaining := timeout
		if remediation.Status.LastRemediated != nil {
			remaining = time.Until(remediation.Status.LastRemediated.Add(timeout))
		}
		if remaining > 0 {
			// Still inside the boot window. If the node comes back healthy the
			// MachineHealthCheck deletes this object before the requeue fires.
			return ctrl.Result{RequeueAfter: remaining}, nil
		}

		// Timed out. This object still existing means the Machine is still
		// unhealthy: retry if budget remains, otherwise give the Machine back.
		if remediation.Status.RetryCount >= strategy.RetryLimit {
			return ctrl.Result{}, r.handOverToCAPI(ctx, logger, remediation, machine,
				fmt.Sprintf("machine still unhealthy after %d restart(s)", remediation.Status.RetryCount))
		}
		remediation.Status.Phase = infrav1.PhaseRunning
	}

	// Phase Running: issue a restart.
	if err := r.restartVM(ctx, machine); err != nil {
		if handErr := r.handOverToCAPI(ctx, logger, remediation, machine, err.Error()); handErr != nil {
			return ctrl.Result{}, handErr
		}
		return ctrl.Result{}, nil
	}

	now := metav1.Now()
	remediation.Status.LastRemediated = &now
	remediation.Status.RetryCount++
	remediation.Status.Phase = infrav1.PhaseWaiting
	logger.Info("Restarted VirtualMachineInstance", "retryCount", remediation.Status.RetryCount,
		"retryLimit", strategy.RetryLimit)

	return ctrl.Result{RequeueAfter: timeout}, nil
}

// restartVM deletes the VirtualMachineInstance backing the Machine. With
// runStrategy Always the KubeVirt VM controller recreates it immediately —
// same VM object, same disks, same node name — which is the closest thing to a
// power cycle the KubeVirt API offers to a plain Kubernetes client. (The
// virtctl restart subresource is not reachable through controller-runtime.)
//
// Errors returned here are terminal for the remediation: the caller hands the
// Machine back to CAPI.
func (r *KubevirtRemediationReconciler) restartVM(ctx gocontext.Context, machine *clusterv1.Machine) error {
	kubevirtMachine := &infrav1.KubevirtMachine{}
	kvmKey := types.NamespacedName{Namespace: machine.Namespace, Name: machine.Spec.InfrastructureRef.Name}
	if err := r.Get(ctx, kvmKey, kubevirtMachine); err != nil {
		return errors.Wrap(err, "failed to get KubevirtMachine")
	}

	infraClient, infraNamespace, err := r.InfraCluster.GenerateInfraClusterClient(
		kubevirtMachine.Spec.InfraClusterSecretRef, kubevirtMachine.Namespace, ctx)
	if err != nil {
		return errors.Wrap(err, "failed to generate infra cluster client")
	}

	// Mirror the namespace defaulting the KubevirtMachine controller applies
	// when it creates the VM.
	vmNamespace := kubevirtMachine.Spec.VirtualMachineTemplate.ObjectMeta.Namespace
	if vmNamespace == "" {
		vmNamespace = infraNamespace
	}
	vmKey := types.NamespacedName{Namespace: vmNamespace, Name: kubevirtMachine.Name}

	vm := &kubevirtv1.VirtualMachine{}
	if err := infraClient.Get(ctx, vmKey, vm); err != nil {
		return errors.Wrap(err, "failed to get VirtualMachine")
	}

	runStrategy, err := vm.RunStrategy()
	if err != nil {
		return errors.Wrap(err, "failed to read VM run strategy")
	}
	if runStrategy != kubevirtv1.RunStrategyAlways {
		// Only Always guarantees the VM controller brings the VMI back after
		// we delete it. Under Manual or Halted a deleted VMI stays gone, which
		// would silently turn a restart into an outage.
		return fmt.Errorf("VM run strategy is %q, restart requires %q", runStrategy, kubevirtv1.RunStrategyAlways)
	}

	vmi := &kubevirtv1.VirtualMachineInstance{}
	vmi.Namespace = vmKey.Namespace
	vmi.Name = vmKey.Name
	if err := infraClient.Delete(ctx, vmi); err != nil && !apierrors.IsNotFound(err) {
		return errors.Wrap(err, "failed to delete VirtualMachineInstance")
	}

	return nil
}

// handOverToCAPI marks the remediation as failed and sets the OwnerRemediated
// condition to False on the Machine. The MachineSet controller watches for
// exactly that combination (unhealthy + OwnerRemediated=False) and responds by
// deleting the Machine and creating a replacement — the same outcome the
// MachineHealthCheck would have produced without external remediation.
func (r *KubevirtRemediationReconciler) handOverToCAPI(ctx gocontext.Context, logger logr.Logger,
	remediation *infrav1.KubevirtRemediation, machine *clusterv1.Machine, reason string) error {
	logger.Info("Handing Machine back to Cluster API for replacement", "reason", reason)

	machineHelper, err := patch.NewHelper(machine, r.Client)
	if err != nil {
		return err
	}

	conditions.Set(machine, metav1.Condition{
		Type:    clusterv1.MachineOwnerRemediatedCondition,
		Status:  metav1.ConditionFalse,
		Reason:  clusterv1.MachineOwnerRemediatedWaitingForRemediationReason,
		Message: fmt.Sprintf("KubeVirt remediation gave up: %s", reason),
	})
	if err := machineHelper.Patch(ctx, machine); err != nil {
		return errors.Wrap(err, "failed to patch Machine with OwnerRemediated condition")
	}

	remediation.Status.Phase = infrav1.PhaseFailed

	return nil
}

// effectiveStrategy returns the remediation strategy with API defaults applied,
// so the controller behaves the same for objects that bypassed the CRD
// defaulting (unit tests, direct Go clients).
func effectiveStrategy(remediation *infrav1.KubevirtRemediation) infrav1.RemediationStrategy {
	strategy := infrav1.RemediationStrategy{
		Type:           infrav1.RebootRemediationType,
		RetryLimit:     1,
		TimeoutSeconds: 600,
	}
	if in := remediation.Spec.Strategy; in != nil {
		if in.Type != "" {
			strategy.Type = in.Type
		}
		if in.RetryLimit > 0 {
			strategy.RetryLimit = in.RetryLimit
		}
		if in.TimeoutSeconds > 0 {
			strategy.TimeoutSeconds = in.TimeoutSeconds
		}
	}

	return strategy
}

// SetupWithManager sets up the controller with the Manager.
func (r *KubevirtRemediationReconciler) SetupWithManager(_ gocontext.Context, mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&infrav1.KubevirtRemediation{}).
		Complete(r)
}
