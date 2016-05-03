package osdn

import (
	"fmt"
	"strings"

	"github.com/openshift/openshift-sdn/pkg/netutils"
	"github.com/openshift/openshift-sdn/plugins/osdn/api"

	osapi "github.com/openshift/origin/pkg/sdn/api"

	kapi "k8s.io/kubernetes/pkg/api"
)

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
