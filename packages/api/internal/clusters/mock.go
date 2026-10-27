package clusters

import (
	"github.com/google/uuid"
	"google.golang.org/grpc"

	"github.com/e2b-dev/infra/packages/shared/pkg/smap"
)

// NewTestPool builds a Pool pre-populated with the given clusters, for use in
// tests that need cluster lookups (e.g. GetClusterById) without spinning up a
// full synchronization loop.
func NewTestPool(clusters ...*Cluster) *Pool {
	m := smap.New[*Cluster]()
	for _, c := range clusters {
		m.Insert(c.ID.String(), c)
	}

	return &Pool{clusters: m, localInstances: make(chan struct{}, 1)}
}

// NewTestCluster builds a minimal Cluster carrying just an ID and sandbox
// domain, for tests. Other collaborators (instances, synchronization,
// resources) are left nil.
func NewTestCluster(id uuid.UUID, sandboxDomain *string) *Cluster {
	return &Cluster{
		ID:            id,
		SandboxDomain: sandboxDomain,
	}
}

// NewTestSharedGRPCClient builds a client that issues its calls on conn the
// way a remote cluster's instances do, for tests: closing it retires the
// client and leaves conn open.
func NewTestSharedGRPCClient(conn *grpc.ClientConn) *GRPCClient {
	return newSharedGRPCClient(conn, "test", func() {})
}
