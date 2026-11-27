package networkruntime

import "github.com/windshare/windshare/connectivity/icepolicy"

func (r *Runtime) ICEPool() icepolicy.ICEEndpointPool {
	pool := icepolicy.DefaultPool()
	if r.directory != nil {
		if snapshot, usable := r.directory.Snapshot(); usable {
			pool = icepolicy.DirectoryPool(snapshot.Payload())
		}
	}
	if c, allowed := r.communitySettings(); allowed {
		pool = pool.WithCommunity(r.community.STUN(r.scope(r.frontend(c.FrontURL).primary)))
	}
	return pool
}
