package osdn

import (
	"fmt"
	"path/filepath"
	"strings"
	"time"

	log "github.com/golang/glog"

	"github.com/openshift/openshift-sdn/pkg/netutils"

	osclient "github.com/openshift/origin/pkg/client"

	kapi "k8s.io/kubernetes/pkg/api"
	kclient "k8s.io/kubernetes/pkg/client/unversioned"
	kubeletTypes "k8s.io/kubernetes/pkg/kubelet/container"
	knetwork "k8s.io/kubernetes/pkg/kubelet/network"
	kexec "k8s.io/kubernetes/pkg/util/exec"
	kubeutilnet "k8s.io/kubernetes/pkg/util/net"

	cniinvoke "github.com/appc/cni/pkg/invoke"
)

type OsdnNode struct {
	multitenant        bool
	registry           *Registry
	podinfoServer      *PodInfoServer
	localIP            string
	hostName           string
	podNetworkReady    chan struct{}
	vnids              *vnidMap
	iptablesSyncPeriod time.Duration
	host               knetwork.Host
	cniConfig          []byte
	mtu                uint
}

// Called by higher layers to create the plugin SDN node instance
func NewNodePlugin(pluginName string, osClient *osclient.Client, kClient *kclient.Client, hostname string, selfIP string, iptablesSyncPeriod time.Duration, mtu uint) (*OsdnNode, error) {
	if !IsOpenShiftNetworkPlugin(pluginName) {
		return nil, nil
	}

	log.Infof("Initializing SDN node of type %q with configured hostname %q (IP %q), iptables sync period %q", pluginName, hostname, selfIP, iptablesSyncPeriod.String())
	if hostname == "" {
		output, err := kexec.New().Command("uname", "-n").CombinedOutput()
		if err != nil {
			return nil, err
		}
		hostname = strings.TrimSpace(string(output))
		log.Infof("Resolved hostname to %q", hostname)
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
			log.Infof("Resolved IP address to %q", selfIP)
		}
	}

	plugin := &OsdnNode{
		multitenant:        IsOpenShiftMultitenantNetworkPlugin(pluginName),
		registry:           newRegistry(osClient, kClient),
		localIP:            selfIP,
		hostName:           hostname,
		podNetworkReady:    make(chan struct{}),
		iptablesSyncPeriod: iptablesSyncPeriod,
		mtu:                mtu,
	}
	if plugin.multitenant {
		plugin.vnids = newVnidMap()
	}

	plugin.podinfoServer = NewPodInfoServer(plugin.registry, plugin.vnids, plugin.hostName)

	return plugin, nil
}

func (node *OsdnNode) Start() error {
	ni, err := node.registry.GetNetworkInfo()
	if err != nil {
		return fmt.Errorf("Failed to get network information: %v", err)
	}

	nodeIPTables := newNodeIPTables(ni.ClusterNetwork.String(), node.iptablesSyncPeriod)
	if err := nodeIPTables.Setup(); err != nil {
		return fmt.Errorf("Failed to set up iptables: %v", err)
	}

	networkChanged, localSubnet, err := node.SubnetStartNode(node.mtu)
	if err != nil {
		return err
	}

	if node.multitenant {
		if err := node.VnidStartNode(); err != nil {
			return err
		}
	}

	if networkChanged {
		pods, err := node.GetLocalPods(kapi.NamespaceAll)
		if err != nil {
			return err
		}
		for _, p := range pods {
			containerID := getPodContainerID(&p)
			err = node.UpdatePod(p.Namespace, p.Name, kubeletTypes.ContainerID{ID: containerID})
			if err != nil {
				log.Warningf("Could not update pod %q (%s): %s", p.Name, containerID, err)
			}
		}
	}

	if err := node.podinfoServer.Start(ni.ClusterNetwork.String(), localSubnet.Subnet, node.mtu); err != nil {
		log.Errorf("Failed to start pod info server: %v", err)
		return err
	}

	node.markPodNetworkReady()

	return nil
}


func (plugin *OsdnNode) UpdatePod(namespace string, name string, id kubeletTypes.ContainerID) error {
	const (
		pluginPath = "/opt/cni/bin/openshift-sdn"
		netConf = `{
  "cniVersion": "0.1.0",
  "name": "openshift-sdn",
  "type": "openshift-sdn"
}`
	)

	args := &cniinvoke.Args{
		Command:     "UPDATE",
		ContainerID: id.String(),
		NetNS:       "/blahblah/foobar",  // plugin finds out namespace itself
		PluginArgs:  [][2]string{
			{"K8S_POD_NAMESPACE", namespace},
			{"K8S_POD_NAME", name},
			{"K8S_POD_INFRA_CONTAINER_ID", id.String()},
		},
		IfName:      "eth0",
		Path:        filepath.Dir(pluginPath),
	}

	if _, err := cniinvoke.ExecPluginWithResult(pluginPath, []byte(netConf), args); err != nil {
		return fmt.Errorf("failed to update pod network: %v", err)
	}

	return nil
}

func (node *OsdnNode) GetLocalPods(namespace string) ([]kapi.Pod, error) {
	return node.registry.GetRunningPods(node.hostName, namespace)
}

func (node *OsdnNode) markPodNetworkReady() {
	close(node.podNetworkReady)
}

func (node *OsdnNode) WaitForPodNetworkReady() error {
	logInterval := 10 * time.Second
	numIntervals := 12 // timeout: 2 mins

	for i := 0; i < numIntervals; i++ {
		select {
		// Wait for StartNode() to finish SDN setup
		case <-node.podNetworkReady:
			return nil
		case <-time.After(logInterval):
			log.Infof("Waiting for SDN pod network to be ready...")
		}
	}
	return fmt.Errorf("SDN pod network is not ready(timeout: 2 mins)")
}
