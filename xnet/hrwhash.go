package xnet

import (
	"crypto/md5"
	"hash/fnv"
	"sort"
	"sync"
)

// Highest Random Weight hashing.
//
// 参考：https://github.com/codahale/hrw
type HrwHash struct {
	nodes []string
	ids   []int32

	// 在大量计算时，减少重复分配内存
	entriesPool sync.Pool
	resPool     sync.Pool
}

func NewHrwHash(nodes []string) *HrwHash {
	l := len(nodes)
	ids := make([]int32, l)
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

func (hrw *HrwHash) calc(key []byte) *hrwEntryList {
	h := fnv.New32a()
	h.Write(key)
	d := int32(h.Sum32())

	var entries *hrwEntryList
	tmp := hrw.entriesPool.Get()
	if tmp == nil {
		entries = &hrwEntryList{
			rs: make([]hrwEntry, len(hrw.ids)),
		}
	} else {
		entries = tmp.(*hrwEntryList)
	}

	for i, v := range hrw.ids {
		entries.rs[i].idx = i
		entries.rs[i].weight = hrwWeight(v, d)
	}

	sort.Sort(entries)

	return entries
}

func (hrw *HrwHash) Calc(key []byte) []string {

	entries := hrw.calc(key)
	defer hrw.entriesPool.Put(entries)

	sorted := make([]string, len(hrw.nodes))
	for i, e := range entries.rs {
		sorted[i] = hrw.nodes[e.idx]
	}
	return sorted
}

func (hrw *HrwHash) TopN(key []byte, n int) []string {
	entries := hrw.calc(key)
	defer hrw.entriesPool.Put(entries)

	if n > len(hrw.nodes) {
		n = len(hrw.nodes)
	}

	sorted := make([]string, n)
	for i := 0; i < n; i++ {
		sorted[i] = hrw.nodes[entries.rs[i].idx]
	}
	return sorted
}

func (hrw *HrwHash) getRes() *HrwHashResult {
	tmp := hrw.resPool.Get()
	if tmp == nil {
		return &HrwHashResult{
			hrw: hrw,
			buf: make([]string, len(hrw.ids)),
		}
	}
	return tmp.(*HrwHashResult)
}

// 跟 Calc 一样，不过返回的 HrwHashResult 用完需要调用Close.
// 目的是防止内存大量重复分配，需要频繁计算，里面的节点很多时优先使用.
func (hrw *HrwHash) NewCalc(key []byte) *HrwHashResult {

	entries := hrw.calc(key)
	defer hrw.entriesPool.Put(entries)

	res := hrw.getRes()
	res.res = res.buf
	for i, e := range entries.rs {
		res.res[i] = hrw.nodes[e.idx]
	}
	return res
}

// 跟 NewTopN 一样，不过返回的 HrwHashResult 用完需要调用Close.
// 目的是防止内存大量重复分配，需要频繁计算，里面的节点很多时优先使用.
func (hrw *HrwHash) NewTopN(key []byte, n int) *HrwHashResult {
	entries := hrw.calc(key)
	defer hrw.entriesPool.Put(entries)

	if n > len(hrw.nodes) {
		n = len(hrw.nodes)
	}

	res := hrw.getRes()
	res.res = res.buf[0:n]
	for i := 0; i < n; i++ {
		res.res[i] = hrw.nodes[entries.rs[i].idx]
	}
	return res
}

type HrwHashResult struct {
	res []string

	hrw *HrwHash
	buf []string
}

// 取得结果，
// 注意，需要在不再使用 Get 返回的数据后，才可以调用Close()
func (r *HrwHashResult) Get() []string {
	return r.res
}

// 注意，需要在不再使用 Get 返回的数据后，才可以调用Close()，优先使用defer.
func (r *HrwHashResult) Close() {
	r.hrw.resPool.Put(r)
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

type hrwEntryList struct {
	rs []hrwEntry
}

func (l hrwEntryList) Len() int {
	return len(l.rs)
}

func (l hrwEntryList) Less(a, b int) bool {
	return l.rs[a].weight > l.rs[b].weight
}

func (l hrwEntryList) Swap(a, b int) {
	l.rs[a], l.rs[b] = l.rs[b], l.rs[a]
}
