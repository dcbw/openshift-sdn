package ovssubnet

import (
	"fmt"
	"net"
	"strings"
	"time"

	log "github.com/golang/glog"

	"github.com/openshift/openshift-sdn/pkg/netutils"
	"github.com/openshift/openshift-sdn/pkg/ovssubnet/api"
	"github.com/openshift/openshift-sdn/pkg/ovssubnet/controller/kube"
	"github.com/openshift/openshift-sdn/pkg/ovssubnet/controller/multitenant"

	utildbus "k8s.io/kubernetes/pkg/util/dbus"
	kerrors "k8s.io/kubernetes/pkg/util/errors"
	kexec "k8s.io/kubernetes/pkg/util/exec"
	"k8s.io/kubernetes/pkg/util/iptables"
)

type startFunc func(oc *OvsController) error

type OvsController struct {
	subnetRegistry  api.SubnetRegistry
	localIP         string
	localSubnet     *api.Subnet
	hostName        string
	subnetAllocator *netutils.SubnetAllocator
	sig             chan struct{}
	ready           chan struct{}
	flowController  FlowController
	VNIDMap         map[string]uint
	netIDManager    *netutils.NetIDAllocator
	AdminNamespaces []string
	nodeMtu         uint

	startMasterFuncs []startFunc
	startNodeFuncs   []startFunc
}

type FlowController interface {
	Setup(localSubnetCIDR, clusterNetworkCIDR, serviceNetworkCIDR string, mtu uint) error

	AddOFRules(nodeIP, nodeSubnetCIDR, localIP string) error
	DelOFRules(nodeIP, localIP string) error

	AddServiceOFRules(netID uint, IP string, protocol api.ServiceProtocol, port uint) error
	DelServiceOFRules(netID uint, IP string, protocol api.ServiceProtocol, port uint) error

	UpdatePod(namespace, podName, containerID string, netID uint) error
}

func NewKubeController(sub api.SubnetRegistry, hostname string, selfIP string, ready chan struct{}) (*OvsController, error) {
	kubeController, err := NewController(sub, hostname, selfIP, ready)
	if err == nil {
		kubeController.flowController = kube.NewFlowController()
		kubeController.startMasterFuncs = append(kubeController.startMasterFuncs, subnetStartMaster)
		kubeController.startNodeFuncs = append(kubeController.startNodeFuncs, subnetStartNode)
	}
	return kubeController, err
}

func NewMultitenantController(sub api.SubnetRegistry, hostname string, selfIP string, ready chan struct{}) (*OvsController, error) {
	mtController, err := NewController(sub, hostname, selfIP, ready)
	if err == nil {
		mtController.flowController = multitenant.NewFlowController()
		mtController.startMasterFuncs = append(mtController.startMasterFuncs, subnetStartMaster)
		mtController.startMasterFuncs = append(mtController.startMasterFuncs, vnidStartMaster)
		mtController.startNodeFuncs = append(mtController.startNodeFuncs, subnetStartNode)
		mtController.startNodeFuncs = append(mtController.startNodeFuncs, vnidStartNode)
	}
	return mtController, err
}

func NewController(sub api.SubnetRegistry, hostname string, selfIP string, ready chan struct{}) (*OvsController, error) {
	if hostname == "" {
		output, err := kexec.New().Command("uname", "-n").CombinedOutput()
		if err != nil {
			log.Fatalf("SDN initialization failed: %v", err)
		}
		hostname = strings.TrimSpace(string(output))
	}

	if selfIP == "" {
		var err error
		selfIP, err = GetNodeIP(hostname)
		if err != nil {
			return nil, err
		}
	}
	log.Infof("Self IP: %s.", selfIP)
	return &OvsController{
		subnetRegistry:   sub,
		localIP:          selfIP,
		hostName:         hostname,
		localSubnet:      nil,
		subnetAllocator:  nil,
		VNIDMap:          make(map[string]uint),
		sig:              make(chan struct{}),
		ready:            ready,
		AdminNamespaces:  make([]string, 0),
		startMasterFuncs: make([]startFunc, 0),
		startNodeFuncs:   make([]startFunc, 0),
	}, nil
}

func (oc *OvsController) validateClusterNetwork(networkCIDR string, subnetsInUse []string) error {
	_, clusterIPNet, err := net.ParseCIDR(networkCIDR)
	if err != nil {
		return fmt.Errorf("Failed to parse network address: %s", networkCIDR)
	}

	errList := []error{}
	for _, netStr := range subnetsInUse {
		subnetIP, _, err := net.ParseCIDR(netStr)
		if err != nil {
			errList = append(errList, fmt.Errorf("Failed to parse network address: %s", netStr))
			continue
		}
		if !clusterIPNet.Contains(subnetIP) {
			errList = append(errList, fmt.Errorf("Error: Existing node subnet: %s is not part of cluster network: %s", netStr, networkCIDR))
		}
	}
	return kerrors.NewAggregate(errList)
}

func (oc *OvsController) validateServiceNetwork(networkCIDR string) error {
	_, serviceIPNet, err := net.ParseCIDR(networkCIDR)
	if err != nil {
		return fmt.Errorf("Failed to parse network address: %s", networkCIDR)
	}

	services, _, err := oc.subnetRegistry.GetServices()
	if err != nil {
		return err
	}
	errList := []error{}
	for _, svc := range services {
		if !serviceIPNet.Contains(net.ParseIP(svc.IP)) {
			errList = append(errList, fmt.Errorf("Error: Existing service with IP: %s is not part of service network: %s", svc.IP, networkCIDR))
		}
	}
	return kerrors.NewAggregate(errList)
}

func (oc *OvsController) validateNetworkConfig(clusterNetworkCIDR, serviceNetworkCIDR string, subnetsInUse []string) error {
	errList := []error{}
	if err := oc.validateClusterNetwork(clusterNetworkCIDR, subnetsInUse); err != nil {
		errList = append(errList, err)
	}
	if err := oc.validateServiceNetwork(serviceNetworkCIDR); err != nil {
		errList = append(errList, err)
	}
	return kerrors.NewAggregate(errList)
}

func (oc *OvsController) StartMaster(clusterNetworkCIDR string, clusterBitsPerSubnet uint, serviceNetworkCIDR string) error {
	subrange := make([]string, 0)
	subnets, _, err := oc.subnetRegistry.GetSubnets()
	if err != nil {
		log.Errorf("Error in initializing/fetching subnets: %v", err)
		return err
	}
	for _, sub := range subnets {
		subrange = append(subrange, sub.SubnetCIDR)
	}

	// Any mismatch in cluster/service network is handled by WriteNetworkConfig
	// For any new cluster/service network, ensure existing node subnets belong
	// to the given cluster network and service IPs belong to the given service network
	if _, err = oc.subnetRegistry.GetClusterNetworkCIDR(); err != nil {
		err = oc.validateNetworkConfig(clusterNetworkCIDR, serviceNetworkCIDR, subrange)
		if err != nil {
			return err
		}
	}

	err = oc.subnetRegistry.WriteNetworkConfig(clusterNetworkCIDR, clusterBitsPerSubnet, serviceNetworkCIDR)
	if err != nil {
		return err
	}

	// Plugin specific startup
	for _, f := range oc.startMasterFuncs {
		err = f(oc)
		if err != nil {
			return err
		}
	}

	return nil
}

func (oc *OvsController) StartNode(mtu uint) error {
	oc.nodeMtu = mtu

	// Assume we are working with IPv4
	clusterNetworkCIDR, err := oc.subnetRegistry.GetClusterNetworkCIDR()
	if err != nil {
		log.Errorf("Failed to obtain ClusterNetwork: %v", err)
		return err
	}

	ipt := iptables.New(kexec.New(), utildbus.New(), iptables.ProtocolIpv4)
	err = SetupIptables(ipt, clusterNetworkCIDR)
	if err != nil {
		return err
	}

	ipt.AddReloadFunc(func() {
		err := SetupIptables(ipt, clusterNetworkCIDR)
		if err != nil {
			log.Errorf("Error reloading iptables: %v\n", err)
		}
	})

	// Plugin specific startup
	for _, f := range oc.startNodeFuncs {
		err = f(oc)
		if err != nil {
			return err
		}
	}

	if oc.ready != nil {
		close(oc.ready)
	}
	return nil
}

func (oc *OvsController) Stop() {
	close(oc.sig)
}

func GetNodeIP(nodeName string) (string, error) {
	ip := net.ParseIP(nodeName)
	if ip == nil {
		addrs, err := net.LookupIP(nodeName)
		if err != nil {
			log.Errorf("Failed to lookup IP address for node %s: %v", nodeName, err)
			return "", err
		}
		for _, addr := range addrs {
			if addr.String() != "127.0.0.1" {
				ip = addr
				break
			}
		}
	}
	if ip == nil || len(ip.String()) == 0 {
		return "", fmt.Errorf("Failed to obtain IP address from node name: %s", nodeName)
	}
	return ip.String(), nil
}

// Wait for ready signal from Watch interface for the given resource
// Closes the ready channel as we don't need it anymore after this point
func waitForWatchReadiness(ready chan bool, resourceName string) {
	timeout := time.Minute
	select {
	case <-ready:
		close(ready)
	case <-time.After(timeout):
		log.Fatalf("Watch for resource %s is not ready(timeout: %v)", resourceName, timeout)
	}
	return
}

type watchWatcher func(oc *OvsController, ready chan<- bool, start <-chan string)
type watchGetter  func(registry api.SubnetRegistry) (interface{}, string, error)

// watchAndGetResource will fetch current items in etcd and watch for any new
// changes for the given resource.
// Supported resources: nodes, subnets, namespaces, services, netnamespaces, and pods.
//
// To avoid any potential race conditions during this process, these steps are followed:
// 1. Initiator(master/node): Watch for a resource as an async op, lets say WatchProcess
// 2. WatchProcess: When ready for watching, send ready signal to initiator
// 3. Initiator: Wait for watch resource to be ready
//    This is needed as step-1 is an asynchronous operation
// 4. WatchProcess: Collect new changes in the queue but wait for initiator
//    to indicate which version to start from
// 5. Initiator: Get existing items with their latest version for the resource
// 6. Initiator: Send version from step-5 to WatchProcess
// 7. WatchProcess: Ignore any items with version <= start version got from initiator on step-6
// 8. WatchProcess: Handle new changes
func (oc *OvsController) watchAndGetResource(resourceName string, watcher watchWatcher, getter watchGetter) (interface{}, error) {
	ready := make(chan bool)
	start := make(chan string)

	go watcher(oc, ready, start)
	waitForWatchReadiness(ready, strings.ToLower(resourceName))
	getOutput, version, err := getter(oc.subnetRegistry)
	if err != nil {
		return nil, err
	}

	start <- version

	return getOutput, nil
}

type FirewallRule struct {
	table string
	chain string
	args  []string
}

func SetupIptables(ipt iptables.Interface, clusterNetworkCIDR string) error {
	rules := []FirewallRule{
		{"nat", "POSTROUTING", []string{"-s", clusterNetworkCIDR, "!", "-d", clusterNetworkCIDR, "-j", "MASQUERADE"}},
		{"filter", "INPUT", []string{"-p", "udp", "-m", "multiport", "--dports", "4789", "-m", "comment", "--comment", "001 vxlan incoming", "-j", "ACCEPT"}},
		{"filter", "INPUT", []string{"-i", "tun0", "-m", "comment", "--comment", "traffic from docker for internet", "-j", "ACCEPT"}},
		{"filter", "FORWARD", []string{"-d", clusterNetworkCIDR, "-j", "ACCEPT"}},
		{"filter", "FORWARD", []string{"-s", clusterNetworkCIDR, "-j", "ACCEPT"}},
	}

	for _, rule := range rules {
		_, err := ipt.EnsureRule(iptables.Prepend, iptables.Table(rule.table), iptables.Chain(rule.chain), rule.args...)
		if err != nil {
			return err
		}
	}

	return nil
}
