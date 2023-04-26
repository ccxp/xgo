package xnet

import (
	"crypto/md5"
	"hash/fnv"
	"sort"
)

// Highest Random Weight hashing.
//
// 参考：https://github.com/codahale/hrw
type HrwHash struct {
	nodes []string
	ids   []int32
}

func NewHrwHash(nodes []string) *HrwHash {
	ids := make([]int32, len(nodes))
	for i, s := range nodes {
		sig := md5.Sum([]byte(s))

		var v uint32
		for j := 0; j < 4; j++ {
			v += uint32(sig[j*4+3])<<24 |
				uint32(sig[j*4+2])<<16 |
				uint32(sig[j*4+1])<<8 |
				uint32(sig[j*4+0])
		}
		ids[i] = int32(v)
	}
	return &HrwHash{
		nodes: nodes,
		ids:   ids,
	}
}

func (hrw *HrwHash) Calc(key []byte) []string {
	h := fnv.New32a()
	h.Write(key)
	d := int32(h.Sum32())

	entries := make(hrwEntryList, len(hrw.ids))

	for i, v := range hrw.ids {
		entries[i] = hrwEntry{idx: i, weight: hrwWeight(v, d)}
	}

	sort.Sort(entries)

	sorted := make([]string, len(hrw.nodes))
	for i, e := range entries {
		sorted[i] = hrw.nodes[e.idx]
	}
	return sorted
}

func (hrw *HrwHash) TopN(key []byte, n int) []string {
	h := fnv.New32a()
	h.Write(key)
	d := int32(h.Sum32())

	entries := make(hrwEntryList, len(hrw.ids))

	for i, v := range hrw.ids {
		entries[i] = hrwEntry{idx: i, weight: hrwWeight(v, d)}
	}

	sort.Sort(entries)

	if n > len(hrw.nodes) {
		n = len(hrw.nodes)
	}

	sorted := make([]string, n)
	for i := 0; i < n; i++ {
		sorted[i] = hrw.nodes[entries[i].idx]
	}
	return sorted
}

const (
	hrwA = 1103515245    // multiplier
	hrwC = 12345         // increment
	hrwM = (1 << 31) - 1 // modulus (2**32-1)
)

func hrwWeight(s, d int32) int {
	v := (hrwA * ((hrwA*s + hrwC) ^ d + hrwC))
	if v < 0 {
		v += hrwM
	}
	return int(v)
}

type hrwEntry struct {
	idx    int
	weight int
}

type hrwEntryList []hrwEntry

func (l hrwEntryList) Len() int {
	return len(l)
}

func (l hrwEntryList) Less(a, b int) bool {
	return l[a].weight > l[b].weight
}

func (l hrwEntryList) Swap(a, b int) {
	l[a], l[b] = l[b], l[a]
}
