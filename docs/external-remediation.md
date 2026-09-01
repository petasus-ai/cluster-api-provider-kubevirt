# External remediation: restart instead of replace

By default, when a Cluster API `MachineHealthCheck` marks a Machine unhealthy,
the Machine is deleted and a replacement is created — a full node rebuild
(image pull, kubeadm join, addon rollout). For faults that a reboot fixes,
CAPK offers a cheaper first step through Cluster API's
[external remediation](https://cluster-api.sigs.k8s.io/tasks/automated-machine-management/healthchecking)
mechanism: restart the VirtualMachineInstance backing the Machine, and only
fall back to replacement if that does not help.

## How it works

1. A `MachineHealthCheck` with `spec.remediation.templateRef` pointing at a
   `KubevirtRemediationTemplate` creates a `KubevirtRemediation` object
   (named after the Machine, owned by it) instead of deleting the Machine.
2. The KubevirtRemediation controller deletes the Machine's
   VirtualMachineInstance. Because CAPK VMs run with `runStrategy: Always`,
   the KubeVirt VM controller immediately recreates the VMI — same
   VirtualMachine, same disks, same node name. This is the API-level
   equivalent of a power cycle.
3. If the node passes its health check again, the MachineHealthCheck
   controller deletes the `KubevirtRemediation` object and nothing else
   happens.
4. If the Machine is still unhealthy after `timeoutSeconds`, the controller
   restarts the VMI again, up to `retryLimit` times.
5. When the retry budget is exhausted — or the VM cannot be restarted at all
   (for example its `runStrategy` is not `Always`) — the controller sets the
   `OwnerRemediated` condition to `False` on the Machine. The MachineSet
   controller then deletes and replaces the Machine, which is exactly what
   would have happened without external remediation.

## Usage

```yaml
apiVersion: infrastructure.cluster.x-k8s.io/v1alpha1
kind: KubevirtRemediationTemplate
metadata:
  name: worker-restart
  namespace: default
spec:
  template:
    spec:
      strategy:
        type: Reboot
        retryLimit: 2
        timeoutSeconds: 300
---
apiVersion: cluster.x-k8s.io/v1beta2
kind: MachineHealthCheck
metadata:
  name: worker-healthcheck
  namespace: default
spec:
  clusterName: my-cluster
  selector:
    matchLabels:
      cluster.x-k8s.io/deployment-name: my-workers
  checks:
    unhealthyNodeConditions:
      - type: Ready
        status: "False"
        timeoutSeconds: 300
      - type: Ready
        status: Unknown
        timeoutSeconds: 300
  remediation:
    templateRef:
      apiVersion: infrastructure.cluster.x-k8s.io/v1alpha1
      kind: KubevirtRemediationTemplate
      name: worker-restart
```

## Requirements and caveats

- The VirtualMachine must use `runStrategy: Always` (the CAPK default). Under
  any other run strategy a deleted VMI is not recreated, so the controller
  refuses to restart and immediately hands the Machine back to Cluster API.
- `timeoutSeconds` only has to outlast a normal boot of the node: the Machine
  is considered healthy again as soon as the MachineHealthCheck deletes the
  remediation object.
- Control plane Machines are remediated by their owner (for example
  KubeadmControlPlane), which applies its own safety rules on top.
