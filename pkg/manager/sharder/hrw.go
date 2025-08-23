package sharder

import (
	"hash/fnv"
)

type HRW struct{}

func NewHRW() *HRW { return &HRW{} }

func (h *HRW) ShouldOwn(clusterID string, peers []PeerInfo, self PeerInfo) bool {
	if len(peers) == 0 { return true }
	var best PeerInfo
	var bestScore uint64
	for _, p := range peers {
		score := hash64(clusterID + "|" + p.ID)
		if p.Weight == 0 { p.Weight = 1 }
		score *= uint64(p.Weight)
		if score >= bestScore {
			bestScore = score
			best = p
		}
	}
	return best.ID == self.ID
}

func hash64(s string) uint64 {
	h := fnv.New64a()
	_, _ = h.Write([]byte(s))
	return h.Sum64()
}
