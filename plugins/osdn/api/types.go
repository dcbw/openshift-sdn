package api

import (
	pconfig "k8s.io/kubernetes/pkg/proxy/config"

	cnitypes "github.com/containernetworking/cni/pkg/types"
)

type FilteringEndpointsConfigHandler interface {
	pconfig.EndpointsConfigHandler
	Start(baseHandler pconfig.EndpointsConfigHandler) error
}

type GetPodResponse struct {
	Vnid        uint
	Privileged  bool
	Annotations map[string]string
}

type GetNetworkResponse struct {
	ClusterNetwork string `json:"clusterNetwork"`
	NodeNetwork    string `json:"nodeNetwork"`
	NodeGateway    string `json:"nodeGateway"`
	MTU            uint `json:"mtu"`
	Multitenant    bool `json:"multitenant"`
}

type SDNNetConf struct {
	cnitypes.NetConf
	MasterKubeConfig string `json:"masterKubeConfig"`
	Multitenant      bool   `json:"multitenant"`
}


const PodInfoSocketPath string = "/var/run/openshift-sdn/podinfo.sock"
const NodeConfigPath string = "/var/run/openshift-sdn/nodeInfo.json"
