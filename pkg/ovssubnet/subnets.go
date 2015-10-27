package ovssubnet

import (
	"fmt"
	"net"
	"time"

	log "github.com/golang/glog"

	"github.com/openshift/openshift-sdn/pkg/netutils"
	"github.com/openshift/openshift-sdn/pkg/ovssubnet/api"
)

func watchGetNodes(registry api.SubnetRegistry) (interface{}, string, error) {
	return registry.GetNodes()
}

func subnetStartMaster(oc *OvsController) error {
	subrange := make([]string, 0)
	subnets, _, err := oc.subnetRegistry.GetSubnets()
	if err != nil {
		log.Errorf("Error in initializing/fetching subnets: %v", err)
		return err
	}
	for _, sub := range subnets {
		subrange = append(subrange, sub.SubnetCIDR)
	}

	cn, err := oc.subnetRegistry.GetClusterNetworkCIDR()
	if err != nil {
		log.Errorf("Error re-fetching cluster network CIDR: %v", err)
		return err
	}

	hsl, err := oc.subnetRegistry.GetHostSubnetLength()
	if err != nil {
		log.Errorf("Error re-fetching host subnet length: %v", err)
		return err
	}

	oc.subnetAllocator, err = netutils.NewSubnetAllocator(cn, uint(hsl), subrange)
	if err != nil {
		return err
	}

	result, err := oc.watchAndGetResource("Node", watchNodes, watchGetNodes)
	if err != nil {
		return err
	}
	nodes := result.([]api.Node)
	err = oc.serveExistingNodes(nodes)
	if err != nil {
		return err
	}

	return nil
}

func (oc *OvsController) serveExistingNodes(nodes []api.Node) error {
	for _, node := range nodes {
		_, err := oc.subnetRegistry.GetSubnet(node.Name)
		if err == nil {
			// subnet already exists, continue
			continue
		}
		err = oc.addNode(node.Name, node.IP)
		if err != nil {
			return err
		}
	}
	return nil
}

func (oc *OvsController) addNode(nodeName string, nodeIP string) error {
	sn, err := oc.subnetAllocator.GetNetwork()
	if err != nil {
		log.Errorf("Error creating network for node %s.", nodeName)
		return err
	}

	if nodeIP == "" || nodeIP == "127.0.0.1" {
		return fmt.Errorf("Invalid node IP")
	}

	subnet := &api.Subnet{
		NodeIP:     nodeIP,
		SubnetCIDR: sn.String(),
	}
	err = oc.subnetRegistry.CreateSubnet(nodeName, subnet)
	if err != nil {
		log.Errorf("Error writing subnet to etcd for node %s: %v", nodeName, sn)
		return err
	}
	return nil
}

func (oc *OvsController) deleteNode(nodeName string) error {
	sub, err := oc.subnetRegistry.GetSubnet(nodeName)
	if err != nil {
		log.Errorf("Error fetching subnet for node %s for delete operation.", nodeName)
		return err
	}
	_, ipnet, err := net.ParseCIDR(sub.SubnetCIDR)
	if err != nil {
		log.Errorf("Error parsing subnet for node %s for deletion: %s", nodeName, sub.SubnetCIDR)
		return err
	}
	oc.subnetAllocator.ReleaseNetwork(ipnet)
	return oc.subnetRegistry.DeleteSubnet(nodeName)
}

func watchGetSubnets(registry api.SubnetRegistry) (interface{}, string, error) {
	return registry.GetSubnets()
}

func subnetStartNode(oc *OvsController) error {
	err := oc.initSelfSubnet()
	if err != nil {
		log.Errorf("Failed to get subnet for this host: %v", err)
		return err
	}

	// Assume we are working with IPv4
	clusterNetworkCIDR, err := oc.subnetRegistry.GetClusterNetworkCIDR()
	if err != nil {
		log.Errorf("Failed to obtain ClusterNetwork: %v", err)
		return err
	}
	servicesNetworkCIDR, err := oc.subnetRegistry.GetServicesNetworkCIDR()
	if err != nil {
		log.Errorf("Failed to obtain ServicesNetwork: %v", err)
		return err
	}
	err = oc.flowController.Setup(oc.localSubnet.SubnetCIDR, clusterNetworkCIDR, servicesNetworkCIDR, oc.nodeMtu)
	if err != nil {
		return err
	}

	result, err := oc.watchAndGetResource("HostSubnet", watchSubnets, watchGetSubnets)
	if err != nil {
		return err
	}
	subnets := result.([]api.Subnet)
	for _, s := range subnets {
		oc.flowController.AddOFRules(s.NodeIP, s.SubnetCIDR, oc.localIP)
	}

	return nil
}

func (oc *OvsController) initSelfSubnet() error {
	// get subnet for self
	for {
		sub, err := oc.subnetRegistry.GetSubnet(oc.hostName)
		if err != nil {
			log.Errorf("Could not find an allocated subnet for node %s: %s. Waiting...", oc.hostName, err)
			time.Sleep(2 * time.Second)
			continue
		}
		oc.localSubnet = sub
		return nil
	}
}

func watchNodes(oc *OvsController, ready chan<- bool, start <-chan string) {
	stop := make(chan bool)
	nodeEvent := make(chan *api.NodeEvent)
	go oc.subnetRegistry.WatchNodes(nodeEvent, ready, start, stop)
	for {
		select {
		case ev := <-nodeEvent:
			switch ev.Type {
			case api.Added:
				sub, err := oc.subnetRegistry.GetSubnet(ev.Node.Name)
				if err != nil {
					// subnet does not exist already
					oc.addNode(ev.Node.Name, ev.Node.IP)
				} else {
					// Current node IP is obtained from event, ev.NodeIP to
					// avoid cached/stale IP lookup by net.LookupIP()
					if sub.NodeIP != ev.Node.IP {
						err = oc.subnetRegistry.DeleteSubnet(ev.Node.Name)
						if err != nil {
							log.Errorf("Error deleting subnet for node %s, old ip %s", ev.Node.Name, sub.NodeIP)
							continue
						}
						sub.NodeIP = ev.Node.IP
						err = oc.subnetRegistry.CreateSubnet(ev.Node.Name, sub)
						if err != nil {
							log.Errorf("Error creating subnet for node %s, ip %s", ev.Node.Name, sub.NodeIP)
							continue
						}
					}
				}
			case api.Deleted:
				oc.deleteNode(ev.Node.Name)
			}
		case <-oc.sig:
			log.Error("Signal received. Stopping watching of nodes.")
			stop <- true
			return
		}
	}
}

func watchSubnets(oc *OvsController, ready chan<- bool, start <-chan string) {
	stop := make(chan bool)
	clusterEvent := make(chan *api.SubnetEvent)
	go oc.subnetRegistry.WatchSubnets(clusterEvent, ready, start, stop)
	for {
		select {
		case ev := <-clusterEvent:
			switch ev.Type {
			case api.Added:
				// add openflow rules
				oc.flowController.AddOFRules(ev.Subnet.NodeIP, ev.Subnet.SubnetCIDR, oc.localIP)
			case api.Deleted:
				// delete openflow rules meant for the node
				oc.flowController.DelOFRules(ev.Subnet.NodeIP, oc.localIP)
			}
		case <-oc.sig:
			stop <- true
			return
		}
	}
}
