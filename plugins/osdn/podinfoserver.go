package osdn

import (
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"os"
//	"path"

	"github.com/gorilla/mux"

	"github.com/openshift/openshift-sdn/pkg/netutils"
	"github.com/openshift/openshift-sdn/plugins/osdn/api"
)

type PodInfoServer struct {
	http.Server

	registry *Registry
	vnids    *vnidMap
	nodeName string

	clusterNetwork string
	nodeNetwork    string
	nodeGateway    string
	mtu            uint
}

func NewPodInfoServer(registry *Registry, vnids *vnidMap, nodeName string) *PodInfoServer {
	router := mux.NewRouter()
 
	s := &PodInfoServer{
		registry: registry,
		vnids:  vnids,
		nodeName: nodeName,
		Server:   http.Server{
			Handler: router,
		},
	}

	router.NotFoundHandler = http.HandlerFunc(http.NotFound)
	router.Methods("GET").Path("/network").HandlerFunc(s.getNetwork)
	router.Methods("GET").Path("/pods/{namespace}/{name}").HandlerFunc(s.getPod)

	return s
}

func (s *PodInfoServer) Start(clusterNetworkCIDR, localSubnetCIDR string, mtu uint) error {
	s.clusterNetwork = clusterNetworkCIDR
	s.nodeNetwork = localSubnetCIDR
	_, ipnet, err := net.ParseCIDR(localSubnetCIDR)
	s.nodeGateway = netutils.GenerateDefaultGateway(ipnet).String()
	s.mtu = mtu

	// On Linux the socket is created with the permissions of the directory
	// it is in, so as long as the directory is root-only we can avoid
	// racy umask manipulation.
	l, err := net.Listen("unix", api.PodInfoSocketPath)
	if err != nil {
		return fmt.Errorf("failed to listen on pod info socket: %v", err)
	}
	if err := os.Chmod(api.PodInfoSocketPath, 0600); err != nil {
		l.Close()
		return fmt.Errorf("failed to set pod info socket mode: %v", err)
	}

	s.SetKeepAlivesEnabled(false)
	s.Serve(l)
	return nil
}

func (s *PodInfoServer) getNetwork(w http.ResponseWriter, r *http.Request) {
	err := json.NewEncoder(w).Encode(&api.GetNetworkResponse{
		ClusterNetwork: s.clusterNetwork,
		NodeNetwork:    s.nodeNetwork,
		NodeGateway:    s.nodeGateway,
		MTU:            s.mtu,
	})
	if err != nil {
		http.Error(w, fmt.Sprintf("encode error: %v", err), http.StatusInternalServerError)
	}
}

func (s *PodInfoServer) getPod(w http.ResponseWriter, r *http.Request) {
	vars := mux.Vars(r)
	namespace, ok := vars["namespace"]
	if !ok {
		http.NotFound(w, r)
		return
	}
	name, ok := vars["name"]
	if !ok {
		http.NotFound(w, r)
		return
	}

	pod, err := s.registry.GetPod(s.nodeName, namespace, name)
	if err != nil {
		http.Error(w, fmt.Sprintf("failed to find pod %s/%s: %v", namespace, name, err), http.StatusInternalServerError)
		return
	}

	privileged := false
	for _, container := range pod.Spec.Containers {
		if container.SecurityContext.Privileged != nil && *container.SecurityContext.Privileged {
			privileged = true
			break
		}
	}

	var vnid uint
	if s.vnids != nil {
		vnid, err = s.vnids.WaitAndGetVNID(namespace)
		if err != nil {
			http.Error(w, fmt.Sprintf("failed to find VNID for namespace %s: %v", namespace, err), http.StatusInternalServerError)
			return
		}
	}

	err = json.NewEncoder(w).Encode(&api.GetPodResponse{
		Vnid:        vnid,
		Privileged:  privileged,
		Annotations: pod.Annotations,
	})
	if err != nil {
		http.Error(w, fmt.Sprintf("encode error: %v", err), http.StatusInternalServerError)
	}
}
