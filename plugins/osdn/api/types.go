package api

import (
	pconfig "k8s.io/kubernetes/pkg/proxy/config"
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
	ClusterNetwork string
	NodeNetwork    string
	NodeGateway    string
	MTU            uint
}

const PodInfoSocketPath string = "/var/run/openshift-sdn/podinfo.sock"
