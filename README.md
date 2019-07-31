# estafette-k8s-node-compactor

This controller can scale down a node pool if it has underutilized nodes more aggressively than how the cloud implementation (such as GKE, AWS, etc.) would do.

[![License](https://img.shields.io/github/license/estafette/estafette-k8s-node-compactor.svg)](https://github.com/estafette/estafette-k8s-node-compactor/blob/master/LICENSE)

**Note**: Currently this controller only supports the Google Kubernetes Engine, but it could be adapted to be used with other implementations as well.  
When we mention the concept of a "node pool" we specifically mean the node pool construct of GKE.

## Why?

Certain cloud implementations of Kubernetes (such as GKE, the Google Kubernetes Engine) have a fixed built-in limit for the CPU-utilization under which the Cluster Autoscaler considers removing a node from a node pool. For example in GKE this limit is 50%, [and there is no way to customize it](https://stackoverflow.com/a/50911019).

This means that it can happen that during peak hours we have nodes which are all fully packed with pods.

![Nodes tightly packed with pods during peak hours.](/readme-peak-hours.png)

But then during off hours the deployments scale down, so some of the pods are removed, thus the nodes might become uniformly underutilized.

![Nodes underutilized in off hours.](/readme-off-hours.png)

So it can happen that all of our nodes have only 60% (or even 51%) CPU utilization, but GKE is not going to scale the cluster down, and there is no way to change the 50% limit.

This is particularly a problem if we are running enough nodes so that they incur a substantial hosting costs, in that case we'd like to pack the pods onto our nodes always as tightly as possible in order to run the fewest required nodes.

That's what this controller makes possible by accepting a custom CPU utilization limit we can set to a higher value than 50%, under which it'll start removing the underutilized nodes.

## Requirements

This controller can only be utilized if we use the Cluster Autoscaler in our cluster. The reason for this is that during scaling down, the controller doesn't explicitly delete nodes, it just cordons and drains them, so that the Cluster Autoscaler deletes them due to underutilization.

## Usage

As a Kubernetes administrator, you first need to deploy the `rbac.yaml` file which set role and permissions.

```
kubectl apply -f rbac.yaml
```

Then deploy the application to Kubernetes cluster using the `kubernetes.yaml` manifest:

```
cat kubernetes.yaml | \
    APP_NAME=estafette-k8s-node-compactor \
    NAMESPACE=estafette \
    TEAM_NAME=myteam \
    ESTAFETTE_K8S_NODE_COMPACTOR_CONFIG="{}" \
    GO_PIPELINE_LABEL=1.0.5 \
    VERSION=1.0.5 \
    CPU_REQUEST=10m \
    MEMORY_REQUEST=15Mi \
    CPU_LIMIT=50m \
    MEMORY_LIMIT=128Mi \
    envsubst | kubectl apply -f -
```

Once the controller is up and running you have to edit the ConfigMap `estafette-k8s-node-compactor-config`, and set the `estafette-k8s-node-compactor-config.yaml` data item to the Json configuration specifying which node pools the compactor is enabled.  
The format of the configuration is the following.

```
{
    "nodePools": {
        "nodepool1": {
            "enabled": true,
            "scaleDownCPURequestRatioLimit": 0.75,
            "scaleDownRequiredUnderutilizedNodeCount": 5
        },
        "nodepool2": {
            "enabled": true,
            "scaleDownCPURequestRatioLimit": 0.6,
            "scaleDownRequiredUnderutilizedNodeCount": 3
        }
    }
}
```

Where the field names in the `nodePools` object have to be equal to the name (which is in the `cloud.google.com/gke-nodepool` label). The fields which can be configured for each node pool are the following.

 - `enabled`: With this label the node compaction can be enabled for a node pool. The node compaction will only happen for the node pool in which the nodes have this label with the value `"true"`.
 - `scaleDownCPURequestRatioLimit`: Specifies the limit of CPU utilization under which a node is considered for removal. (To get value out of using this controller, it should be set to a higher value then what the built-in limit of the Cluster Autoscaler is.)
 - `scaleDownRequiredUnderutilizedNodeCount`: The number of underutilized nodes needed to start a scaledown. This setting is needed, because if there is only one single underutilized node, and all the others are tightly packed, then there is no point in removing that node, because its pods wouldn't fit anywhere, so a new node would be started in its place anyway.  
 This has to be depending on the CPU limit we configured to ensure that we only do a scaledown when there are enough underutilized nodes to take over the load. For example If that's set to 75% (`0.75`), then we can set this label to `5`, but if the limit is 90% (`0.9`), then this label has to be set to `10`.

*The default value of the `enabled` field is `false`, so we only have to include the node pools in the configuration for which we want to enable the compactor.*

## Algorithm

The algorithm used by the controller is the following:

 - We iterate over all the node pools. (The actual compaction only happens for the pools for which it's enabled in the configuration, but the metrics are published for every pool.)
 - All the nodes for which the CPU utilization is under the limit specified in `scaleDownCPURequestRatioLimit` are marked for removal, and the time of marking it is saved. (If a node has already been marked, but its utilization has increased over the limit, then the mark is removed.)
 - If we have at least as many underutilized nodes as `scaleDownRequiredUnderutilizedNodeCount`, then we try to pick a node for scaledown.
 - When scaling down, we check if there is a node which has been marked for removal for at least 5 minutes (customizable with the `NEEDED_MARKED_TIME_FOR_REMOVAL_SECONDS` env var), we pick it for removal. If there are multiple suitable candidates, we pick the one with the lowest current CPU utilization.
 - Removing a node consists of two steps:
   - Cordoning it
   - Deleting all of its pods

And then the Cluster autoscaler will notice that the node is not utilized, and it'll remove it automatically, so we don't actually delete nodes nor VMs ourselves.

**Note**: This algorithm doesn't take the pod disruption budgets into account at all, so it's only recommended to use the controller when we don't depend on that.