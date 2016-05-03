package osdn

import (
	"fmt"
	"net"

	log "github.com/golang/glog"

	"github.com/openshift/openshift-sdn/pkg/netutils"

	osclient "github.com/openshift/origin/pkg/client"
	osconfigapi "github.com/openshift/origin/pkg/cmd/server/api"

	kclient "k8s.io/kubernetes/pkg/client/unversioned"
	kerrors "k8s.io/kubernetes/pkg/util/errors"
)

type OsdnMaster struct {
	registry        *Registry
	subnetAllocator *netutils.SubnetAllocator
	vnidMap         VNIDMap
	netIDManager    *netutils.NetIDAllocator
	adminNamespaces []string
}

func StartMaster(networkConfig osconfigapi.MasterNetworkConfig, osClient *osclient.Client, kClient *kclient.Client) error {
	if !isOsdnPlugin(networkConfig.NetworkPluginName) {
		return nil
	}

	log.Infof("Initializing SDN master of type %q", networkConfig.NetworkPluginName)
	master := &OsdnMaster{
		registry:        NewRegistry(osClient, kClient),
		vnidMap:         NewVNIDMap(),
		adminNamespaces: make([]string, 0),
	}

	// Validate command-line/config parameters
	clusterNetwork, hostBitsPerSubnet, serviceNetwork, err := ValidateClusterNetwork(networkConfig.ClusterNetworkCIDR, int(networkConfig.HostSubnetLength), networkConfig.ServiceNetworkCIDR)
	if err != nil {
		return err
	}

	changed, net_err := master.isClusterNetworkChanged(networkConfig.ClusterNetworkCIDR, hostBitsPerSubnet, networkConfig.ServiceNetworkCIDR)
	if changed {
		if err := master.validateNetworkConfig(clusterNetwork, serviceNetwork); err != nil {
			return err
		}
		if err := master.registry.UpdateClusterNetwork(clusterNetwork, hostBitsPerSubnet, serviceNetwork); err != nil {
			return err
		}
	} else if net_err != nil {
		if err := master.registry.CreateClusterNetwork(clusterNetwork, hostBitsPerSubnet, serviceNetwork); err != nil {
			return err
		}
	}

	if err := master.SubnetStartMaster(clusterNetwork, networkConfig.HostSubnetLength); err != nil {
		return err
	}

	if isMultitenantPlugin(networkConfig.NetworkPluginName) {
		if err := master.VnidStartMaster(); err != nil {
			return err
		}
	}

	return nil
}

func (oc *OsdnMaster) validateNetworkConfig(clusterNetwork, serviceNetwork *net.IPNet) error {
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
	subnets, err := oc.registry.GetSubnets()
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
	services, err := oc.registry.GetServices()
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

func (oc *OsdnMaster) isClusterNetworkChanged(clusterNetworkCIDR string, hostBitsPerSubnet int, serviceNetworkCIDR string) (bool, error) {
	clusterNetwork, hostSubnetLength, serviceNetwork, err := oc.registry.GetNetworkInfo()
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
