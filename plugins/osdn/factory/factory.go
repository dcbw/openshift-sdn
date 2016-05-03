package factory

import (
	"strings"

	"github.com/openshift/openshift-sdn/plugins/osdn"
	"github.com/openshift/openshift-sdn/plugins/osdn/api"

	osclient "github.com/openshift/origin/pkg/client"
	kclient "k8s.io/kubernetes/pkg/client/unversioned"
)

// Call by higher layers to create the plugin SDN master instance
func NewMasterPlugin(pluginType string, osClient *osclient.Client, kClient *kclient.Client) (api.OsdnPlugin, error) {
	return newPlugin(pluginType, osClient, kClient, "", "")
}

// Call by higher layers to create the plugin SDN node instance
func NewNodePlugin(pluginType string, osClient *osclient.Client, kClient *kclient.Client, hostname string, selfIP string) (api.OsdnPlugin, error) {
	return newPlugin(pluginType, osClient, kClient, hostname, selfIP)
}

func newPlugin(pluginType string, osClient *osclient.Client, kClient *kclient.Client, hostname string, selfIP string) (api.OsdnPlugin, error) {
	switch strings.ToLower(pluginType) {
	case api.SingleTenantPluginName:
		return osdn.CreatePlugin(osdn.NewRegistry(osClient, kClient), false, hostname, selfIP)
	case api.MultiTenantPluginName:
		return osdn.CreatePlugin(osdn.NewRegistry(osClient, kClient), true, hostname, selfIP)
	}

	return nil, nil
}

// Call by higher layers to create the proxy plugin instance; only used by nodes
func NewProxyPlugin(pluginType string, osClient *osclient.Client, kClient *kclient.Client) (api.FilteringEndpointsConfigHandler, error) {
	switch strings.ToLower(pluginType) {
	case api.MultiTenantPluginName:
		return osdn.CreateProxyPlugin(osdn.NewRegistry(osClient, kClient))
	}

	return nil, nil
}
