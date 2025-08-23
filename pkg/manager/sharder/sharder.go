package sharder

type PeerInfo struct {
	ID     string
	Weight uint32 // optional (default 1)
}

type Sharder interface {
	ShouldOwn(clusterID string, peers []PeerInfo, self PeerInfo) bool
}
