package networkruntime

import "github.com/windshare/windshare/connectivity/icepolicy"

func (r *Runtime) ICEPool() icepolicy.ICEEndpointPool {
	if r.directory != nil {
		if snapshot, usable := r.directory.Snapshot(); usable {
			return icepolicy.DirectoryPool(snapshot.Payload())
		}
	}
	return icepolicy.DefaultPool()
}
