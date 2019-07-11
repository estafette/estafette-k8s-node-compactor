package main

import (
	"context"
	"encoding/json"
	"flag"
	stdlog "log"
	"math/rand"
	"net/http"
	"os"
	"os/signal"
	"runtime"
	"strconv"
	"sync"
	"syscall"
	"time"

	"github.com/rs/zerolog"
	"github.com/rs/zerolog/log"

	"github.com/ericchiang/k8s"
	corev1 "github.com/ericchiang/k8s/apis/core/v1"
	metav1 "github.com/ericchiang/k8s/apis/meta/v1"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"
)

const (
	labelNodeCompactorEnabled                                 = "estafette.io/node-compactor-enabled"
	labelNodeCompactorScaleDownCPURequestRatioLimit           = "estafette.io/node-compactor-scale-down-cpu-request-ratio-limit"
	labelNodeCompactorScaleDownRequiredUnderutilizedNodeCount = "estafette.io/node-compactor-scale-down-required-underutilized-node-count"
	annotationNodeCompactorState                              = "estafette.io/node-compactor-state"
	podSafeToEvictKey                                         = "cluster-autoscaler.kubernetes.io/safe-to-evict"
)

type nodeCompactorState struct {
	ScaleDownInProgress bool   `json:"scaleDownInProgress"`
	LastUpdated         string `json:"lastUpdated"`
}

type nodeLabels struct {
	// Shows whether the node compactor is enabled for the node.
	enabled bool
	// Sets the percentage if under which the CPU utilization falls, the node gets deleted.
	scaleDownCPURequestRatioLimit float64
	// The number of nodes which need to be underutilized in order to do the compaction.
	// (If there is only one underutilized node, we shouldn't delete it, because its pods could not be moved anywhere else.)
	scaleDownRequiredUnderutilizedNodeCount int
	// Stores the state saved by the compactor controller.
	state nodeCompactorState
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

var (
	app       string
	version   string
	branch    string
	revision  string
	buildDate string
	goVersion = runtime.Version()
)

var (
	addr = flag.String("listen-address", ":9101", "The address to listen on for HTTP requests.")

	// Seed random number.
	r = rand.New(rand.NewSource(time.Now().UnixNano()))

	// Create prometheus counter for the total number of nodes.
	nodesTotal = prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Name: "estafette_node_compactor_node_count",
		Help: "The number of nodes in the node pool",
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
	prometheus.MustRegister(allocatableCpus)
	prometheus.MustRegister(allocatableMemory)
	prometheus.MustRegister(totalCPURequests)
	prometheus.MustRegister(totalMemoryRequests)
	prometheus.MustRegister(utilizedCPURatio)
	prometheus.MustRegister(utilizedMemoryRatio)
}

func main() {
	// Parse command line parameters.
	flag.Parse()

	// Log as severity for stackdriver logging to recognize the level.
	zerolog.LevelFieldName = "severity"

	// Set some default fields added to all logs.
	log.Logger = zerolog.New(os.Stdout).With().
		Timestamp().
		Str("app", "estafette-k8s-hpa-scaler").
		Str("version", version).
		Logger()

	// Use zerolog for any logs sent via standard log library.
	stdlog.SetFlags(0)
	stdlog.SetOutput(log.Logger)

	log.Info().
		Str("app", app).
		Str("branch", branch).
		Str("revision", revision).
		Str("buildDate", buildDate).
		Str("goVersion", goVersion).
		Msg("Starting estafette-k8s-hpa-scaler...")

	client, err := k8s.NewInClusterClient()

	if err != nil {
		log.Fatal().Err(err).Msg("Could not create the K8s client.")
	}

	// Start prometheus.
	go func() {
		log.Debug().
			Str("port", *addr).
			Msg("Serving Prometheus metrics...")

		http.Handle("/metrics", promhttp.Handler())

		if err := http.ListenAndServe(*addr, nil); err != nil {
			log.Fatal().Err(err).Msg("Starting Prometheus listener failed")
		}
	}()

	// Define channel used to gracefully shut down the application.
	gracefulShutdown := make(chan os.Signal)

	signal.Notify(gracefulShutdown, syscall.SIGTERM, syscall.SIGINT)

	waitGroup := &sync.WaitGroup{}

	go func(waitGroup *sync.WaitGroup) {
		// Loop indefinitely.
		for {
			log.Info().Msg("Running node compaction process")

			// Run the main logic of the controller, which tries to compact the node pool.
			runNodeCompaction(client)

			// Sleep random time around 300 seconds.
			sleepTime := getSleepTime()
			log.Info().Msgf("Sleeping for %v seconds...", sleepTime)
			time.Sleep(time.Duration(sleepTime) * time.Second)
		}
	}(waitGroup)

	signalReceived := <-gracefulShutdown
	log.Info().
		Msgf("Received signal %v. Waiting for running tasks to finish...", signalReceived)

	waitGroup.Wait()

	log.Info().Msg("Shutting down...")
}

func runNodeCompaction(client *k8s.Client) {
	var nodes corev1.NodeList
	if err := client.List(context.Background(), k8s.AllNamespaces, &nodes); err != nil {
		log.Fatal().Err(err).Msg("Could not retrieve the list of nodes.")
	}

	var allPods corev1.PodList

	if err := client.List(context.Background(), k8s.AllNamespaces, &allPods); err != nil {
		log.Fatal().Err(err).Msg("Could not retrieve the list of pods.")
	}

	nodesByPool := groupNodesByPool(nodes.Items)

	// NOTE: We need to reset the metrics, otherwise information about already removed nodes would stay reported forever.
	resetNodePoolMetrics()

	for pool, nodes := range nodesByPool {
		log.Info().Msgf("Node pool: %s", pool)

		nodeInfos, err := collectNodeInfos(nodes, allPods.Items)

		if err != nil {
			log.Error().Err(err).Msgf("Collecting the node info failed on the pool %s, skipping the pool.", pool)
			continue
		}

		reportNodePoolMetrics(pool, nodeInfos)

		nodeCountUnderLimit := 0
		nodeCountScaleDownInProgress := 0

		// For every node pool we check if there are enough nodes using less resources than the limit for scaledown.
		for _, nodeInfo := range nodeInfos {
			nodeLabels := nodeInfo.labels

			if isNodeUnderutilizedCandidate(nodeInfo) {
				nodeCountUnderLimit++
			}

			if nodeLabels.state.ScaleDownInProgress {
				nodeCountScaleDownInProgress++
			}
		}

		log.Info().Msgf("Number of underutilized nodes: %d", nodeCountUnderLimit)
		log.Info().Msgf("Number of nodes already being removed: %d", nodeCountScaleDownInProgress)

		// We check if there are enough underutilized pods so that we can initiate a scaledown.
		// NOTE: We multiply by (nodeCountScaleDownInProgress + 1), because there might be nodes for which
		// we have initiated the scaledown in previous iterations already, which haven't been removed yet,
		// and we have to take these into account.
		if nodeCountUnderLimit >= nodeInfos[0].labels.scaleDownRequiredUnderutilizedNodeCount*(nodeCountScaleDownInProgress+1) {
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

func cordonAndMarkNode(node *corev1.Node, k8sClient *k8s.Client) error {
	*node.Spec.Unschedulable = true

	// We add an explicit label so in the next iteration we know that this node has already been picked for removal.
	newState := nodeCompactorState{ScaleDownInProgress: true, LastUpdated: time.Now().Format(time.RFC3339)}
	nodeCompactorStateByteArray, err := json.Marshal(newState)
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
		if isNodeUnderutilizedCandidate(n) && !hasPodWhichPreventsNodeRemoval(n) && (pick == nil || n.stats.utilizedCPURatio < pick.stats.utilizedCPURatio) {
			pick = &nodes[i]
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
		!node.labels.state.scaleDownInProgress &&
		*node.node.Metadata.CreationTimestamp.Seconds < time.Now().Unix()-3600
}

func collectNodeInfos(nodes []*corev1.Node, allPods []*corev1.Pod) ([]nodeInfo, error) {
	nodeInfos := make([]nodeInfo, 0)

	for _, node := range nodes {
		labels, err := readNodeLabels(node)

		if err != nil {
			return nodeInfos, err
		}

		podsOnNode := getNonTerminatedPodsOnNode(node, allPods)
		nodeInfos = append(
			nodeInfos,
			nodeInfo{
				node:   node,
				labels: labels,
				stats:  calculateNodeStats(node, podsOnNode),
				pods:   podsOnNode})
	}

	return nodeInfos, nil
}

func resetNodePoolMetrics() {
	nodesTotal.Reset()
	allocatableCpus.Reset()
	allocatableMemory.Reset()
	totalCPURequests.Reset()
	totalMemoryRequests.Reset()
	utilizedCPURatio.Reset()
	utilizedMemoryRatio.Reset()
}

func reportNodePoolMetrics(pool string, nodes []nodeInfo) {
	nodesTotal.WithLabelValues(pool).Set((float64(len(nodes))))

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

func readNodeLabels(node *corev1.Node) (nodeLabels, error) {
	labels := nodeLabels{}

	enabledStr, ok := node.Metadata.Labels[labelNodeCompactorEnabled]
	if ok {
		e, err := strconv.ParseBool(enabledStr)
		if err == nil {
			labels.enabled = e
		} else {
			return labels, err
		}
	} else {
		labels.enabled = false
	}

	ratioLimitStr, ok := node.Metadata.Labels[labelNodeCompactorScaleDownCPURequestRatioLimit]
	if ok {
		l, err := strconv.ParseFloat(ratioLimitStr, 64)
		if err == nil {
			labels.scaleDownCPURequestRatioLimit = l
		} else {
			return labels, err
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
			return labels, err
		}
	} else {
		labels.scaleDownRequiredUnderutilizedNodeCount = 0
	}

	stateStr, ok := node.Metadata.Annotations[annotationNodeCompactorState]
	if ok {
		state := nodeCompactorState{}
		err := json.Unmarshal([]byte(stateStr), &state)
		if err == nil {
			labels.state = state
		} else {
			return labels, err
		}
	} else {
		labels.state = nodeCompactorState{ScaleDownInProgress: false, LastUpdated: ""}
	}

	return labels, nil
}

func getSleepTime() int {
	sleepDurationStr := os.Getenv("SLEEP_DURATION_BETWEEN_ITERATIONS_SECONDS")
	sleepDuration := 300
	if i, err := strconv.ParseInt(sleepDurationStr, 0, 32); err == nil {
		sleepDuration = int(i)
	}

	return applyJitter(sleepDuration)
}

func applyJitter(input int) (output int) {
	deviation := int(0.25 * float64(input))

	return input - deviation + r.Intn(2*deviation)
}
