package handler

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"

	"k8s.io/client-go/kubernetes"

	"github.com/Masterminds/semver"
	"github.com/go-kit/kit/endpoint"
	"github.com/gorilla/mux"

	apiv1 "github.com/kubermatic/kubermatic/api/pkg/api/v1"
	apiv2 "github.com/kubermatic/kubermatic/api/pkg/api/v2"
	"github.com/kubermatic/kubermatic/api/pkg/provider"
	machineresource "github.com/kubermatic/kubermatic/api/pkg/resources/machine"
	apierrors "github.com/kubermatic/kubermatic/api/pkg/util/errors"
	k8cerrors "github.com/kubermatic/kubermatic/api/pkg/util/errors"

	machineconversions "github.com/kubermatic/kubermatic/api/pkg/machine"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/rand"
	"k8s.io/apimachinery/pkg/util/sets"
	clusterv1alpha1 "sigs.k8s.io/cluster-api/pkg/apis/cluster/v1alpha1"
	clusterv1alpha1clientset "sigs.k8s.io/cluster-api/pkg/client/clientset_generated/clientset"
)

const (
	kubeletVersionConstraint = ">= 1.8"
	errGlue                  = " & "

	initialConditionParsingDelay = 5
)

func deleteNodeForCluster(projectProvider provider.ProjectProvider) endpoint.Endpoint {
	return func(ctx context.Context, request interface{}) (interface{}, error) {
		req := request.(DeleteNodeForClusterReq)
		clusterProvider := ctx.Value(clusterProviderContextKey).(provider.ClusterProvider)
		userInfo := ctx.Value(userInfoContextKey).(*provider.UserInfo)

		_, err := projectProvider.Get(userInfo, req.ProjectID, &provider.ProjectGetOptions{})
		if err != nil {
			return nil, kubernetesErrorToHTTPError(err)
		}

		cluster, err := clusterProvider.Get(userInfo, req.ClusterID, &provider.ClusterGetOptions{})
		if err != nil {
			return nil, kubernetesErrorToHTTPError(err)
		}

		// TODO:
		// normally we have project, user and sshkey providers
		// but here we decided to use machineClient and kubeClient directly to access the user cluster.
		//
		machineClient, err := clusterProvider.GetMachineClientForCustomerCluster(cluster)
		if err != nil {
			return nil, fmt.Errorf("failed to create a machine client: %v", err)
		}

		kubeClient, err := clusterProvider.GetKubernetesClientForCustomerCluster(cluster)
		if err != nil {
			return nil, fmt.Errorf("failed to create a kubernetes client: %v", err)
		}

		machine, node, err := findMachineAndNode(req.NodeID, machineClient, kubeClient)
		if err != nil {
			return nil, err
		}
		if machine == nil && node == nil {
			return nil, k8cerrors.NewNotFound("Node", req.NodeID)
		}

		if machine != nil {
			return nil, kubernetesErrorToHTTPError(machineClient.ClusterV1alpha1().Machines(machine.Namespace).Delete(machine.Name, nil))
		} else if node != nil {
			return nil, kubernetesErrorToHTTPError(kubeClient.CoreV1().Nodes().Delete(node.Name, nil))
		}
		return nil, nil
	}
}

func listNodesForCluster(projectProvider provider.ProjectProvider) endpoint.Endpoint {
	return func(ctx context.Context, request interface{}) (interface{}, error) {
		req := request.(ListNodesForClusterReq)
		clusterProvider := ctx.Value(clusterProviderContextKey).(provider.ClusterProvider)
		userInfo := ctx.Value(userInfoContextKey).(*provider.UserInfo)

		_, err := projectProvider.Get(userInfo, req.ProjectID, &provider.ProjectGetOptions{})
		if err != nil {
			return nil, kubernetesErrorToHTTPError(err)
		}

		cluster, err := clusterProvider.Get(userInfo, req.ClusterID, &provider.ClusterGetOptions{})
		if err != nil {
			return nil, kubernetesErrorToHTTPError(err)
		}

		// TODO:
		// normally we have project, user and sshkey providers
		// but here we decided to use machineClient and kubeClient directly to access the user cluster.
		//
		// how about moving machineClient and kubeClient to their own provider ?
		machineClient, err := clusterProvider.GetMachineClientForCustomerCluster(cluster)
		if err != nil {
			return nil, kubernetesErrorToHTTPError(err)
		}

		kubeClient, err := clusterProvider.GetKubernetesClientForCustomerCluster(cluster)
		if err != nil {
			return nil, kubernetesErrorToHTTPError(err)
		}

		machineList, err := machineClient.ClusterV1alpha1().Machines(metav1.NamespaceSystem).List(metav1.ListOptions{})
		if err != nil {
			return nil, fmt.Errorf("failed to load machines from cluster: %v", err)
		}

		nodeList, err := kubeClient.CoreV1().Nodes().List(metav1.ListOptions{})
		if err != nil {
			return nil, kubernetesErrorToHTTPError(err)
		}

		//The following is a bit tricky. We might have a node which is not created by a machine and vice versa...
		var nodesV2 []*apiv2.LegacyNode
		matchedMachineNodes := sets.NewString()

		//Go over all machines first
		for i := range machineList.Items {
			node := getNodeForMachine(&machineList.Items[i], nodeList.Items)
			if node != nil {
				matchedMachineNodes.Insert(string(node.UID))
			}
			outNode, err := outputMachine(&machineList.Items[i], node, req.HideInitialConditions)
			if err != nil {
				return nil, fmt.Errorf("failed to output machine %s: %v", machineList.Items[i].Name, err)
			}
			nodesV2 = append(nodesV2, outNode)
		}

		// Now all nodes, which do not belong to a machine - Relevant for BYO
		for i := range nodeList.Items {
			if !matchedMachineNodes.Has(string(nodeList.Items[i].UID)) {
				nodesV2 = append(nodesV2, outputNode(&nodeList.Items[i], req.HideInitialConditions))
			}
		}
		return convertNodesV2ToNodesV1(nodesV2), nil
	}
}

func getNodeForCluster(projectProvider provider.ProjectProvider) endpoint.Endpoint {
	return func(ctx context.Context, request interface{}) (interface{}, error) {
		req := request.(NodeReq)
		clusterProvider := ctx.Value(clusterProviderContextKey).(provider.ClusterProvider)
		userInfo := ctx.Value(userInfoContextKey).(*provider.UserInfo)

		_, err := projectProvider.Get(userInfo, req.ProjectID, &provider.ProjectGetOptions{})
		if err != nil {
			return nil, kubernetesErrorToHTTPError(err)
		}

		cluster, err := clusterProvider.Get(userInfo, req.ClusterID, &provider.ClusterGetOptions{})
		if err != nil {
			return nil, kubernetesErrorToHTTPError(err)
		}

		machineClient, err := clusterProvider.GetMachineClientForCustomerCluster(cluster)
		if err != nil {
			return nil, kubernetesErrorToHTTPError(err)
		}

		// TODO:
		// normally we have project, user and sshkey providers
		// but here we decided to use machineClient and kubeClient directly to access the user cluster.
		//
		// how about moving machineClient and kubeClient to their own provider ?
		kubeClient, err := clusterProvider.GetKubernetesClientForCustomerCluster(cluster)
		if err != nil {
			return nil, kubernetesErrorToHTTPError(err)
		}

		machine, node, err := findMachineAndNode(req.NodeID, machineClient, kubeClient)
		if err != nil {
			return nil, err
		}
		if machine == nil && node == nil {
			return nil, apierrors.NewNotFound("Node", req.NodeID)
		}

		if machine == nil {
			return convertNodeV2ToNodeV1(outputNode(node, req.HideInitialConditions)), nil
		}

		nodeV2, err := outputMachine(machine, node, req.HideInitialConditions)
		if err != nil {
			return nil, err
		}
		return convertNodeV2ToNodeV1(nodeV2), nil
	}
}

func createNodeForCluster(sshKeyProvider provider.SSHKeyProvider, projectProvider provider.ProjectProvider, dcs map[string]provider.DatacenterMeta) endpoint.Endpoint {
	return func(ctx context.Context, request interface{}) (interface{}, error) {
		req := request.(CreateNodeReq)
		clusterProvider := ctx.Value(clusterProviderContextKey).(provider.ClusterProvider)
		userInfo := ctx.Value(userInfoContextKey).(*provider.UserInfo)

		project, err := projectProvider.Get(userInfo, req.ProjectID, &provider.ProjectGetOptions{})
		if err != nil {
			return nil, kubernetesErrorToHTTPError(err)
		}

		cluster, err := clusterProvider.Get(userInfo, req.ClusterID, &provider.ClusterGetOptions{CheckInitStatus: true})
		if err != nil {
			return nil, kubernetesErrorToHTTPError(err)
		}

		keys, err := sshKeyProvider.List(project, &provider.SSHKeyListOptions{ClusterName: req.ClusterID})
		if err != nil {
			return nil, kubernetesErrorToHTTPError(err)
		}

		// TODO:
		// normally we have project, user and sshkey providers
		// but here we decided to use machineClient and kubeClient directly to access the user cluster.
		//
		// how about moving machineClient and kubeClient to their own provider ?
		machineClient, err := clusterProvider.GetMachineClientForCustomerCluster(cluster)
		if err != nil {
			return nil, kubernetesErrorToHTTPError(err)
		}

		dc, found := dcs[cluster.Spec.Cloud.DatacenterName]
		if !found {
			return nil, fmt.Errorf("unknown cluster datacenter %s", cluster.Spec.Cloud.DatacenterName)
		}

		node := convertNodeV1ToNodeV2(&req.Body)
		if node.Spec.Cloud.Openstack == nil &&
			node.Spec.Cloud.Digitalocean == nil &&
			node.Spec.Cloud.AWS == nil &&
			node.Spec.Cloud.Hetzner == nil &&
			node.Spec.Cloud.VSphere == nil &&
			node.Spec.Cloud.Azure == nil {
			return nil, errors.NewBadRequest("cannot create node without cloud provider")
		}

		//TODO(mrIncompetent): We need to make the kubelet version configurable but restrict it to master version
		if node.Spec.Versions.Kubelet != "" {
			kversion, err := semver.NewVersion(node.Spec.Versions.Kubelet)
			if err != nil {
				return nil, fmt.Errorf("failed to parse kubelet version: %v", err)
			}
			c, err := semver.NewConstraint(kubeletVersionConstraint)
			if err != nil {
				return nil, fmt.Errorf("failed to parse kubelet constraint version: %v", err)
			}

			if !c.Check(kversion) {
				return nil, fmt.Errorf("kubelet version does not fit constraint. Allowed %s", kubeletVersionConstraint)
			}
			node.Spec.Versions.Kubelet = kversion.String()
		} else {
			//TODO(mrIncompetent): rework the versions
			node.Spec.Versions.Kubelet = cluster.Spec.Version
		}

		if node.Metadata.Name == "" {
			node.Metadata.Name = "kubermatic-" + cluster.Name + "-" + rand.String(5)
		}

		// Create machine resource
		machine, err := machineresource.Machine(cluster, node, dc, keys)
		if err != nil {
			return nil, fmt.Errorf("failed to create machine from template: %v", err)
		}

		// Send machine resource to k8s
		machine, err = machineClient.ClusterV1alpha1().Machines(machine.Namespace).Create(machine)
		if err != nil {
			return nil, fmt.Errorf("failed to create machine: %v", err)
		}

		nodeV2, err := outputMachine(machine, nil, false)
		if err != nil {
			return nil, err
		}
		return convertNodeV2ToNodeV1(nodeV2), nil
	}
}

func convertNodeV1ToNodeV2(nodeV1 *apiv1.Node) *apiv2.LegacyNode {
	return &apiv2.LegacyNode{
		Metadata: apiv2.LegacyObjectMeta{
			Name:              nodeV1.ID,
			DisplayName:       nodeV1.Name,
			CreationTimestamp: nodeV1.CreationTimestamp,
			DeletionTimestamp: nodeV1.DeletionTimestamp,
		},
		Spec:   nodeV1.Spec,
		Status: nodeV1.Status,
	}
}

func convertNodeV2ToNodeV1(nodeV2 *apiv2.LegacyNode) *apiv1.Node {
	return &apiv1.Node{
		ObjectMeta: apiv1.ObjectMeta{
			ID:                nodeV2.Metadata.Name,
			Name:              nodeV2.Metadata.DisplayName,
			CreationTimestamp: nodeV2.Metadata.CreationTimestamp,
			DeletionTimestamp: nodeV2.Metadata.DeletionTimestamp,
		},
		Spec:   nodeV2.Spec,
		Status: nodeV2.Status,
	}
}

func convertNodesV2ToNodesV1(nodesV2 []*apiv2.LegacyNode) []*apiv1.Node {
	nodesV1 := make([]*apiv1.Node, len(nodesV2))
	for index, nodeV2 := range nodesV2 {

		nodesV1[index] = convertNodeV2ToNodeV1(nodeV2)
	}
	return nodesV1
}

func outputNode(node *corev1.Node, hideInitialNodeConditions bool) *apiv2.LegacyNode {
	nodeStatus := apiv2.NodeStatus{}
	nodeStatus = apiNodeStatus(nodeStatus, node, hideInitialNodeConditions)
	var deletionTimestamp *time.Time
	if node.DeletionTimestamp != nil {
		deletionTimestamp = &node.DeletionTimestamp.Time
	}

	return &apiv2.LegacyNode{
		Metadata: apiv2.LegacyObjectMeta{
			Name:              node.Name,
			DisplayName:       node.Name,
			Labels:            node.Labels,
			Annotations:       node.Annotations,
			DeletionTimestamp: deletionTimestamp,
			CreationTimestamp: node.CreationTimestamp.Time,
		},
		Spec: apiv2.NodeSpec{
			Versions:        apiv2.NodeVersionInfo{},
			OperatingSystem: apiv2.OperatingSystemSpec{},
			Cloud:           apiv2.NodeCloudSpec{},
		},
		Status: nodeStatus,
	}
}

func apiNodeStatus(status apiv2.NodeStatus, inputNode *corev1.Node, hideInitialNodeConditions bool) apiv2.NodeStatus {
	for _, address := range inputNode.Status.Addresses {
		status.Addresses = append(status.Addresses, apiv2.NodeAddress{
			Type:    string(address.Type),
			Address: string(address.Address),
		})
	}

	if !hideInitialNodeConditions || time.Since(inputNode.CreationTimestamp.Time).Minutes() > initialConditionParsingDelay {
		reason, message := parseNodeConditions(inputNode)
		status.ErrorReason += reason
		status.ErrorMessage += message
	}

	status.Allocatable.Memory = inputNode.Status.Allocatable.Memory().String()
	status.Allocatable.CPU = inputNode.Status.Allocatable.Cpu().String()

	status.Capacity.Memory = inputNode.Status.Capacity.Memory().String()
	status.Capacity.CPU = inputNode.Status.Capacity.Cpu().String()

	status.NodeInfo.OperatingSystem = inputNode.Status.NodeInfo.OperatingSystem
	status.NodeInfo.KubeletVersion = inputNode.Status.NodeInfo.KubeletVersion
	status.NodeInfo.Architecture = inputNode.Status.NodeInfo.Architecture
	return status
}

func outputMachine(machine *clusterv1alpha1.Machine, node *corev1.Node, hideInitialNodeConditions bool) (*apiv2.LegacyNode, error) {
	displayName := machine.Spec.Name
	labels := map[string]string{}
	annotations := map[string]string{}
	nodeStatus := apiv2.NodeStatus{}
	nodeStatus.MachineName = machine.Name
	var deletionTimestamp *time.Time
	if machine.DeletionTimestamp != nil {
		deletionTimestamp = &machine.DeletionTimestamp.Time
	}

	if machine.Status.ErrorReason != nil {
		nodeStatus.ErrorReason += string(*machine.Status.ErrorReason) + errGlue
		nodeStatus.ErrorMessage += string(*machine.Status.ErrorMessage) + errGlue
	}

	operatingSystemSpec, err := machineconversions.GetAPIV2OperatingSystemSpec(machine)
	if err != nil {
		return nil, fmt.Errorf("failed to get operating system spec from machine: %v", err)
	}

	cloudSpec, err := machineconversions.GetAPIV2NodeCloudSpec(machine)
	if err != nil {
		return nil, fmt.Errorf("failed to get node cloud spec from machine: %v", err)
	}

	if node != nil {
		if node.Name != machine.Spec.Name {
			displayName = node.Name
		}

		labels = node.Labels
		annotations = node.Annotations
		nodeStatus = apiNodeStatus(nodeStatus, node, hideInitialNodeConditions)
	}

	nodeStatus.ErrorReason = strings.TrimSuffix(nodeStatus.ErrorReason, errGlue)
	nodeStatus.ErrorMessage = strings.TrimSuffix(nodeStatus.ErrorMessage, errGlue)

	return &apiv2.LegacyNode{
		Metadata: apiv2.LegacyObjectMeta{
			Name:              machine.Name,
			DisplayName:       displayName,
			Labels:            labels,
			Annotations:       annotations,
			DeletionTimestamp: deletionTimestamp,
			CreationTimestamp: machine.CreationTimestamp.Time,
		},
		Spec: apiv2.NodeSpec{
			Versions: apiv2.NodeVersionInfo{
				Kubelet: machine.Spec.Versions.Kubelet,
			},
			OperatingSystem: *operatingSystemSpec,
			Cloud:           *cloudSpec,
		},
		Status: nodeStatus,
	}, nil
}

func parseNodeConditions(node *corev1.Node) (reason string, message string) {
	for _, condition := range node.Status.Conditions {
		goodConditionType := condition.Type == corev1.NodeReady || condition.Type == corev1.NodeKubeletConfigOk
		if goodConditionType && condition.Status != corev1.ConditionTrue {
			reason += condition.Reason + errGlue
			message += condition.Message + errGlue
		} else if !goodConditionType && condition.Status == corev1.ConditionTrue {
			reason += condition.Reason + errGlue
			message += condition.Message + errGlue
		}
	}
	return reason, message
}

func getNodeForMachine(machine *clusterv1alpha1.Machine, nodes []corev1.Node) *corev1.Node {
	for _, node := range nodes {
		if (machine.Status.NodeRef != nil && node.UID == machine.Status.NodeRef.UID) || node.Name == machine.Name {
			return &node
		}
	}
	return nil
}

func getMachineForNode(node *corev1.Node, machines []clusterv1alpha1.Machine) *clusterv1alpha1.Machine {
	ref := metav1.GetControllerOf(node)
	if ref == nil {
		return nil
	}
	for _, machine := range machines {
		if ref.UID == machine.UID {
			return &machine
		}
	}
	return nil
}

func findMachineAndNode(name string, machineClient clusterv1alpha1clientset.Interface, kubeClient kubernetes.Interface) (*clusterv1alpha1.Machine, *corev1.Node, error) {
	machineList, err := machineClient.ClusterV1alpha1().Machines(metav1.NamespaceSystem).List(metav1.ListOptions{})
	if err != nil {
		return nil, nil, fmt.Errorf("failed to load machines from cluster: %v", err)
	}

	nodeList, err := kubeClient.CoreV1().Nodes().List(metav1.ListOptions{})
	if err != nil {
		return nil, nil, fmt.Errorf("failed to load nodes from cluster: %v", err)
	}

	var node *corev1.Node
	var machine *clusterv1alpha1.Machine

	for i, n := range nodeList.Items {
		if n.Name == name {
			node = &nodeList.Items[i]
			break
		}
	}

	for i, m := range machineList.Items {
		if m.Name == name {
			machine = &machineList.Items[i]
			break
		}
	}

	//Check if we can get a owner ref from a machine
	if node != nil && machine == nil {
		machine = getMachineForNode(node, machineList.Items)
	}

	if machine != nil && node == nil {
		node = getNodeForMachine(machine, nodeList.Items)
	}

	return machine, node, nil
}

// DeleteNodeForClusterReq defines HTTP request for deleteNodeForCluster
// swagger:parameters deleteNodeForCluster
type DeleteNodeForClusterReq struct {
	GetClusterReq
	// in: path
	NodeID string `json:"node_id"`
}

func decodeDeleteNodeForCluster(c context.Context, r *http.Request) (interface{}, error) {
	var req DeleteNodeForClusterReq

	nodeID := mux.Vars(r)["node_id"]
	if nodeID == "" {
		return "", fmt.Errorf("'node_id' parameter is required but was not provided")
	}

	clusterID, err := decodeClusterID(c, r)
	if err != nil {
		return nil, err
	}

	dcr, err := decodeDcReq(c, r)
	if err != nil {
		return nil, err
	}

	req.ClusterID = clusterID
	req.NodeID = nodeID
	req.DCReq = dcr.(DCReq)

	return req, nil
}

// ListNodesForClusterReq defines HTTP request for listNodesForCluster
// swagger:parameters listNodesForCluster
type ListNodesForClusterReq struct {
	GetClusterReq
	// in: query
	HideInitialConditions bool `json:"hideInitialConditions"`
}

func decodeListNodesForCluster(c context.Context, r *http.Request) (interface{}, error) {
	var req ListNodesForClusterReq

	clusterID, err := decodeClusterID(c, r)
	if err != nil {
		return nil, err
	}

	dcr, err := decodeDcReq(c, r)
	if err != nil {
		return nil, err
	}

	req.HideInitialConditions, _ = strconv.ParseBool(r.URL.Query().Get("hideInitialConditions"))
	req.ClusterID = clusterID
	req.DCReq = dcr.(DCReq)

	return req, nil
}

// CreateNodeReq defines HTTP request for createNodeForCluster
// swagger:parameters createNodeForCluster
type CreateNodeReq struct {
	GetClusterReq
	// in: body
	Body apiv1.Node
}

func decodeCreateNodeForCluster(c context.Context, r *http.Request) (interface{}, error) {
	var req CreateNodeReq

	clusterID, err := decodeClusterID(c, r)
	if err != nil {
		return nil, err
	}
	dcr, err := decodeDcReq(c, r)
	if err != nil {
		return nil, err
	}

	req.ClusterID = clusterID
	req.DCReq = dcr.(DCReq)

	if err = json.NewDecoder(r.Body).Decode(&req.Body); err != nil {
		return nil, err
	}

	return req, nil
}

// NodeReq defines HTTP request for getNodeForCluster
// swagger:parameters getNodeForCluster
type NodeReq struct {
	GetClusterReq
	// in: path
	NodeID string `json:"node_id"`
	// in: query
	HideInitialConditions bool `json:"hideInitialConditions"`
}

func decodeGetNodeForCluster(c context.Context, r *http.Request) (interface{}, error) {
	var req NodeReq

	clusterID, err := decodeClusterID(c, r)
	if err != nil {
		return nil, err
	}
	nodeID := mux.Vars(r)["node_id"]
	if nodeID == "" {
		return nil, fmt.Errorf("'node_id' parameter is required but was not provided")
	}

	dcr, err := decodeDcReq(c, r)
	if err != nil {
		return nil, err
	}

	req.ClusterID = clusterID
	req.NodeID = nodeID
	req.DCReq = dcr.(DCReq)

	return req, nil
}