package main // import "github.com/jmccarty3/kube2consul"

import (
	"github.com/golang/glog"
	//"k8s.io/kubernetes/pkg/api"
	kapi "k8s.io/kubernetes/pkg/api"
	kclient "k8s.io/kubernetes/pkg/client/unversioned"
	kfields "k8s.io/kubernetes/pkg/fields"
	klabels "k8s.io/kubernetes/pkg/labels"
)

//KubeNode Represents a node in the system
//TODO: Chang to store Node pointer. Add getName, getReadyStatus accessors
type KubeNode struct {
	name        string
	readyStatus bool
	serviceIDS  map[string]string
	address     string
}

//KubeBookKeeper Interface for reacting to changes in kubernetes
type KubeBookKeeper interface {
	AddNode(*kapi.Node)
	RemoveNode(*kapi.Node)
	UpdateNode(*kapi.Node)
	SyncNodes()
	AddService(*kapi.Service)
	RemoveService(*kapi.Service)
	UpdateService(*kapi.Service)
}

//ClientBookKeeper Bookkeeper that uses a connection the api server
type ClientBookKeeper struct {
	client      *kclient.Client
	nodes       map[string]*KubeNode
	services    map[string]*kapi.Service
	consulQueue chan<- ConsulWork
}

//BuildServiceBaseID Creates a base id to be used for Consul based on the Node name and the Service name
func BuildServiceBaseID(nodeName string, service *kapi.Service) string {
	return nodeName + "-" + service.Name
}

func newKubeNode() *KubeNode {
	return &KubeNode{
		name:        "",
		readyStatus: false,
		serviceIDS:  make(map[string]string),
	}
}

//NewClientBookKeeper Creates a new client based Bookkeeper
func NewClientBookKeeper(client *kclient.Client) *ClientBookKeeper {
	return &ClientBookKeeper{
		client:   client,
		nodes:    make(map[string]*KubeNode),
		services: make(map[string]*kapi.Service),
	}
}

//RunBookKeeper Runs the Bookkeeper as long as the work queue is open
func RunBookKeeper(workQue <-chan KubeWork, consulQueue chan<- ConsulWork, apiClient *kclient.Client) {

	client := NewClientBookKeeper(apiClient)
	client.consulQueue = consulQueue

	for work := range workQue {
		switch work.Action {
		case KubeWorkAddNode:
			client.AddNode(work.Node)
		case KubeWorkRemoveNode:
			client.RemoveNode(work.Node.Name)
		case KubeWorkAddService:
			client.AddService(work.Service)
		case KubeWorkRemoveService:
			client.RemoveService(work.Service)
		case KubeWorkUpdateService:
			client.UpdateService(work.Service)
		case KubeWorkSync:
			client.Sync()
		default:
			glog.Info("Unsupported work action: ", work.Action)
		}
	}

	glog.Info("Completed all node work")
}

func (client *ClientBookKeeper) attachServiceToNode(node *KubeNode, service *kapi.Service) {
	baseID := BuildServiceBaseID(node.name, service)
	client.consulQueue <- ConsulWork{
		Action:  ConsulWorkAddDNS,
		Service: service,
		Config: DNSInfo{
			BaseID:    baseID,
			IPAddress: node.address,
		},
	}
	glog.V(3).Info("Requesting Addition of services with Base ID: ", baseID)
	node.serviceIDS[service.Name] = baseID
}

func (client *ClientBookKeeper) detachServiceFromNode(node *KubeNode, service *kapi.Service) {
	if baseID, ok := node.serviceIDS[service.Name]; ok {
		//To Consol -> TODO
		client.consulQueue <- ConsulWork{
			Action: ConsulWorkRemoveDNS,
			Config: DNSInfo{
				BaseID: baseID,
			},
		}

		glog.V(3).Info("Requesting Removal of services with Base ID: ", baseID)
		delete(node.serviceIDS, service.Name)
	}
}

func (client *ClientBookKeeper) addAllServicesToNode(node *KubeNode) {
	for _, service := range client.services {
		client.attachServiceToNode(node, service)
	}
}

func (client *ClientBookKeeper) removeAllServicesFromNode(node *KubeNode) {
	for _, service := range client.services {
		client.detachServiceFromNode(node, service)
	}
}

//AddNode Adds a new node to the Bookkeeper
func (client *ClientBookKeeper) AddNode(newNode *kapi.Node) {
	if _, ok := client.nodes[newNode.Name]; ok {
		glog.Error("Attempted to Add existing node ", newNode.Name)
		return
	}

	//Add to Node Collection
	createdNode := newKubeNode()
	createdNode.readyStatus = nodeReady(newNode)
	createdNode.name = newNode.Name
	createdNode.address = newNode.Status.Addresses[0].Address

	//Send request for Service Addition for node and all serviceIDS (Create Service ID here)
	if createdNode.readyStatus {
		client.addAllServicesToNode(createdNode)
	}
	client.nodes[newNode.Name] = createdNode
	glog.Info("Added Node: ", newNode.Name)
}

//RemoveNode Removes the node from the Bookkeeper
func (client *ClientBookKeeper) RemoveNode(oldNodeName string) {
	if node, ok := client.nodes[oldNodeName]; ok {
		//Remove All DNS for node
		client.removeAllServicesFromNode(node)
		//Remove Node from Collection
		delete(client.nodes, oldNodeName)
	} else {
		glog.Error("Attmepted to remove missing node: ", oldNodeName)
	}

}

//UpdateNode Updates the status for the node.
func (client *ClientBookKeeper) UpdateNode(updatedNode *kapi.Node) {
	//If now ready -> Service Addtion for node
	//TODO Check it exists
	if nodeReady(updatedNode) {
		client.addAllServicesToNode(client.nodes[updatedNode.Name])
	} else {
		client.removeAllServicesFromNode(client.nodes[updatedNode.Name])
	}
	//Else -> Service Removal for Node
	//UnLock
}

//ContainsNodeName determines if a Node exists in the list with the requested name
func ContainsNodeName(name string, nodes *kapi.NodeList) bool {
	for _, node := range nodes.Items {
		if node.ObjectMeta.Name == name {
			return true
		}
	}
	return false
}

//Sync Performs a full syncroniztion of Nodes.
func (client *ClientBookKeeper) Sync() {
	nodes := client.client.Nodes()
	if nodeList, err := nodes.List(klabels.Everything(), kfields.Everything()); err == nil {
		for name := range client.nodes {
			if !ContainsNodeName(name, nodeList) {
				glog.Errorf("Bookkeeper has node: %s that does not exist in api server", name)
				client.RemoveNode(name)
			}
		}
	}
	//Add Remove as needed
	//UnLock
}

//AddService Adds a service to the Bookkeeper
func (client *ClientBookKeeper) AddService(service *kapi.Service) {
	//TODO Verify it doesn't exist
	client.services[service.Name] = service
	//Perform All DNS Adds
	for _, node := range client.nodes {
		client.attachServiceToNode(node, service)
	}

	glog.Info("Added Service: ", service.Name)
}

//RemoveService Removes the service from the Bookkeeper
func (client *ClientBookKeeper) RemoveService(service *kapi.Service) {
	//TODO Verify it does exist
	//Perform All DNS Removes
	for _, node := range client.nodes {
		client.detachServiceFromNode(node, service)
	}

	delete(client.services, service.Name)
	glog.Info("Removed Service: ", service.Name)
}

//UpdateService Removes a service and Re-Adds the service
func (client *ClientBookKeeper) UpdateService(service *kapi.Service) {
	client.RemoveService(service)
	client.AddService(service)
}
