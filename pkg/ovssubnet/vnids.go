package ovssubnet

import (
	"fmt"

	log "github.com/golang/glog"

	"github.com/openshift/openshift-sdn/pkg/netutils"
	"github.com/openshift/openshift-sdn/pkg/ovssubnet/api"
)

const (
	// Maximum VXLAN Network Identifier as per RFC#7348
	MaxVNID = ((1 << 24) - 1)
	// VNID for the admin namespaces
	AdminVNID = uint(0)
)

func watchGetNamespaces(registry api.SubnetRegistry) (interface{}, string, error) {
	return registry.GetNamespaces()
}

func vnidStartMaster(oc *OvsController) error {
	nets, _, err := oc.subnetRegistry.GetNetNamespaces()
	if err != nil {
		return err
	}
	inUse := make([]uint, 0)
	for _, net := range nets {
		if net.NetID != AdminVNID {
			inUse = append(inUse, net.NetID)
		}
		oc.VNIDMap[net.Name] = net.NetID
	}
	// VNID: 0 reserved for default namespace and can reach any network in the cluster
	// VNID: 1 to 9 are internally reserved for any special cases in the future
	oc.netIDManager, err = netutils.NewNetIDAllocator(10, MaxVNID, inUse)
	if err != nil {
		return err
	}

	result, err := oc.watchAndGetResource("Namespace", watchNamespaces, watchGetNamespaces)
	if err != nil {
		return err
	}

	// Handle existing namespaces
	namespaces := result.([]string)
	for _, nsName := range namespaces {
		// Revoke invalid VNID for admin namespaces
		if oc.isAdminNamespace(nsName) {
			netid, ok := oc.VNIDMap[nsName]
			if ok && (netid != AdminVNID) {
				err := oc.revokeVNID(nsName)
				if err != nil {
					return err
				}
			}
		}
		_, found := oc.VNIDMap[nsName]
		// Assign VNID for the namespace if it doesn't exist
		if !found {
			err := oc.assignVNID(nsName)
			if err != nil {
				return err
			}
		}
	}

	return nil
}

func (oc *OvsController) isAdminNamespace(nsName string) bool {
	for _, name := range oc.AdminNamespaces {
		if name == nsName {
			return true
		}
	}
	return false
}

func (oc *OvsController) assignVNID(namespaceName string) error {
	_, err := oc.subnetRegistry.GetNetNamespace(namespaceName)
	if err == nil {
		return nil
	}
	var netid uint
	if oc.isAdminNamespace(namespaceName) {
		netid = AdminVNID
	} else {
		var err error
		netid, err = oc.netIDManager.GetNetID()
		if err != nil {
			return err
		}
	}
	err = oc.subnetRegistry.WriteNetNamespace(namespaceName, netid)
	if err != nil {
		e := oc.netIDManager.ReleaseNetID(netid)
		if e != nil {
			log.Error("Error while releasing Net ID: %v", e)
		}
		return err
	}
	oc.VNIDMap[namespaceName] = netid
	return nil
}

func (oc *OvsController) revokeVNID(namespaceName string) error {
	err := oc.subnetRegistry.DeleteNetNamespace(namespaceName)
	if err != nil {
		return err
	}
	netid, found := oc.VNIDMap[namespaceName]
	if !found {
		return fmt.Errorf("Error while fetching Net ID for namespace: %s", namespaceName)
	}
	delete(oc.VNIDMap, namespaceName)

	// Skip AdminVNID as it is not part of Net ID allocation
	if netid == AdminVNID {
		return nil
	}

	// Check if this netid is used by any other namespaces
	// If not, then release the netid
	netid_inuse := false
	for _, id := range oc.VNIDMap {
		if id == netid {
			netid_inuse = true
			break
		}
	}
	if !netid_inuse {
		err = oc.netIDManager.ReleaseNetID(netid)
		if err != nil {
			return fmt.Errorf("Error while releasing Net ID: %v", err)
		}
	}
	return nil
}

func watchNamespaces(oc *OvsController, ready chan<- bool, start <-chan string) {
	nsevent := make(chan *api.NamespaceEvent)
	stop := make(chan bool)
	go oc.subnetRegistry.WatchNamespaces(nsevent, ready, start, stop)
	for {
		select {
		case ev := <-nsevent:
			switch ev.Type {
			case api.Added:
				err := oc.assignVNID(ev.Name)
				if err != nil {
					log.Error("Error assigning Net ID: %v", err)
					continue
				}
			case api.Deleted:
				err := oc.revokeVNID(ev.Name)
				if err != nil {
					log.Error("Error revoking Net ID: %v", err)
					continue
				}
			}
		case <-oc.sig:
			log.Error("Signal received. Stopping watching of nodes.")
			stop <- true
			return
		}
	}
}

func vnidStartNode(oc *OvsController) error {
	result, err := oc.watchAndGetResource("NetNamespace", watchNetNamespaces, watchGetNetNamespaces)
	if err != nil {
		return err
	}
	nslist := result.([]api.NetNamespace)
	for _, ns := range nslist {
		oc.VNIDMap[ns.Name] = ns.NetID
	}

	result, err = oc.watchAndGetResource("Service", watchServices, watchGetServices)
	if err != nil {
		return err
	}
	services := result.([]api.Service)
	for _, svc := range services {
		netid, found := oc.VNIDMap[svc.Namespace]
		if !found {
			return fmt.Errorf("Error fetching Net ID for namespace: %s", svc.Namespace)
		}
		oc.flowController.AddServiceOFRules(netid, svc.IP, svc.Protocol, svc.Port)
	}

	return nil
}

func (oc *OvsController) updatePodNetwork(namespace string, netID, oldNetID uint) error {
	// Update OF rules for the existing/old pods in the namespace
	pods, err := oc.subnetRegistry.GetRunningPods(oc.hostName, namespace)
	if err != nil {
		return err
	}
	for _, pod := range pods {
		err := oc.flowController.UpdatePod(pod.Namespace, pod.Name, pod.ContainerID, netID)
		if err != nil {
			return err
		}
	}

	// Update OF rules for the old services in the namespace
	services, err := oc.subnetRegistry.GetServicesForNamespace(namespace)
	if err != nil {
		return err
	}
	for _, svc := range services {
		oc.flowController.DelServiceOFRules(oldNetID, svc.IP, svc.Protocol, svc.Port)
		oc.flowController.AddServiceOFRules(netID, svc.IP, svc.Protocol, svc.Port)
	}
	return nil
}

func watchGetNetNamespaces(registry api.SubnetRegistry) (interface{}, string, error) {
	return registry.GetNetNamespaces()
}

func watchNetNamespaces(oc *OvsController, ready chan<- bool, start <-chan string) {
	stop := make(chan bool)
	netNsEvent := make(chan *api.NetNamespaceEvent)
	go oc.subnetRegistry.WatchNetNamespaces(netNsEvent, ready, start, stop)
	for {
		select {
		case ev := <-netNsEvent:
			oldNetID, found := oc.VNIDMap[ev.Name]
			if !found {
				log.Error("Error fetching Net ID for namespace: %s, skipped netNsEvent: %v", ev.Name, ev)
			}
			switch ev.Type {
			case api.Added:
				// Skip this event if the old and new network ids are same
				if oldNetID == ev.NetID {
					continue
				}
				oc.VNIDMap[ev.Name] = ev.NetID
				err := oc.updatePodNetwork(ev.Name, ev.NetID, oldNetID)
				if err != nil {
					log.Error("Failed to update pod network for namespace '%s', error: %s", ev.Name, err)
				}
			case api.Deleted:
				err := oc.updatePodNetwork(ev.Name, AdminVNID, oldNetID)
				if err != nil {
					log.Error("Failed to update pod network for namespace '%s', error: %s", ev.Name, err)
				}
				delete(oc.VNIDMap, ev.Name)
			}
		case <-oc.sig:
			log.Error("Signal received. Stopping watching of NetNamespaces.")
			stop <- true
			return
		}
	}
}

func watchGetServices(registry api.SubnetRegistry) (interface{}, string, error) {
	return registry.GetServices()
}

func watchServices(oc *OvsController, ready chan<- bool, start <-chan string) {
	stop := make(chan bool)
	svcevent := make(chan *api.ServiceEvent)
	go oc.subnetRegistry.WatchServices(svcevent, ready, start, stop)
	for {
		select {
		case ev := <-svcevent:
			netid, found := oc.VNIDMap[ev.Service.Namespace]
			if !found {
				log.Error("Error fetching Net ID for namespace: %s, skipped serviceEvent: %v", ev.Service.Namespace, ev)
			}
			switch ev.Type {
			case api.Added:
				oc.flowController.AddServiceOFRules(netid, ev.Service.IP, ev.Service.Protocol, ev.Service.Port)
			case api.Deleted:
				oc.flowController.DelServiceOFRules(netid, ev.Service.IP, ev.Service.Protocol, ev.Service.Port)
			}
		case <-oc.sig:
			log.Error("Signal received. Stopping watching of services.")
			stop <- true
			return
		}
	}
}
