package main

import (
	"context"
	"encoding/json"
	"flag"
	"runtime"
	"strconv"
	"sync"
	"time"

	"github.com/alecthomas/kingpin"
	foundation "github.com/estafette/estafette-foundation"
	"github.com/rs/zerolog/log"

	"github.com/ericchiang/k8s"
	corev1 "github.com/ericchiang/k8s/apis/core/v1"
	metav1 "github.com/ericchiang/k8s/apis/meta/v1"
	"github.com/prometheus/client_golang/prometheus"
)

const (
	nodeCompactorConfigMapDataKey = "estafette-k8s-node-compactor-config.yaml"
	annotationNodeCompactorState  = "estafette.io/node-compactor-state"
	podSafeToEvictKey             = "cluster-autoscaler.kubernetes.io/safe-to-evict"
)

type nodePoolConfig struct {
	// Shows whether the node compactor is enabled for the node pool.
	Enabled bool `json:"enabled"`
	// Sets the percentage if under which the CPU utilization falls, the node gets deleted.
	ScaleDownCPURequestRatioLimit float64 `json:"scaleDownCPURequestRatioLimit"`
	// The number of nodes which need to be underutilized in order to do the compaction.
	// (If there is only one underutilized node, we shouldn't delete it, because its pods could not be moved anywhere else.)
	ScaleDownRequiredUnderutilizedNodeCount int `json:"scaleDownRequiredUnderutilizedNodeCount"`
}

type nodeCompactorConfigMap struct {
	NodePools map[string]nodePoolConfig `json:"nodePools"`
}

type nodeCompactorState struct {
	MarkedForRemoval    bool   `json:"markedForRemoval"`
	MarkedAt            string `json:"markedAt"`
	ScaleDownInProgress bool   `json:"scaleDownInProgress"`
	LastUpdated         string `json:"lastUpdated"`
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
	config nodePoolConfig
	state  nodeCompactorState
	stats  nodeStats
	pods   []*corev1.Pod
}

var (
	appgroup  string
	app       string
	version   string
	branch    string
	revision  string
	buildDate string
	goVersion = runtime.Version()
)

var (
	addr = flag.String("listen-address", ":9101", "The address to listen on for HTTP requests.")

	minimumNodeAgeSeconds                 = kingpin.Flag("minimum-node-age-seconds", "The number of seconds before a new node is inspected for compaction.").Default("1200").OverrideDefaultFromEnvar("MINIMUM_NODE_AGE_SECONDS").Int64()
	neededMarkedTimeForRemovalSeconds     = kingpin.Flag("needed-marked-time-for-seconds", "The number of seconds a node will be removed after marking it for removal.").Default("300").OverrideDefaultFromEnvar("NEEDED_MARKED_TIME_FOR_REMOVAL_SECONDS").Int64()
	sleepDurationBetweenIterationsSeconds = kingpin.Flag("sleep-duration-between-iterations-seconds", "The number of seconds between compaction runs.").Default("300").OverrideDefaultFromEnvar("SLEEP_DURATION_BETWEEN_ITERATIONS_SECONDS").Int()
	nodeCompactorConfigMapName            = kingpin.Flag("configmap-name", "Name of the configmap.").Default("estafette-k8s-node-compactor").OverrideDefaultFromEnvar("CONFIGMAP_NAME").String()

	// Create prometheus counter for the total number of nodes.
	nodesTotal = prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Name: "estafette_node_compactor_total_node_count",
		Help: "The total number of nodes in the node pool.",
	}, []string{"nodepool"})
	nodesUnderutilized = prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Name: "estafette_node_compactor_underutilized_node_count",
		Help: "The number of nodes considered underutilized in the node pool.",
	}, []string{"nodepool"})
	nodesMarkedForRemoval = prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Name: "estafette_node_compactor_marked_for_removal_node_count",
		Help: "The number of nodes in the node pool which are marked for removal at the moment.",
	}, []string{"nodepool"})
	nodesScaleDownInProgressTotal = prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Name: "estafette_node_compactor_scale_down_in_progress_node_count",
		Help: "The number of nodes in the node pool for which the scaledown is in progress.",
	}, []string{"nodepool"})

	// Create gauges for the various resource values.
	allocatableCpus = prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Name: "estafette_node_compactor_allocatable_cpu",
		Help: "The allocatable CPU value of the node.",
	}, []string{"node", "nodepool"})
	allocatableMemory = prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Name: "estafette_node_compactor_allocatable_memory",
		Help: "The allocatable memory value (in MB) of the node.",
	}, []string{"node", "nodepool"})
	totalCPURequests = prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Name: "estafette_node_compactor_total_cpu_requests",
		Help: "The total amount of CPU requests on the node.",
	}, []string{"node", "nodepool"})
	totalMemoryRequests = prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Name: "estafette_node_compactor_total_memory_requests",
		Help: "The total amount (in MB) of memory requests on the node.",
	}, []string{"node", "nodepool"})
	utilizedCPURatio = prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Name: "estafette_node_compactor_utilized_cpu_ratio",
		Help: "The utilized CPU ratio on the node.",
	}, []string{"node", "nodepool"})
	utilizedMemoryRatio = prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Name: "estafette_node_compactor_utilized_memory_ratio",
		Help: "The utilized memory ratio on the node",
	}, []string{"node", "nodepool"})
)

func init() {
	prometheus.MustRegister(nodesTotal)
	prometheus.MustRegister(nodesUnderutilized)
	prometheus.MustRegister(nodesMarkedForRemoval)
	prometheus.MustRegister(nodesScaleDownInProgressTotal)
	prometheus.MustRegister(allocatableCpus)
	prometheus.MustRegister(allocatableMemory)
	prometheus.MustRegister(totalCPURequests)
	prometheus.MustRegister(totalMemoryRequests)
	prometheus.MustRegister(utilizedCPURatio)
	prometheus.MustRegister(utilizedMemoryRatio)
}

func main() {
	// parse command line parameters
	kingpin.Parse()

	// init log format from envvar ESTAFETTE_LOG_FORMAT
	foundation.InitLoggingFromEnv(foundation.NewApplicationInfo(appgroup, app, version, branch, revision, buildDate))

	// init /liveness endpoint
	foundation.InitLiveness()

	client, err := k8s.NewInClusterClient()

	if err != nil {
		log.Fatal().Err(err).Msg("Could not create the K8s client.")
	}

	// start prometheus
	foundation.InitMetrics()

	// define channel used to gracefully shutdown the application
	gracefulShutdown, waitGroup := foundation.InitGracefulShutdownHandling()

	go func(waitGroup *sync.WaitGroup) {
		// Loop indefinitely.
		for {
			log.Info().Msg("Running node compaction process")

			// Run the main logic of the controller, which tries to compact the node pool.
			runNodeCompaction(client)

			// Sleep random time around 300 seconds.
			sleepTime := foundation.ApplyJitter(*sleepDurationBetweenIterationsSeconds)
			log.Info().Msgf("Sleeping for %v seconds...", sleepTime)
			time.Sleep(time.Duration(sleepTime) * time.Second)
		}
	}(waitGroup)

	foundation.HandleGracefulShutdown(gracefulShutdown, waitGroup)
}

func runNodeCompaction(client *k8s.Client) {
	var allPods corev1.PodList

	if err := client.List(context.Background(), k8s.AllNamespaces, &allPods); err != nil {
		log.Fatal().Err(err).Msg("Could not retrieve the list of pods.")
	}

	var nodes corev1.NodeList

	if err := client.List(context.Background(), k8s.AllNamespaces, &nodes); err != nil {
		log.Fatal().Err(err).Msg("Could not retrieve the list of nodes.")
	}

	nodesByPool := groupNodesByPool(nodes.Items)

	var configMap corev1.ConfigMap
	if err := client.Get(context.Background(), client.Namespace, *nodeCompactorConfigMapName, &configMap); err != nil {
		log.Fatal().Err(err).Msg("Could not retrieve the ConfigMap.")
	}

	var configMapData nodeCompactorConfigMap

	if err := json.Unmarshal([]byte(configMap.Data[nodeCompactorConfigMapDataKey]), &configMapData); err != nil {
		// Couldn't parse the config, we'll use the default
		log.Error().Err(err).Msg("Unmarshalling the node compactor config failed.")
	}

	// NOTE: We need to reset the metrics, otherwise information about already removed nodes would stay reported forever.
	resetNodePoolMetrics()

	for pool, nodes := range nodesByPool {
		log.Info().Msgf("Node pool: %s", pool)

		poolConfig, ok := configMapData.NodePools[pool]
		if !ok {
			poolConfig = nodePoolConfig{Enabled: false}
		}

		nodeInfos, err := collectNodeInfos(nodes, allPods.Items, poolConfig)

		if err != nil {
			log.Error().Err(err).Msgf("Collecting the node info failed on the pool %s, skipping the pool.", pool)
			continue
		}

		nodeCountUnderLimit := 0
		nodeCountMarkedForRemoval := 0
		nodeCountScaleDownInProgress := 0

		// For every node pool we check if there are enough nodes using less resources than the limit for scaledown.
		for _, nodeInfo := range nodeInfos {
			isNodeUnderutilized := isNodeUnderutilizedCandidate(nodeInfo)
			if isNodeUnderutilized {
				nodeCountUnderLimit++
			}

			if nodeInfo.state.MarkedForRemoval {
				nodeCountMarkedForRemoval++
			}

			if nodeInfo.state.ScaleDownInProgress {
				nodeCountScaleDownInProgress++
			}

			if poolConfig.Enabled {
				err = updateNodeMarkedState(nodeInfo, isNodeUnderutilized, client)
				if err != nil {
					log.Error().Err(err).Msg("Updating the marked state of the node has failed.")
					continue
				}
			}
		}

		reportNodePoolMetrics(pool, nodeInfos, nodeCountUnderLimit, nodeCountMarkedForRemoval, nodeCountScaleDownInProgress)

		log.Info().Msgf("Number of underutilized nodes: %d", nodeCountUnderLimit)
		log.Info().Msgf("Number of nodes marked for removal: %d", nodeCountMarkedForRemoval)
		log.Info().Msgf("Number of nodes already being removed: %d", nodeCountScaleDownInProgress)

		// We check if there are enough underutilized pods so that we can initiate a scaledown.
		// NOTE: We multiply by (nodeCountScaleDownInProgress + 1), because there might be nodes for which
		// we have initiated the scaledown in previous iterations already, which haven't been removed yet,
		// and we have to take these into account.
		if nodeCountUnderLimit >= poolConfig.ScaleDownRequiredUnderutilizedNodeCount+nodeCountScaleDownInProgress {
			pick := pickUnderutilizedNodeToRemove(nodeInfos)

			if pick == nil {
				log.Info().Msg("No node was picked for removal.")
			} else {
				log.Info().Msg("The node picked for removal:")
				log.Info().Msgf("Node %v", *pick.node.Metadata.Name)
				log.Info().Msgf("Allocatable CPU: %vm, memory: %vMi", pick.stats.allocatableCPU, pick.stats.allocatableMemoryMB)
				log.Info().Msgf("Pods on node total requests, CPU: %vm, memory: %vMi", pick.stats.totalCPURequests, pick.stats.totalMemoryRequests)
				log.Info().Msgf("CPU utilization: %v%%, memory utilization: %v%%", pick.stats.utilizedCPURatio*100, pick.stats.utilizedMemoryRatio*100)

				log.Info().Msg("Cordoning the node...")
				err := cordonAndMarkNode(pick.node, client)

				if err != nil {
					log.Error().Err(err).Msg("Cordoning the node has failed.")
					continue
				}

				log.Info().Msg("Draining the pods...")
				err = drainPods(pick, client)

				if err != nil {
					log.Error().Err(err).Msg("Draining the pods from the node has failed.")
					continue
				}
			}
		}
	}
}

func updateNodeMarkedState(node nodeInfo, isNodeUnderutilized bool, k8sClient *k8s.Client) error {
	if node.state.ScaleDownInProgress {
		return nil
	}

	update := false

	// We only update the node state if needed, we don't send a needless update, because it's resource-intensive, and updates can occasionally fail.
	if !node.state.MarkedForRemoval && isNodeUnderutilized {
		node.state = nodeCompactorState{MarkedForRemoval: true, MarkedAt: time.Now().Format(time.RFC3339), ScaleDownInProgress: false, LastUpdated: time.Now().Format(time.RFC3339)}
		update = true
	}

	if node.state.MarkedForRemoval && !isNodeUnderutilized {
		node.state = nodeCompactorState{MarkedForRemoval: false, MarkedAt: "", ScaleDownInProgress: false, LastUpdated: time.Now().Format(time.RFC3339)}
		update = true
	}

	if update {
		return updateNodeStateAnnotation(node.node, node.state, k8sClient)
	}

	return nil
}

func cordonAndMarkNode(node *corev1.Node, k8sClient *k8s.Client) error {
	*node.Spec.Unschedulable = true

	// We save the state so that in the next iteration we know that this node has already been picked for removal.
	newState := nodeCompactorState{ScaleDownInProgress: true, LastUpdated: time.Now().Format(time.RFC3339), MarkedForRemoval: false, MarkedAt: ""}

	return updateNodeStateAnnotation(node, newState, k8sClient)
}

func updateNodeStateAnnotation(node *corev1.Node, state nodeCompactorState, k8sClient *k8s.Client) error {
	nodeCompactorStateByteArray, err := json.Marshal(state)
	if err != nil {
		return err
	}
	node.Metadata.Annotations[annotationNodeCompactorState] = string(nodeCompactorStateByteArray)

	err = k8sClient.Update(context.Background(), node)

	return err
}

func drainPods(node *nodeInfo, k8sClient *k8s.Client) error {
	for _, pod := range filterPodsToDrain(node.pods) {
		err := k8sClient.Delete(context.Background(), pod)

		if err != nil {
			return err
		}
	}

	return nil
}

func filterPodsToDrain(pods []*corev1.Pod) (output []*corev1.Pod) {
	for _, pod := range pods {
		addPod := true

		// Skip DaemonSets
		for _, ownerRef := range pod.Metadata.OwnerReferences {
			if *ownerRef.Kind == "DaemonSet" {
				addPod = false
			}
		}

		// Skip pods in the kube-system namespace
		if *pod.Metadata.Namespace == "kube-system" {
			addPod = false
		}

		if addPod {
			output = append(output, pod)
		}
	}

	return
}

func hasLocalStorage(pod *corev1.Pod) bool {
	for _, volume := range pod.Spec.Volumes {
		isLocalVolume := volume.VolumeSource.HostPath != nil || volume.VolumeSource.EmptyDir != nil
		if isLocalVolume {
			return true
		}
	}
	return false
}

// Returns whether the pod is replicated.
// We consider a pod replicated if it has a controller reference of Kind "ReplicationController", "Job", "ReplicaSet" or "StatefulSet"
func isReplicated(pod *corev1.Pod) bool {
	controllerRef := getControllerRef(pod)

	if controllerRef == nil {
		return false
	}

	return *controllerRef.Kind == "ReplicationController" ||
		*controllerRef.Kind == "Job" ||
		*controllerRef.Kind == "ReplicaSet" ||
		*controllerRef.Kind == "StatefulSet"
}

func isDaemonSetPod(pod *corev1.Pod) bool {
	controllerRef := getControllerRef(pod)

	if controllerRef == nil {
		return false
	}

	return *controllerRef.Kind == "DaemonSet"
}

func getControllerRef(pod *corev1.Pod) (controllerRef *metav1.OwnerReference) {
	for _, ownerRef := range pod.Metadata.OwnerReferences {
		if *ownerRef.Controller {
			controllerRef = ownerRef
		}
	}

	return
}

// Returns whether the node has a pod which prevents the node from being removed.
// A pod prevents a node from being removed if either
// - It's safe to evict annotation is set to false
// - It has local storage (and its safe to evict annotation is not set to true)
// - It's not replicated
func hasPodWhichPreventsNodeRemoval(node nodeInfo) bool {
	for _, pod := range node.pods {
		if pod.Metadata.Annotations[podSafeToEvictKey] == "false" {
			return true
		}

		if *pod.Metadata.Namespace != "kube-system" && !isDaemonSetPod(pod) && hasLocalStorage(pod) && pod.Metadata.Annotations[podSafeToEvictKey] != "true" {
			return true
		}

		if *pod.Metadata.Namespace != "kube-system" && !isReplicated(pod) && !isDaemonSetPod(pod) {
			return true
		}
	}

	return false
}

// Picks a node to be removed. We pick the node with the lowest utilization.
func pickUnderutilizedNodeToRemove(nodes []nodeInfo) *nodeInfo {
	var pick *nodeInfo

	for i, n := range nodes {

		if isNodeMarkedForRemovalLongEnough(n) && !hasPodWhichPreventsNodeRemoval(n) && (pick == nil || n.stats.utilizedCPURatio < pick.stats.utilizedCPURatio) {
			pick = &nodes[i]
		}
	}

	return pick
}

func isNodeMarkedForRemovalLongEnough(node nodeInfo) bool {
	if !node.state.MarkedForRemoval {
		return false
	}

	markedAt, err := time.Parse(time.RFC3339, node.state.MarkedAt)

	if err != nil {
		return false
	}

	return int64(time.Now().Sub(markedAt).Seconds()) >= *neededMarkedTimeForRemovalSeconds
}

// Returns if the node is a candidate for removing it for compacting the pool.
// A node is considered a candidate if the following stands:
// - It is underutilized
// - Its scaledown is not in progress yet
// - It's older than one hour (to prevent continuously deleting newly created nodes)
func isNodeUnderutilizedCandidate(node nodeInfo) bool {
	return node.config.Enabled &&
		node.stats.utilizedCPURatio < node.config.ScaleDownCPURequestRatioLimit &&
		!node.state.ScaleDownInProgress &&
		*node.node.Metadata.CreationTimestamp.Seconds < time.Now().Unix()-*minimumNodeAgeSeconds
}

func collectNodeInfos(nodes []*corev1.Node, allPods []*corev1.Pod, poolConfig nodePoolConfig) ([]nodeInfo, error) {
	nodeInfos := make([]nodeInfo, 0)

	for _, node := range nodes {
		state, err := readNodeState(node)

		if err != nil {
			return nodeInfos, err
		}

		podsOnNode := getNonTerminatedPodsOnNode(node, allPods)
		nodeInfos = append(
			nodeInfos,
			nodeInfo{
				node:   node,
				config: poolConfig,
				stats:  calculateNodeStats(node, podsOnNode),
				state:  state,
				pods:   podsOnNode})
	}

	return nodeInfos, nil
}

func resetNodePoolMetrics() {
	nodesTotal.Reset()
	nodesUnderutilized.Reset()
	nodesMarkedForRemoval.Reset()
	nodesScaleDownInProgressTotal.Reset()
	allocatableCpus.Reset()
	allocatableMemory.Reset()
	totalCPURequests.Reset()
	totalMemoryRequests.Reset()
	utilizedCPURatio.Reset()
	utilizedMemoryRatio.Reset()
}

func reportNodePoolMetrics(pool string, nodes []nodeInfo, underutilizedCount int, markedForRemovalCount, scaledownInProgressCount int) {
	nodesTotal.WithLabelValues(pool).Set((float64(len(nodes))))
	nodesUnderutilized.WithLabelValues(pool).Set((float64(underutilizedCount)))
	nodesMarkedForRemoval.WithLabelValues(pool).Set((float64(markedForRemovalCount)))
	nodesScaleDownInProgressTotal.WithLabelValues(pool).Set((float64(scaledownInProgressCount)))

	for _, node := range nodes {
		allocatableCpus.WithLabelValues(*node.node.Metadata.Name, pool).Set(float64(node.stats.allocatableCPU))
		allocatableMemory.WithLabelValues(*node.node.Metadata.Name, pool).Set(float64(node.stats.allocatableMemoryMB))
		totalCPURequests.WithLabelValues(*node.node.Metadata.Name, pool).Set(float64(node.stats.totalCPURequests))
		totalMemoryRequests.WithLabelValues(*node.node.Metadata.Name, pool).Set(float64(node.stats.totalMemoryRequests))
		utilizedCPURatio.WithLabelValues(*node.node.Metadata.Name, pool).Set(float64(node.stats.utilizedCPURatio))
		utilizedMemoryRatio.WithLabelValues(*node.node.Metadata.Name, pool).Set(float64(node.stats.utilizedMemoryRatio))
	}
}

func getNonTerminatedPodsOnNode(node *corev1.Node, allPods []*corev1.Pod) []*corev1.Pod {
	var podsOnNode []*corev1.Pod
	for _, pod := range allPods {
		if *pod.Spec.NodeName == *node.Metadata.Name && *pod.Status.Phase != "Succeeded" && *pod.Status.Phase != "Failed" {
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
	}

	coreCount, _ := strconv.Atoi(str) // For example: 3

	return coreCount * 1000
}

func readNodeState(node *corev1.Node) (state nodeCompactorState, err error) {
	stateStr, ok := node.Metadata.Annotations[annotationNodeCompactorState]
	if ok {
		state = nodeCompactorState{}
		err := json.Unmarshal([]byte(stateStr), &state)
		if err != nil {
			return state, err
		}
	} else {
		state = nodeCompactorState{ScaleDownInProgress: false, LastUpdated: ""}
	}

	return state, nil
}
