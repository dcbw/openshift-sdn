package osdn

import (
	"fmt"
	"net"
	"strings"
	"sync"
	"time"

	log "github.com/golang/glog"

	"github.com/openshift/openshift-sdn/pkg/netutils"
	"github.com/openshift/openshift-sdn/plugins/osdn/api"

	osclient "github.com/openshift/origin/pkg/client"
	osapi "github.com/openshift/origin/pkg/sdn/api"

	kapi "k8s.io/kubernetes/pkg/api"
	kclient "k8s.io/kubernetes/pkg/client/unversioned"
	kubeletTypes "k8s.io/kubernetes/pkg/kubelet/container"
	utildbus "k8s.io/kubernetes/pkg/util/dbus"
	kerrors "k8s.io/kubernetes/pkg/util/errors"
	kexec "k8s.io/kubernetes/pkg/util/exec"
	"k8s.io/kubernetes/pkg/util/iptables"
	kubeutilnet "k8s.io/kubernetes/pkg/util/net"
)

type OsdnController struct {
	multitenant     bool
	Registry        *Registry
	localIP         string
	localSubnet     *osapi.HostSubnet
	HostName        string
	subnetAllocator *netutils.SubnetAllocator
	podNetworkReady chan struct{}
	vnidMap         map[string]uint
	vnidLock        sync.Mutex
	netIDManager    *netutils.NetIDAllocator
	adminNamespaces []string
}

// Called by higher layers to create the plugin SDN master instance
func NewMasterPlugin(pluginType string, osClient *osclient.Client, kClient *kclient.Client) (api.OsdnPlugin, error) {
	if !isOsdnPlugin(pluginType) {
		return nil, nil
	}
	return createPlugin(osClient, kClient, pluginType, "", "")
}

// Called by higher layers to create the plugin SDN node instance
func NewNodePlugin(pluginType string, osClient *osclient.Client, kClient *kclient.Client, hostname string, selfIP string) (api.OsdnPlugin, error) {
	if !isOsdnPlugin(pluginType) {
		return nil, nil
	}
	return createPlugin(osClient, kClient, pluginType, hostname, selfIP)
}

func createPlugin(osClient *osclient.Client, kClient *kclient.Client, pluginType string, hostname string, selfIP string) (api.OsdnPlugin, error) {
	log.Infof("Starting with configured hostname '%s' (IP '%s')", hostname, selfIP)

	if hostname == "" {
		output, err := kexec.New().Command("uname", "-n").CombinedOutput()
		if err != nil {
			return nil, err
		}
		hostname = strings.TrimSpace(string(output))
	}

	if selfIP == "" {
		var err error
		selfIP, err = netutils.GetNodeIP(hostname)
		if err != nil {
			log.V(5).Infof("Failed to determine node address from hostname %s; using default interface (%v)", hostname, err)
			defaultIP, err := kubeutilnet.ChooseHostInterface()
			if err != nil {
				return nil, err
			}
			selfIP = defaultIP.String()
		}
	}

	plugin := &OsdnController{
		multitenant:     isMultitenantPlugin(pluginType),
		Registry:        NewRegistry(osClient, kClient),
		localIP:         selfIP,
		HostName:        hostname,
		vnidMap:         make(map[string]uint),
		podNetworkReady: make(chan struct{}),
		adminNamespaces: make([]string, 0),
	}
	if plugin.multitenant {
		log.Infof("Initializing multi-tenant plugin for %s (%s)", hostname, selfIP)
	} else {
		log.Infof("Initializing single-tenant plugin for %s (%s)", hostname, selfIP)
	}
	return plugin, nil
}

func isOsdnPlugin(pluginType string) bool {
	switch strings.ToLower(pluginType) {
	case api.SingleTenantPluginName, api.MultiTenantPluginName:
		return true
	default:
		return false
	}
}

func isMultitenantPlugin(pluginType string) bool {
	switch strings.ToLower(pluginType) {
	case api.MultiTenantPluginName:
		return true
	default:
		return false
	}
}

func (oc *OsdnController) validateNetworkConfig(clusterNetwork, serviceNetwork *net.IPNet) error {
	// TODO: Instead of hardcoding 'tun0' and 'lbr0', get it from common place.
	// This will ensure both the kube/multitenant scripts and master validations use the same name.
	hostIPNets, err := netutils.GetHostIPNetworks([]string{"tun0", "lbr0"})
	if err != nil {
		return err
	}

	errList := []error{}

	// Ensure cluster and service network don't overlap with host networks
	for _, ipNet := range hostIPNets {
		if ipNet.Contains(clusterNetwork.IP) {
			errList = append(errList, fmt.Errorf("Error: Cluster IP: %s conflicts with host network: %s", clusterNetwork.IP.String(), ipNet.String()))
		}
		if clusterNetwork.Contains(ipNet.IP) {
			errList = append(errList, fmt.Errorf("Error: Host network with IP: %s conflicts with cluster network: %s", ipNet.IP.String(), clusterNetwork.String()))
		}
		if ipNet.Contains(serviceNetwork.IP) {
			errList = append(errList, fmt.Errorf("Error: Service IP: %s conflicts with host network: %s", serviceNetwork.String(), ipNet.String()))
		}
		if serviceNetwork.Contains(ipNet.IP) {
			errList = append(errList, fmt.Errorf("Error: Host network with IP: %s conflicts with service network: %s", ipNet.IP.String(), serviceNetwork.String()))
		}
	}

	// Ensure each host subnet is within the cluster network
	subnets, err := oc.Registry.GetSubnets()
	if err != nil {
		return fmt.Errorf("Error in initializing/fetching subnets: %v", err)
	}
	for _, sub := range subnets {
		subnetIP, _, err := net.ParseCIDR(sub.Subnet)
		if err != nil {
			errList = append(errList, fmt.Errorf("Failed to parse network address: %s", sub.Subnet))
			continue
		}
		if !clusterNetwork.Contains(subnetIP) {
			errList = append(errList, fmt.Errorf("Error: Existing node subnet: %s is not part of cluster network: %s", sub.Subnet, clusterNetwork.String()))
		}
	}

	// Ensure each service is within the services network
	services, err := oc.Registry.GetServices()
	if err != nil {
		return err
	}
	for _, svc := range services {
		if !serviceNetwork.Contains(net.ParseIP(svc.Spec.ClusterIP)) {
			errList = append(errList, fmt.Errorf("Error: Existing service with IP: %s is not part of service network: %s", svc.Spec.ClusterIP, serviceNetwork.String()))
		}
	}

	return kerrors.NewAggregate(errList)
}

func (oc *OsdnController) isClusterNetworkChanged(clusterNetworkCIDR string, hostBitsPerSubnet int, serviceNetworkCIDR string) (bool, error) {
	clusterNetwork, hostSubnetLength, serviceNetwork, err := oc.Registry.GetNetworkInfo()
	if err != nil {
		return false, err
	}
	if clusterNetworkCIDR != clusterNetwork.String() ||
		hostSubnetLength != hostBitsPerSubnet ||
		serviceNetworkCIDR != serviceNetwork.String() {
		return true, nil
	}
	return false, nil
}

func (oc *OsdnController) StartMaster(clusterNetworkCIDR string, clusterBitsPerSubnet uint, serviceNetworkCIDR string) error {
	// Validate command-line/config parameters
	hostBitsPerSubnet := int(clusterBitsPerSubnet)
	clusterNetwork, _, serviceNetwork, err := ValidateClusterNetwork(clusterNetworkCIDR, hostBitsPerSubnet, serviceNetworkCIDR)
	if err != nil {
		return err
	}

	changed, net_err := oc.isClusterNetworkChanged(clusterNetworkCIDR, hostBitsPerSubnet, serviceNetworkCIDR)
	if changed {
		if err := oc.validateNetworkConfig(clusterNetwork, serviceNetwork); err != nil {
			return err
		}
		if err := oc.Registry.UpdateClusterNetwork(clusterNetwork, hostBitsPerSubnet, serviceNetwork); err != nil {
			return err
		}
	} else if net_err != nil {
		if err := oc.Registry.CreateClusterNetwork(clusterNetwork, hostBitsPerSubnet, serviceNetwork); err != nil {
			return err
		}
	}

	if err := oc.SubnetStartMaster(clusterNetwork, clusterBitsPerSubnet); err != nil {
		return err
	}

	if oc.multitenant {
		if err := oc.VnidStartMaster(); err != nil {
			return err
		}
	}

	return nil
}

func (oc *OsdnController) StartNode(mtu uint) error {
	// Assume we are working with IPv4
	clusterNetwork, err := oc.Registry.GetClusterNetwork()
	if err != nil {
		log.Errorf("Failed to obtain ClusterNetwork: %v", err)
		return err
	}

	ipt := iptables.New(kexec.New(), utildbus.New(), iptables.ProtocolIpv4)
	if err := SetupIptables(ipt, clusterNetwork.String()); err != nil {
		return fmt.Errorf("Failed to set up iptables: %v", err)
	}

	ipt.AddReloadFunc(func() {
		err := SetupIptables(ipt, clusterNetwork.String())
		if err != nil {
			log.Errorf("Error reloading iptables: %v\n", err)
		}
	})

	networkChanged, err := oc.SubnetStartNode(mtu)
	if err != nil {
		return err
	}

	if oc.multitenant {
		if err := oc.VnidStartNode(); err != nil {
			return err
		}
	}

	if networkChanged {
		pods, err := oc.GetLocalPods(kapi.NamespaceAll)
		if err != nil {
			return err
		}
		for _, p := range pods {
			containerID := GetPodContainerID(&p)
			err = oc.UpdatePod(p.Namespace, p.Name, kubeletTypes.DockerID(containerID))
			if err != nil {
				log.Warningf("Could not update pod %q (%s): %s", p.Name, containerID, err)
			}
		}
	}

	oc.markPodNetworkReady()

	return nil
}

func (oc *OsdnController) GetLocalPods(namespace string) ([]kapi.Pod, error) {
	return oc.Registry.GetRunningPods(oc.HostName, namespace)
}

func (oc *OsdnController) markPodNetworkReady() {
	close(oc.podNetworkReady)
}

func (oc *OsdnController) WaitForPodNetworkReady() error {
	logInterval := 10 * time.Second
	numIntervals := 12 // timeout: 2 mins

	for i := 0; i < numIntervals; i++ {
		select {
		// Wait for StartNode() to finish SDN setup
		case <-oc.podNetworkReady:
			return nil
		case <-time.After(logInterval):
			log.Infof("Waiting for SDN pod network to be ready...")
		}
	}
	return fmt.Errorf("SDN pod network is not ready(timeout: 2 mins)")
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

func GetNodeIP(node *kapi.Node) (string, error) {
	if len(node.Status.Addresses) > 0 && node.Status.Addresses[0].Address != "" {
		return node.Status.Addresses[0].Address, nil
	} else {
		return netutils.GetNodeIP(node.Name)
	}
}

func GetPodContainerID(pod *kapi.Pod) string {
	if len(pod.Status.ContainerStatuses) > 0 {
		// Extract only container ID, pod.Status.ContainerStatuses[0].ContainerID is of the format: docker://<containerID>
		if parts := strings.Split(pod.Status.ContainerStatuses[0].ContainerID, "://"); len(parts) > 1 {
			return parts[1]
		}
	}
	return ""
}

func HostSubnetToString(subnet *osapi.HostSubnet) string {
	return fmt.Sprintf("%s [host: '%s'] [ip: '%s'] [subnet: '%s']", subnet.Name, subnet.Host, subnet.HostIP, subnet.Subnet)
}
