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
	"time"

	"github.com/golang/mock/gomock"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	kubevirtv1 "kubevirt.io/api/core/v1"
	clusterv1 "sigs.k8s.io/cluster-api/api/core/v1beta2"
	"sigs.k8s.io/cluster-api/util/conditions"
	ctrl "sigs.k8s.io/controller-runtime"
	ctrlclient "sigs.k8s.io/controller-runtime/pkg/client"
	ctrlfake "sigs.k8s.io/controller-runtime/pkg/client/fake"

	infrav1 "sigs.k8s.io/cluster-api-provider-kubevirt/api/v1alpha1"
	infraclustermock "sigs.k8s.io/cluster-api-provider-kubevirt/pkg/infracluster/mock"
	"sigs.k8s.io/cluster-api-provider-kubevirt/pkg/testing"
)

var _ = Describe("KubevirtRemediation controller", func() {
	const (
		namespace   = "test-ns"
		machineName = "test-machine"
	)

	var (
		mockCtrl         *gomock.Controller
		infraClusterMock *infraclustermock.MockInfraCluster
		fakeClient       ctrlclient.Client
		reconciler       *KubevirtRemediationReconciler

		machine         *clusterv1.Machine
		kubevirtMachine *infrav1.KubevirtMachine
		vm              *kubevirtv1.VirtualMachine
		vmi             *kubevirtv1.VirtualMachineInstance
		remediation     *infrav1.KubevirtRemediation
	)

	runStrategy := func(rs kubevirtv1.VirtualMachineRunStrategy) *kubevirtv1.VirtualMachineRunStrategy {
		return &rs
	}

	BeforeEach(func() {
		mockCtrl = gomock.NewController(GinkgoT())
		infraClusterMock = infraclustermock.NewMockInfraCluster(mockCtrl)

		machine = &clusterv1.Machine{
			ObjectMeta: metav1.ObjectMeta{
				Name:      machineName,
				Namespace: namespace,
				UID:       "machine-uid",
			},
			Spec: clusterv1.MachineSpec{
				ClusterName: "test-cluster",
				InfrastructureRef: clusterv1.ContractVersionedObjectReference{
					Kind:     "KubevirtMachine",
					Name:     machineName,
					APIGroup: infrav1.GroupVersion.Group,
				},
			},
		}

		kubevirtMachine = &infrav1.KubevirtMachine{
			ObjectMeta: metav1.ObjectMeta{
				Name:      machineName,
				Namespace: namespace,
			},
		}

		vm = &kubevirtv1.VirtualMachine{
			ObjectMeta: metav1.ObjectMeta{
				Name:      machineName,
				Namespace: namespace,
			},
			Spec: kubevirtv1.VirtualMachineSpec{
				RunStrategy: runStrategy(kubevirtv1.RunStrategyAlways),
			},
		}

		vmi = &kubevirtv1.VirtualMachineInstance{
			ObjectMeta: metav1.ObjectMeta{
				Name:      machineName,
				Namespace: namespace,
			},
		}

		remediation = &infrav1.KubevirtRemediation{
			ObjectMeta: metav1.ObjectMeta{
				Name:      machineName,
				Namespace: namespace,
				OwnerReferences: []metav1.OwnerReference{
					{
						APIVersion: clusterv1.GroupVersion.String(),
						Kind:       "Machine",
						Name:       machineName,
						UID:        "machine-uid",
					},
				},
			},
			Spec: infrav1.KubevirtRemediationSpec{
				Strategy: &infrav1.RemediationStrategy{
					Type:           infrav1.RebootRemediationType,
					RetryLimit:     2,
					TimeoutSeconds: 300,
				},
			},
		}
	})

	AfterEach(func() {
		mockCtrl.Finish()
	})

	setup := func(objects ...ctrlclient.Object) {
		fakeClient = ctrlfake.NewClientBuilder().
			WithScheme(testing.SetupScheme()).
			WithObjects(objects...).
			WithStatusSubresource(&infrav1.KubevirtRemediation{}, &clusterv1.Machine{}).
			Build()
		reconciler = &KubevirtRemediationReconciler{
			Client:       fakeClient,
			InfraCluster: infraClusterMock,
		}
	}

	reconcile := func() (ctrl.Result, error) {
		return reconciler.Reconcile(gocontext.Background(), ctrl.Request{
			NamespacedName: types.NamespacedName{Namespace: namespace, Name: machineName},
		})
	}

	getRemediation := func() *infrav1.KubevirtRemediation {
		out := &infrav1.KubevirtRemediation{}
		Expect(fakeClient.Get(gocontext.Background(),
			types.NamespacedName{Namespace: namespace, Name: machineName}, out)).To(Succeed())
		return out
	}

	getMachine := func() *clusterv1.Machine {
		out := &clusterv1.Machine{}
		Expect(fakeClient.Get(gocontext.Background(),
			types.NamespacedName{Namespace: namespace, Name: machineName}, out)).To(Succeed())
		return out
	}

	expectInfraClient := func() {
		infraClusterMock.EXPECT().
			GenerateInfraClusterClient(gomock.Any(), gomock.Any(), gomock.Any()).
			Return(fakeClient, namespace, nil)
	}

	It("restarts the VMI on a fresh remediation and moves to Waiting", func() {
		setup(remediation, machine, kubevirtMachine, vm, vmi)
		expectInfraClient()

		result, err := reconcile()
		Expect(err).ShouldNot(HaveOccurred())
		Expect(result.RequeueAfter).To(Equal(300 * time.Second))

		// The VMI must be gone; the KubeVirt VM controller recreates it.
		err = fakeClient.Get(gocontext.Background(),
			types.NamespacedName{Namespace: namespace, Name: machineName}, &kubevirtv1.VirtualMachineInstance{})
		Expect(apierrors.IsNotFound(err)).To(BeTrue())

		updated := getRemediation()
		Expect(updated.Status.Phase).To(Equal(infrav1.PhaseWaiting))
		Expect(updated.Status.RetryCount).To(Equal(int32(1)))
		Expect(updated.Status.LastRemediated).ToNot(BeNil())
	})

	It("tolerates a VMI that is already gone", func() {
		setup(remediation, machine, kubevirtMachine, vm) // no VMI
		expectInfraClient()

		_, err := reconcile()
		Expect(err).ShouldNot(HaveOccurred())
		Expect(getRemediation().Status.Phase).To(Equal(infrav1.PhaseWaiting))
	})

	It("waits without restarting while inside the boot window", func() {
		lastRemediated := metav1.NewTime(time.Now().Add(-1 * time.Minute))
		remediation.Status = infrav1.KubevirtRemediationStatus{
			Phase:          infrav1.PhaseWaiting,
			RetryCount:     1,
			LastRemediated: &lastRemediated,
		}
		setup(remediation, machine, kubevirtMachine, vm, vmi)
		// No infra client expected: nothing is restarted.

		result, err := reconcile()
		Expect(err).ShouldNot(HaveOccurred())
		Expect(result.RequeueAfter).To(BeNumerically(">", 0))
		Expect(result.RequeueAfter).To(BeNumerically("<=", 300*time.Second))

		// The VMI is untouched.
		Expect(fakeClient.Get(gocontext.Background(),
			types.NamespacedName{Namespace: namespace, Name: machineName}, &kubevirtv1.VirtualMachineInstance{})).To(Succeed())
		Expect(getRemediation().Status.RetryCount).To(Equal(int32(1)))
	})

	It("restarts again when the boot window expired and retries remain", func() {
		lastRemediated := metav1.NewTime(time.Now().Add(-10 * time.Minute))
		remediation.Status = infrav1.KubevirtRemediationStatus{
			Phase:          infrav1.PhaseWaiting,
			RetryCount:     1,
			LastRemediated: &lastRemediated,
		}
		setup(remediation, machine, kubevirtMachine, vm, vmi)
		expectInfraClient()

		_, err := reconcile()
		Expect(err).ShouldNot(HaveOccurred())

		updated := getRemediation()
		Expect(updated.Status.Phase).To(Equal(infrav1.PhaseWaiting))
		Expect(updated.Status.RetryCount).To(Equal(int32(2)))
	})

	It("hands the Machine back to CAPI when the retry budget is exhausted", func() {
		lastRemediated := metav1.NewTime(time.Now().Add(-10 * time.Minute))
		remediation.Status = infrav1.KubevirtRemediationStatus{
			Phase:          infrav1.PhaseWaiting,
			RetryCount:     2, // == RetryLimit
			LastRemediated: &lastRemediated,
		}
		setup(remediation, machine, kubevirtMachine, vm, vmi)

		_, err := reconcile()
		Expect(err).ShouldNot(HaveOccurred())

		Expect(getRemediation().Status.Phase).To(Equal(infrav1.PhaseFailed))

		cond := conditions.Get(getMachine(), clusterv1.MachineOwnerRemediatedCondition)
		Expect(cond).ToNot(BeNil())
		Expect(cond.Status).To(Equal(metav1.ConditionFalse))
		Expect(cond.Reason).To(Equal(clusterv1.MachineOwnerRemediatedWaitingForRemediationReason))
	})

	It("hands the Machine back to CAPI when the VM run strategy cannot restart", func() {
		vm.Spec.RunStrategy = runStrategy(kubevirtv1.RunStrategyHalted)
		setup(remediation, machine, kubevirtMachine, vm, vmi)
		expectInfraClient()

		_, err := reconcile()
		Expect(err).ShouldNot(HaveOccurred())

		Expect(getRemediation().Status.Phase).To(Equal(infrav1.PhaseFailed))

		cond := conditions.Get(getMachine(), clusterv1.MachineOwnerRemediatedCondition)
		Expect(cond).ToNot(BeNil())
		Expect(cond.Status).To(Equal(metav1.ConditionFalse))

		// The VMI must not have been deleted: with runStrategy Halted nothing
		// would bring it back.
		Expect(fakeClient.Get(gocontext.Background(),
			types.NamespacedName{Namespace: namespace, Name: machineName}, &kubevirtv1.VirtualMachineInstance{})).To(Succeed())
	})

	It("does nothing once the remediation is Failed", func() {
		remediation.Status.Phase = infrav1.PhaseFailed
		setup(remediation, machine, kubevirtMachine, vm, vmi)

		result, err := reconcile()
		Expect(err).ShouldNot(HaveOccurred())
		Expect(result.RequeueAfter).To(BeZero())
	})

	It("does nothing while the Machine is being deleted", func() {
		now := metav1.Now()
		machine.DeletionTimestamp = &now
		machine.Finalizers = []string{"test.finalizer.cluster.x-k8s.io"}
		setup(remediation, machine, kubevirtMachine, vm, vmi)

		result, err := reconcile()
		Expect(err).ShouldNot(HaveOccurred())
		Expect(result.RequeueAfter).To(BeZero())

		// The VMI is untouched.
		Expect(fakeClient.Get(gocontext.Background(),
			types.NamespacedName{Namespace: namespace, Name: machineName}, &kubevirtv1.VirtualMachineInstance{})).To(Succeed())
	})

	It("ignores a remediation without an owner Machine", func() {
		remediation.OwnerReferences = nil
		setup(remediation, machine, kubevirtMachine, vm, vmi)

		result, err := reconcile()
		Expect(err).ShouldNot(HaveOccurred())
		Expect(result.RequeueAfter).To(BeZero())
	})
})
