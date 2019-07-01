// TODO:
// - RBAC
// - Prometheus
// - Proper logging
// - kubernetes.yaml
// - Dockerfile
// - estafette.yaml
// - README
package main

import (
	"context"
	"fmt"
	"io/ioutil"
	"log"
	"strconv"
	"strings"
	"time"

	"github.com/ericchiang/k8s"
	corev1 "github.com/ericchiang/k8s/apis/core/v1"
	"github.com/ghodss/yaml"
)

const labelNodeCompactorEnabled = "estafette.io/node-compactor-enabled"
const labelNodeCompactorScaleDownCPURequestRatioLimit = "estafette.io/node-compactor-scale-down-cpu-request-ratio-limit"
const labelNodeCompactorScaleDownRequiredUnderutilizedNodeCount = "estafette.io/node-compactor-scale-down-required-underutilized-node-count"
const labelNodeCompactorScaleDownInProgress = "estafette.io/node-compactor-scale-down-in-progress"

type nodeLabels struct {
	// Shows whether the node compactor is enabled for the node.
	enabled bool
	// Sets the percentage if under which the CPU utilization falls, the node gets deleted.
	scaleDownCPURequestRatioLimit float64
	// The number of nodes which need to be underutilized in order to do the compaction.
	// (If there is only one underutilized node, we shouldn't delete it, because its pods could not be moved anywhere else.)
	scaleDownRequiredUnderutilizedNodeCount int
	// It shows if scaling down is already in progress for this node.
	scaleDownInProgress bool
}

type nodeStats struct {
	allocatableCPU      int
	allocatableMemoryMB int
	totalCPURequests    int
	totalMemoryRequests int
	utilizedCPURatio    float64
	utilizedMemoryRatio float64
}

type nodeInfo struct {
	node   *corev1.Node
	labels nodeLabels
	stats  nodeStats
	pods   []*corev1.Pod
}

func main() {
	// client, err := k8s.NewInClusterClient()

	client, err := loadClient("kubeconfig.yaml")

	if err != nil {
		log.Fatal(err)
	}

	var nodes corev1.NodeList
	if err := client.List(context.Background(), k8s.AllNamespaces, &nodes); err != nil {
		log.Fatal(err)
	}

	var allPods corev1.PodList

	if err := client.List(context.Background(), k8s.AllNamespaces, &allPods); err != nil {
		log.Fatal(err)
	}

	nodesByPool := groupNodesByPool(nodes.Items)

	for pool, nodes := range nodesByPool {
		fmt.Printf("Node pool: %s\n", pool)

		nodeInfos := collectNodeInfos(nodes, allPods.Items)

		nodeCountUnderLimit := 0
		nodeCountScaleDownInProgress := 0

		// For every node pool we check if there are enough nodes using less resources than the limit for scaledown.
		for _, nodeInfo := range nodeInfos {
			nodeLabels := nodeInfo.labels

			if isNodeUnderutilizedCandidate(nodeInfo) {
				nodeCountUnderLimit++
			}

			if nodeLabels.scaleDownInProgress {
				nodeCountScaleDownInProgress++
			}
		}

		// We check if there are enough underutilized pods so that we can initiate a scaledown.
		// NOTE: We multiply by (nodeCountScaleDownInProgress + 1), because there might be nodes for which
		// we have initiated the scaledown in previous iterations already, which haven't been removed yet,
		// and we have to take these into account.
		if nodeCountUnderLimit >= nodeInfos[0].labels.scaleDownRequiredUnderutilizedNodeCount*(nodeCountScaleDownInProgress+1) {
			pick := pickUnderutilizedNodeToRemove(nodeInfos)

			if pick == nil {
				fmt.Printf("No node was picked for removal.")
			} else {
				fmt.Printf("The node picked for removal:")
				fmt.Printf("Node %v\n", *pick.node.Metadata.Name)
				fmt.Printf("Allocatable CPU: %vm, memory: %vMi\n", pick.stats.allocatableCPU, pick.stats.allocatableMemoryMB)
				fmt.Printf("Pods on node total requests, CPU: %vm, memory: %vMi\n", pick.stats.totalCPURequests, pick.stats.totalMemoryRequests)
				fmt.Printf("CPU utilization: %v%%, memory utilization: %v%%\n", pick.stats.utilizedCPURatio*100, pick.stats.utilizedMemoryRatio*100)

				fmt.Printf("Cordoning the node...")
				cordonNode(pick.node, client)

				fmt.Printf("Drain the pods...")
				drainPods(pick, client)
			}
		}
	}
}

func cordonNode(node *corev1.Node, k8sClient *k8s.Client) error {
	*node.Spec.Unschedulable = true

	err := k8sClient.Update(context.Background(), node)

	return err
}

func drainPods(node *nodeInfo, k8sClient *k8s.Client) error {
	for _, pod := range node.pods {
		err := k8sClient.Delete(context.Background(), pod)

		if err != nil {
			return err
		}
	}

	return nil
}

// Picks a node to be removed. We pick the node with the lowest utilization.
func pickUnderutilizedNodeToRemove(nodes []nodeInfo) *nodeInfo {
	var pick *nodeInfo

	for _, n := range nodes {
		if isNodeUnderutilizedCandidate(n) && (pick == nil || n.stats.utilizedCPURatio < pick.stats.utilizedCPURatio) {
			pick = &n
		}
	}

	return pick
}

// Returns if the node is a candidate for removing it for compacting the pool.
// A node is considered a candidate if the following stands:
// - It is underutilized
// - Its scaledown is not in progress yet
// - It's older than one hour (to prevent continuously deleting newly created nodes)
func isNodeUnderutilizedCandidate(node nodeInfo) bool {
	return node.labels.enabled &&
		node.stats.utilizedCPURatio < node.labels.scaleDownCPURequestRatioLimit &&
		!node.labels.scaleDownInProgress &&
		*node.node.Metadata.CreationTimestamp.Seconds < time.Now().Unix()-3600
}

func collectNodeInfos(nodes []*corev1.Node, allPods []*corev1.Pod) []nodeInfo {
	nodeInfos := make([]nodeInfo, 0)

	for _, node := range nodes {
		podsOnNode := getPodsOnNode(node, allPods)
		nodeInfos = append(
			nodeInfos,
			nodeInfo{
				node:   node,
				labels: readNodeLabels(node),
				stats:  calculateNodeStats(node, podsOnNode),
				pods:   podsOnNode})
	}

	return nodeInfos
}

func getPodsOnNode(node *corev1.Node, allPods []*corev1.Pod) []*corev1.Pod {
	var podsOnNode []*corev1.Pod
	for _, pod := range allPods {
		if *pod.Spec.NodeName == *node.Metadata.Name {
			podsOnNode = append(podsOnNode, pod)
		}
	}

	return podsOnNode
}

func calculateNodeStats(node *corev1.Node, podsOnNode []*corev1.Pod) nodeStats {
	allocatableCPU := cpuReqStrToCPU(*node.Status.Allocatable["cpu"].String_)
	allocatableMemory := memoryReqStrToMemoryMB(*node.Status.Allocatable["memory"].String_)

	podsTotalCPUReq := 0
	podsTotalMemoryReq := 0

	for _, pod := range podsOnNode {
		cpuReq := 0
		memoryReq := 0
		for _, container := range pod.Spec.Containers {
			if container.Resources.Requests["cpu"] != nil {
				cpuReqStr := *container.Resources.Requests["cpu"].String_
				cpuReq += cpuReqStrToCPU(cpuReqStr)
			}

			if container.Resources.Requests["memory"] != nil {
				memoryReqStr := *container.Resources.Requests["memory"].String_
				memoryReq += memoryReqStrToMemoryMB(memoryReqStr)
			}
		}

		podsTotalCPUReq += cpuReq
		podsTotalMemoryReq += memoryReq
	}

	return nodeStats{
		allocatableCPU:      allocatableCPU,
		allocatableMemoryMB: allocatableMemory,
		totalCPURequests:    podsTotalCPUReq,
		totalMemoryRequests: podsTotalMemoryReq,
		utilizedCPURatio:    float64(podsTotalCPUReq) / float64(allocatableCPU),
		utilizedMemoryRatio: float64(podsTotalMemoryReq) / float64(allocatableMemory),
	}
}

func groupNodesByPool(nodes []*corev1.Node) map[string][]*corev1.Node {
	grouped := make(map[string][]*corev1.Node)

	for _, node := range nodes {
		pool := node.Metadata.Labels["cloud.google.com/gke-nodepool"]
		if ns, ok := grouped[pool]; ok {
			grouped[pool] = append(ns, node)
		} else {
			ns := make([]*corev1.Node, 1)
			ns[0] = node
			grouped[pool] = ns
		}
	}

	return grouped
}

func memoryReqStrToMemoryMB(str string) int {
	unit := str[len(str)-2:]
	str = str[:len(str)-2] // For example: 2000Mi
	memory, _ := strconv.Atoi(str)
	switch unit {
	case "Ki":
		return memory / 1024
	case "Mi":
		return memory
	default:
		return 0
	}
}

func cpuReqStrToCPU(str string) int {
	if str[len(str)-1:] == "m" {
		str = str[:len(str)-1] // For example: 1500m
		cpu, _ := strconv.Atoi(str)
		return cpu
	} else {
		coreCount, _ := strconv.Atoi(str) // For example: 3

		return coreCount * 1000
	}
}

func readNodeLabels(node *corev1.Node) nodeLabels {
	labels := nodeLabels{}

	enabledStr, ok := node.Metadata.Labels[labelNodeCompactorEnabled]
	if ok {
		e, err := strconv.ParseBool(enabledStr)
		if err == nil {
			labels.enabled = e
		} else {
			labels.enabled = false
		}
	} else {
		labels.enabled = false
	}

	if strings.HasPrefix(*node.Metadata.Name, "gke-production-europ-rulesapi-14-2662") {
		labels.enabled = true
	}

	ratioLimitStr, ok := node.Metadata.Labels[labelNodeCompactorScaleDownCPURequestRatioLimit]
	if ok {
		l, err := strconv.ParseFloat(ratioLimitStr, 64)
		if err == nil {
			labels.scaleDownCPURequestRatioLimit = l
		} else {
			labels.scaleDownCPURequestRatioLimit = 0
		}
	} else {
		labels.scaleDownCPURequestRatioLimit = 0
	}

	nodeCountStr, ok := node.Metadata.Labels[labelNodeCompactorScaleDownRequiredUnderutilizedNodeCount]
	if ok {
		c, err := strconv.ParseInt(nodeCountStr, 10, 0)
		if err == nil {
			labels.scaleDownRequiredUnderutilizedNodeCount = int(c)
		} else {
			labels.scaleDownRequiredUnderutilizedNodeCount = 0
		}
	} else {
		labels.scaleDownRequiredUnderutilizedNodeCount = 0
	}

	scaleDownInProgressStr, ok := node.Metadata.Labels[labelNodeCompactorScaleDownInProgress]
	if ok {
		i, err := strconv.ParseBool(scaleDownInProgressStr)
		if err == nil {
			labels.scaleDownInProgress = i
		} else {
			labels.scaleDownInProgress = false
		}
	} else {
		labels.scaleDownInProgress = false
	}

	labels.scaleDownCPURequestRatioLimit = 0.75
	labels.scaleDownRequiredUnderutilizedNodeCount = 4

	return labels
}

// loadClient parses a kubeconfig from a file and returns a Kubernetes client. It does not support extensions or client auth providers.
func loadClient(kubeconfigPath string) (*k8s.Client, error) {
	data, err := ioutil.ReadFile(kubeconfigPath)
	if err != nil {
		return nil, fmt.Errorf("read kubeconfig: %v", err)
	}

	// Unmarshal YAML into a Kubernetes config object.
	var config k8s.Config
	if err := yaml.Unmarshal(data, &config); err != nil {
		return nil, fmt.Errorf("unmarshal kubeconfig: %v", err)
	}
	return k8s.NewClient(&config)
}
