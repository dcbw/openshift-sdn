package osdn

import (
	"fmt"
	"sync"
	"time"

	log "github.com/golang/glog"

	"k8s.io/kubernetes/pkg/util/sets"
)

type VNIDMap struct {
	ids  map[string]uint
	lock sync.Mutex
}

func NewVNIDMap() VNIDMap {
	return VNIDMap{ids: make(map[string]uint)}
}

func (vmap VNIDMap) PopulateVNIDs(registry *Registry) error {
	nets, err := registry.GetNetNamespaces()
	if err != nil {
		return err
	}

	for _, net := range nets {
		vmap.SetVNID(net.Name, net.NetID)
	}
	return nil
}

func (vmap VNIDMap) GetVNID(name string) (uint, error) {
	vmap.lock.Lock()
	defer vmap.lock.Unlock()

	if id, ok := vmap.ids[name]; ok {
		return id, nil
	}
	// In case of error, return some value which is not a valid VNID
	return MaxVNID + 1, fmt.Errorf("Failed to find netid for namespace: %s in vnid map", name)
}

// Nodes asynchronously watch for both NetNamespaces and services
// NetNamespaces populates vnid map and services/pod-setup depend on vnid map
// If for some reason, vnid map propagation from master to node is slow
// and if service/pod-setup tries to lookup vnid map then it may fail.
// So, use this method to alleviate this problem. This method will
// retry vnid lookup before giving up.
func (vmap VNIDMap) WaitAndGetVNID(name string) (uint, error) {
	// Try few times up to 2 seconds
	retries := 20
	retryInterval := 100 * time.Millisecond
	for i := 0; i < retries; i++ {
		if id, err := vmap.GetVNID(name); err == nil {
			return id, nil
		}
		time.Sleep(retryInterval)
	}

	// In case of error, return some value which is not a valid VNID
	return MaxVNID + 1, fmt.Errorf("Failed to find netid for namespace: %s in vnid map", name)
}

func (vmap VNIDMap) SetVNID(name string, id uint) {
	vmap.lock.Lock()
	defer vmap.lock.Unlock()

	vmap.ids[name] = id
	log.Infof("Associate netid %d to namespace %q", id, name)
}

func (vmap VNIDMap) UnsetVNID(name string) (id uint, err error) {
	vmap.lock.Lock()
	defer vmap.lock.Unlock()

	id, found := vmap.ids[name]
	if !found {
		// In case of error, return some value which is not a valid VNID
		return MaxVNID + 1, fmt.Errorf("Failed to find netid for namespace: %s in vnid map", name)
	}
	delete(vmap.ids, name)
	log.Infof("Dissociate netid %d from namespace %q", id, name)
	return id, nil
}

func (vmap VNIDMap) CheckVNID(id uint) bool {
	vmap.lock.Lock()
	defer vmap.lock.Unlock()

	for _, netid := range vmap.ids {
		if netid == id {
			return true
		}
	}
	return false
}

func (vmap VNIDMap) GetAllocatedVNIDs() []uint {
	vmap.lock.Lock()
	defer vmap.lock.Unlock()

	ids := []uint{}
	idSet := sets.Int{}
	for _, id := range vmap.ids {
		if id != AdminVNID {
			if !idSet.Has(int(id)) {
				ids = append(ids, id)
				idSet.Insert(int(id))
			}
		}
	}
	return ids
}
