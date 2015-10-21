package valuelocmap

import (
	"math/rand"
	"os"
	"runtime"
	"strconv"
	"sync"
	"sync/atomic"
	"unsafe"
)

// ValueLocMapConfig represents the set of values for configuring a
// ValueLocMap. Note that changing the values in this structure will have no
// effect on existing ValueLocMaps; they are copied on instance creation.
type ValueLocMapConfig struct {
	// Workers indicates how many workers may be in use (for calculating the
	// number of locks to create, for example). Note that the ValueLocMap does
	// not create any goroutines itself, but is written to allow concurrent
	// access. Defaults to GOMAXPROCS.
	Workers int
	// Roots indicates how many top level nodes the map should have. More top
	// level nodes means less contention but a bit more memory. Defaults to the
	// Workers value squared. This will be rounded up to the next power of two.
	// The floor for this setting is 2.
	Roots int
	// PageSize controls the size in bytes of each chunk of memory allocated.
	// Defaults to 1,048,576 bytes. The floor for this setting is four times
	// the Sizeof an internal entry (160 bytes for ValueLocMap and 224 bytes
	// for GroupLocMap).
	PageSize int
	// SplitMultiplier indicates how full a memory page can get before being
	// split into two pages. Defaults to 3.0, which means 3 times as many
	// entries as the page alone has slots (overflow subpages are used on
	// collisions).
	SplitMultiplier float64
}

func resolveValueLocMapConfig(c *ValueLocMapConfig) *ValueLocMapConfig {
	cfg := &ValueLocMapConfig{}
	if c != nil {
		*cfg = *c
	}
	if env := os.Getenv("ValueLOCMAP_WORKERS"); env != "" {
		if val, err := strconv.Atoi(env); err == nil {
			cfg.Workers = val
		}
	} else if env = os.Getenv("ValueSTORE_WORKERS"); env != "" {
		if val, err := strconv.Atoi(env); err == nil {
			cfg.Workers = val
		}
	}
	if cfg.Workers <= 0 {
		cfg.Workers = runtime.GOMAXPROCS(0)
	}
	if cfg.Workers < 1 { // GOMAXPROCS should always give >= 1, but in case
		cfg.Workers = 1
	}
	if env := os.Getenv("ValueLOCMAP_ROOTS"); env != "" {
		if val, err := strconv.Atoi(env); err == nil {
			cfg.Roots = val
		}
	}
	if cfg.Roots <= 0 {
		cfg.Roots = cfg.Workers * cfg.Workers
	}
	// Because the Roots logic needs at least a bit to work with.
	if cfg.Roots < 2 {
		cfg.Roots = 2
	}
	if env := os.Getenv("ValueLOCMAP_PAGESIZE"); env != "" {
		if val, err := strconv.Atoi(env); err == nil {
			cfg.PageSize = val
		}
	}
	if cfg.PageSize <= 0 {
		cfg.PageSize = 1048576
	}
	// Because the Page logic needs at least two bits to work with, so it can
	// split a page when needed.
	pageSizeFloor := 4 * int(unsafe.Sizeof(valueLocMapEntry{}))
	if cfg.PageSize < pageSizeFloor {
		cfg.PageSize = pageSizeFloor
	}
	if env := os.Getenv("ValueLOCMAP_SPLITMULTIPLIER"); env != "" {
		if val, err := strconv.ParseFloat(env, 64); err == nil {
			cfg.SplitMultiplier = val
		}
	}
	if cfg.SplitMultiplier <= 0 {
		cfg.SplitMultiplier = 3.0
	}
	return cfg
}

type valueLocMap struct {
	bits            uint32
	lowMask         uint32
	entriesLockMask uint32
	workers         uint32
	splitLevel      uint32
	rootShift       uint64
	roots           []valueLocMapNode
	inactiveMask    uint64
}

type valueLocMapNode struct {
	resizingLock       sync.Mutex
	resizing           bool
	lock               sync.RWMutex
	highMask           uint64
	rangeStart         uint64
	rangeStop          uint64
	splitLevel         uint32
	mergeLevel         uint32
	a                  *valueLocMapNode
	b                  *valueLocMapNode
	entries            []valueLocMapEntry
	entriesLocks       []sync.RWMutex
	overflow           [][]valueLocMapEntry
	overflowLowestFree uint32
	overflowLock       sync.RWMutex
	used               uint32
}

// If the Sizeof this changes, be sure to update docs.
type valueLocMapEntry struct {
	keyA uint64
	keyB uint64

	timestamp uint64
	blockID   uint32
	offset    uint32
	length    uint32
	next      uint32
}

// NewValueLocMap returns a new ValueLocMap instance using the config options
// given.
func NewValueLocMap(c *ValueLocMapConfig) ValueLocMap {
	cfg := resolveValueLocMapConfig(c)
	tlm := &valueLocMap{workers: uint32(cfg.Workers)}
	// Minimum bits = 2 and count = 4 because the Page logic needs at least two
	// bits to work with, so it can split a page when needed.
	tlm.bits = 2
	count := uint32(4)
	est := uint32(cfg.PageSize / int(unsafe.Sizeof(valueLocMapEntry{})))
	for count < est {
		tlm.bits++
		count <<= 1
	}
	tlm.lowMask = count - 1
	// Minimum rootShift = 63 and count = 2 because the Roots logic needs at
	// least 1 bit to work with.
	tlm.rootShift = 63
	count = 2
	for count < uint32(cfg.Roots) {
		tlm.rootShift--
		count <<= 1
	}
	tlm.entriesLockMask = count - 1
	tlm.splitLevel = uint32(float64(uint32(1<<tlm.bits)) * cfg.SplitMultiplier)
	tlm.roots = make([]valueLocMapNode, count)
	for i := 0; i < len(tlm.roots); i++ {
		tlm.roots[i].highMask = uint64(1) << (tlm.rootShift - 1)
		tlm.roots[i].rangeStart = uint64(i) << tlm.rootShift
		tlm.roots[i].rangeStop = uint64(1)<<tlm.rootShift - 1 + tlm.roots[i].rangeStart
		// Local splitLevel should be a random +-10% to keep splits across a
		// distributed load from synchronizing and causing overall "split lag".
		tlm.roots[i].splitLevel = uint32(float64(tlm.splitLevel) + (rand.Float64()-.5)/5*float64(tlm.splitLevel))
		// Local mergeLevel should be a random percentage, up to 10% of the
		// splitLevel, to keep merges across a distributed load from
		// synchronizing and causing overall "merge lag".
		tlm.roots[i].mergeLevel = uint32(rand.Float64() / 10 * float64(tlm.splitLevel))
	}
	return tlm
}

func (tlm *valueLocMap) split(n *valueLocMapNode) {
	newhm := n.highMask >> 1
	if newhm == 0 {
		// Can't split anymore since there are no more mask bits to use; means
		// this area will just be overfull. Everything should work fine, it
		// just won't be as fast. Really should only reach this issue with
		// grouplocmap or when keys barely differ in their high 64 bits.
		return
	}
	n.resizingLock.Lock()
	if n.resizing {
		n.resizingLock.Unlock()
		return
	}
	n.resizing = true
	n.resizingLock.Unlock()
	n.lock.Lock()
	u := atomic.LoadUint32(&n.used)
	if n.a != nil || u < n.splitLevel {
		n.lock.Unlock()
		n.resizingLock.Lock()
		n.resizing = false
		n.resizingLock.Unlock()
		return
	}
	hm := n.highMask
	var newsl uint32
	if newhm != 1 {
		newsl = uint32(float64(tlm.splitLevel) + (rand.Float64()-.5)/5*float64(tlm.splitLevel))
	}
	an := &valueLocMapNode{
		highMask:           newhm,
		rangeStart:         n.rangeStart,
		rangeStop:          hm - 1 + n.rangeStart,
		splitLevel:         newsl,
		mergeLevel:         uint32(rand.Float64() / 10 * float64(tlm.splitLevel)),
		entries:            n.entries,
		entriesLocks:       n.entriesLocks,
		overflow:           n.overflow,
		overflowLowestFree: n.overflowLowestFree,
		used:               n.used,
	}
	if newhm != 1 {
		newsl = uint32(float64(tlm.splitLevel) + (rand.Float64()-.5)/5*float64(tlm.splitLevel))
	}
	bn := &valueLocMapNode{
		highMask:     newhm,
		rangeStart:   hm + n.rangeStart,
		rangeStop:    n.rangeStop,
		splitLevel:   newsl,
		mergeLevel:   uint32(rand.Float64() / 10 * float64(tlm.splitLevel)),
		entries:      make([]valueLocMapEntry, len(n.entries)),
		entriesLocks: make([]sync.RWMutex, len(n.entriesLocks)),
	}
	n.a = an
	n.b = bn
	n.entries = nil
	n.entriesLocks = nil
	n.overflow = nil
	n.used = 0
	b := tlm.bits
	lm := tlm.lowMask
	ao := an.overflow
	bes := bn.entries
	bo := bn.overflow
	boc := uint32(0)
	aes := an.entries
	// Move over all matching overflow entries.
	for i := uint32(0); i <= lm; i++ {
		ae := &aes[i]
		if ae.blockID == 0 {
			continue
		}
		for ae.next != 0 {
			aen := &ao[ae.next>>b][ae.next&lm]
			if aen.keyA&hm == 0 {
				ae = aen
				continue
			}
			be := &bes[uint32(aen.keyB)&lm]
			if be.blockID == 0 {
				*be = *aen
				be.next = 0
			} else {
				if bn.overflowLowestFree != 0 {
					be2 := &bo[bn.overflowLowestFree>>b][bn.overflowLowestFree&lm]
					*be2 = *aen
					be2.next = be.next
					be.next = bn.overflowLowestFree
					bn.overflowLowestFree++
					if bn.overflowLowestFree&lm == 0 {
						bn.overflowLowestFree = 0
					}
				} else {
					bo = append(bo, make([]valueLocMapEntry, 1<<b))
					bn.overflow = bo
					if boc == 0 {
						be2 := &bo[0][1]
						*be2 = *aen
						be2.next = be.next
						be.next = 1
						bn.overflowLowestFree = 2
					} else {
						be2 := &bo[boc][0]
						*be2 = *aen
						be2.next = be.next
						be.next = boc << b
						bn.overflowLowestFree = boc<<b + 1
					}
					boc++
				}
			}
			bn.used++
			if ae.next < an.overflowLowestFree {
				an.overflowLowestFree = ae.next
			}
			ae.next = aen.next
			aen.blockID = 0
			aen.next = 0
			an.used--
		}
	}
	// Now any matching entries left are non-overflow entries. Move those.
	for i := uint32(0); i <= lm; i++ {
		ae := &aes[i]
		if ae.blockID == 0 || ae.keyA&hm == 0 {
			continue
		}
		be := &bes[i]
		if be.blockID == 0 {
			*be = *ae
			be.next = 0
		} else {
			if bn.overflowLowestFree != 0 {
				be2 := &bo[bn.overflowLowestFree>>b][bn.overflowLowestFree&lm]
				*be2 = *ae
				be2.next = be.next
				be.next = bn.overflowLowestFree
				bn.overflowLowestFree++
				if bn.overflowLowestFree&lm == 0 {
					bn.overflowLowestFree = 0
				}
			} else {
				bo = append(bo, make([]valueLocMapEntry, 1<<b))
				bn.overflow = bo
				if boc == 0 {
					be2 := &bo[0][1]
					*be2 = *ae
					be2.next = be.next
					be.next = 1
					bn.overflowLowestFree = 2
				} else {
					be2 := &bo[boc][0]
					*be2 = *ae
					be2.next = be.next
					be.next = boc << b
					bn.overflowLowestFree = boc<<b + 1
				}
				boc++
			}
		}
		bn.used++
		if ae.next == 0 {
			ae.blockID = 0
		} else {
			if ae.next < an.overflowLowestFree {
				an.overflowLowestFree = ae.next
			}
			aen := &ao[ae.next>>b][ae.next&lm]
			*ae = *aen
			aen.blockID = 0
			aen.next = 0
		}
		an.used--
	}
	n.lock.Unlock()
	n.resizingLock.Lock()
	n.resizing = false
	n.resizingLock.Unlock()
}

func (tlm *valueLocMap) merge(n *valueLocMapNode) {
	n.resizingLock.Lock()
	if n.resizing {
		n.resizingLock.Unlock()
		return
	}
	n.resizing = true
	n.resizingLock.Unlock()
	n.lock.Lock()
	if n.a == nil {
		n.lock.Unlock()
		n.resizingLock.Lock()
		n.resizing = false
		n.resizingLock.Unlock()
		return
	}
	an := n.a
	bn := n.b
	if atomic.LoadUint32(&an.used) < atomic.LoadUint32(&bn.used) {
		an, bn = bn, an
	}
	an.resizingLock.Lock()
	if an.resizing {
		an.resizingLock.Unlock()
		n.lock.Unlock()
		n.resizingLock.Lock()
		n.resizing = false
		n.resizingLock.Unlock()
		return
	}
	an.resizing = true
	an.resizingLock.Unlock()
	an.lock.Lock()
	if an.a != nil {
		an.lock.Unlock()
		an.resizingLock.Lock()
		an.resizing = false
		an.resizingLock.Unlock()
		n.lock.Unlock()
		n.resizingLock.Lock()
		n.resizing = false
		n.resizingLock.Unlock()
		return
	}
	bn.resizingLock.Lock()
	if bn.resizing {
		bn.resizingLock.Unlock()
		an.lock.Unlock()
		an.resizingLock.Lock()
		an.resizing = false
		an.resizingLock.Unlock()
		n.lock.Unlock()
		n.resizingLock.Lock()
		n.resizing = false
		n.resizingLock.Unlock()
		return
	}
	bn.resizing = true
	bn.resizingLock.Unlock()
	bn.lock.Lock()
	if bn.a != nil {
		bn.lock.Unlock()
		bn.resizingLock.Lock()
		bn.resizing = false
		bn.resizingLock.Unlock()
		an.lock.Unlock()
		an.resizingLock.Lock()
		an.resizing = false
		an.resizingLock.Unlock()
		n.lock.Unlock()
		n.resizingLock.Lock()
		n.resizing = false
		n.resizingLock.Unlock()
		return
	}
	b := tlm.bits
	lm := tlm.lowMask
	aes := an.entries
	ao := an.overflow
	aoc := uint32(len(ao))
	bo := bn.overflow
	bes := bn.entries
	for i := uint32(0); i <= lm; i++ {
		be := &bes[i]
		if be.blockID == 0 {
			continue
		}
		for {
			ae := &aes[uint32(be.keyB)&lm]
			if ae.blockID == 0 {
				*ae = *be
				ae.next = 0
			} else {
				if an.overflowLowestFree != 0 {
					oA := an.overflowLowestFree >> b
					oB := an.overflowLowestFree & lm
					ae2 := &ao[oA][oB]
					*ae2 = *be
					ae2.next = ae.next
					ae.next = an.overflowLowestFree
					an.overflowLowestFree = 0
					for {
						if oB == lm {
							oA++
							if oA == aoc {
								break
							}
							oB = 0
						} else {
							oB++
						}
						if ao[oA][oB].blockID == 0 {
							an.overflowLowestFree = oA<<b | oB
							break
						}
					}
				} else {
					ao = append(ao, make([]valueLocMapEntry, 1<<b))
					an.overflow = ao
					if aoc == 0 {
						ae2 := &ao[0][1]
						*ae2 = *be
						ae2.next = ae.next
						ae.next = 1
						an.overflowLowestFree = 2
					} else {
						ae2 := &ao[aoc][0]
						*ae2 = *be
						ae2.next = ae.next
						ae.next = aoc << b
						an.overflowLowestFree = aoc<<b + 1
					}
					aoc++
				}
			}
			an.used++
			if be.next == 0 {
				break
			}
			be = &bo[be.next>>b][be.next&lm]
		}
	}
	n.a = nil
	n.b = nil
	n.entries = an.entries
	n.entriesLocks = an.entriesLocks
	n.overflow = an.overflow
	n.overflowLowestFree = an.overflowLowestFree
	n.used = an.used
	bn.lock.Unlock()
	bn.resizingLock.Lock()
	bn.resizing = false
	bn.resizingLock.Unlock()
	an.lock.Unlock()
	an.resizingLock.Lock()
	an.resizing = false
	an.resizingLock.Unlock()
	n.lock.Unlock()
	n.resizingLock.Lock()
	n.resizing = false
	n.resizingLock.Unlock()
}

func (tlm *valueLocMap) Get(keyA uint64, keyB uint64) (uint64, uint32, uint32, uint32) {
	n := &tlm.roots[keyA>>tlm.rootShift]
	n.lock.RLock()
	for {
		if n.a == nil {
			if n.entries == nil {
				n.lock.RUnlock()
				return 0, 0, 0, 0
			}
			break
		}
		l := &n.lock
		if keyA&n.highMask == 0 {
			n = n.a
		} else {
			n = n.b
		}
		n.lock.RLock()
		l.RUnlock()
	}
	b := tlm.bits
	lm := tlm.lowMask
	i := uint32(keyB) & lm
	l := &n.entriesLocks[i&tlm.entriesLockMask]
	ol := &n.overflowLock
	e := &n.entries[i]
	l.RLock()
	if e.blockID == 0 {
		l.RUnlock()
		n.lock.RUnlock()
		return 0, 0, 0, 0
	}
	for {
		if e.keyA == keyA && e.keyB == keyB {
			rt := e.timestamp
			rb := e.blockID
			ro := e.offset
			rl := e.length
			l.RUnlock()
			n.lock.RUnlock()
			return rt, rb, ro, rl
		}
		if e.next == 0 {
			break
		}
		ol.RLock()
		e = &n.overflow[e.next>>b][e.next&lm]
		ol.RUnlock()
	}
	l.RUnlock()
	n.lock.RUnlock()
	return 0, 0, 0, 0
}

func (tlm *valueLocMap) Set(keyA uint64, keyB uint64, timestamp uint64, blockID uint32, offset uint32, length uint32, evenIfSameTimestamp bool) uint64 {
	n := &tlm.roots[keyA>>tlm.rootShift]
	var pn *valueLocMapNode
	n.lock.RLock()
	for {
		if n.a == nil {
			if n.entries != nil {
				break
			}
			n.lock.RUnlock()
			n.lock.Lock()
			if n.entries == nil {
				n.entries = make([]valueLocMapEntry, 1<<tlm.bits)
				n.entriesLocks = make([]sync.RWMutex, tlm.entriesLockMask+1)
			}
			n.lock.Unlock()
			n.lock.RLock()
			continue
		}
		pn = n
		if keyA&n.highMask == 0 {
			n = n.a
		} else {
			n = n.b
		}
		n.lock.RLock()
		pn.lock.RUnlock()
	}
	b := tlm.bits
	lm := tlm.lowMask
	i := uint32(keyB) & lm
	l := &n.entriesLocks[i&tlm.entriesLockMask]
	ol := &n.overflowLock
	e := &n.entries[i]
	var ep *valueLocMapEntry
	l.Lock()
	if e.blockID != 0 {
		var f uint32
		for {
			if e.keyA == keyA && e.keyB == keyB {
				t := e.timestamp
				if e.timestamp > timestamp || (e.timestamp == timestamp && !evenIfSameTimestamp) {
					l.Unlock()
					n.lock.RUnlock()
					return t
				}
				if blockID != 0 {
					e.timestamp = timestamp
					e.blockID = blockID
					e.offset = offset
					e.length = length
					l.Unlock()
					n.lock.RUnlock()
					return t
				}
				var u uint32
				if ep == nil {
					if e.next == 0 {
						e.blockID = 0
					} else {
						f = e.next
						ol.Lock()
						en := &n.overflow[f>>b][f&lm]
						*e = *en
						en.blockID = 0
						en.next = 0
						if f < n.overflowLowestFree {
							n.overflowLowestFree = f
						}
						ol.Unlock()
					}
				} else {
					ol.Lock()
					ep.next = e.next
					e.blockID = 0
					e.next = 0
					if f < n.overflowLowestFree {
						n.overflowLowestFree = f
					}
					ol.Unlock()
				}
				u = atomic.AddUint32(&n.used, ^uint32(0))
				l.Unlock()
				n.lock.RUnlock()
				if u <= n.mergeLevel && pn != nil {
					tlm.merge(pn)
				}
				return t
			}
			if e.next == 0 {
				break
			}
			ep = e
			f = e.next
			ol.RLock()
			e = &n.overflow[f>>b][f&lm]
			ol.RUnlock()
		}
	}
	if blockID == 0 {
		l.Unlock()
		n.lock.RUnlock()
		return 0
	}
	e = &n.entries[i]
	if e.blockID == 0 {
		e.keyA = keyA
		e.keyB = keyB
		e.timestamp = timestamp
		e.blockID = blockID
		e.offset = offset
		e.length = length
	} else {
		ol.Lock()
		o := n.overflow
		oc := uint32(len(o))
		if n.overflowLowestFree != 0 {
			oA := n.overflowLowestFree >> b
			oB := n.overflowLowestFree & lm
			e = &o[oA][oB]
			e.next = n.entries[i].next
			n.entries[i].next = n.overflowLowestFree
			n.overflowLowestFree = 0
			for {
				if oB == lm {
					oA++
					if oA == oc {
						break
					}
					oB = 0
				} else {
					oB++
				}
				if o[oA][oB].blockID == 0 {
					n.overflowLowestFree = oA<<b | oB
					break
				}
			}
		} else {
			n.overflow = append(n.overflow, make([]valueLocMapEntry, 1<<b))
			if oc == 0 {
				e = &n.overflow[0][1]
				e.next = n.entries[i].next
				n.entries[i].next = 1
				n.overflowLowestFree = 2
			} else {
				e = &n.overflow[oc][0]
				e.next = n.entries[i].next
				n.entries[i].next = oc << b
				n.overflowLowestFree = oc<<b + 1
			}
		}
		e.keyA = keyA
		e.keyB = keyB
		e.timestamp = timestamp
		e.blockID = blockID
		e.offset = offset
		e.length = length
		ol.Unlock()
	}
	u := atomic.AddUint32(&n.used, 1)
	l.Unlock()
	n.lock.RUnlock()
	if n.splitLevel != 0 && u >= n.splitLevel {
		tlm.split(n)
	}
	return 0
}

func (tlm *valueLocMap) Discard(start uint64, stop uint64, mask uint64) {
	for i := 0; i < len(tlm.roots); i++ {
		n := &tlm.roots[i]
		n.lock.RLock() // Will be released by discard
		tlm.discard(start, stop, mask, n)
	}
}

// Will call n.lock.RUnlock()
func (tlm *valueLocMap) discard(start uint64, stop uint64, mask uint64, n *valueLocMapNode) {
	if start > n.rangeStop || stop < n.rangeStart {
		n.lock.RUnlock()
		return
	}
	if n.a != nil {
		// It's okay if we run Discard on nodes that aren't reachable any
		// longer, whereas with Get and Set their work must be reachable. So
		// Discard will pull the subnode values, unlock the parent, and then
		// work on the children so it doesn't keep things tied up.
		a := n.a
		b := n.b
		n.lock.RUnlock()
		a.lock.RLock() // Will be released by discard
		tlm.discard(start, stop, mask, a)
		if b != nil {
			b.lock.RLock() // Will be released by discard
			tlm.discard(start, stop, mask, b)
		}
		return
	}
	if n.used == 0 {
		n.lock.RUnlock()
		return
	}
	b := tlm.bits
	lm := tlm.lowMask
	es := n.entries
	ol := &n.overflowLock
	for i := uint32(0); i <= lm; i++ {
		e := &es[i]
		l := &n.entriesLocks[i&tlm.entriesLockMask]
		l.Lock()
		if e.blockID == 0 {
			l.Unlock()
			continue
		}
		var p *valueLocMapEntry
		for {
			if e.keyA >= start && e.keyA <= stop && e.timestamp&mask != 0 {
				if p == nil {
					if e.next == 0 {
						e.blockID = 0
						break
					} else {
						ol.Lock()
						if e.next < n.overflowLowestFree {
							n.overflowLowestFree = e.next
						}
						en := &n.overflow[e.next>>b][e.next&lm]
						*e = *en
						en.blockID = 0
						en.next = 0
						ol.Unlock()
						continue
					}
				} else {
					ol.Lock()
					if p.next < n.overflowLowestFree {
						n.overflowLowestFree = p.next
					}
					p.next = e.next
					e.blockID = 0
					e.next = 0
					if p.next == 0 {
						ol.Unlock()
						break
					}
					e = &n.overflow[p.next>>b][p.next&lm]
					ol.Unlock()
					continue
				}
			}
			if e.next == 0 {
				break
			}
			p = e
			ol.RLock()
			e = &n.overflow[e.next>>b][e.next&lm]
			ol.RUnlock()
		}
		l.Unlock()
	}
	n.lock.RUnlock()
}

func (tlm *valueLocMap) ScanCallback(start uint64, stop uint64, mask uint64, notMask uint64, cutoff uint64, max uint64, callback func(keyA uint64, keyB uint64, timestamp uint64, length uint32) bool) (uint64, bool) {
	var stopped uint64
	var more bool
	for i := 0; i < len(tlm.roots); i++ {
		n := &tlm.roots[i]
		n.lock.RLock() // Will be released by scanCallback
		max, stopped, more = tlm.scanCallback(start, stop, mask, notMask, cutoff, max, callback, n)
		if more {
			break
		}
	}
	return stopped, more
}

// Will call n.lock.RUnlock()
func (tlm *valueLocMap) scanCallback(start uint64, stop uint64, mask uint64, notMask uint64, cutoff uint64, max uint64, callback func(keyA uint64, keyB uint64, timestamp uint64, length uint32) bool, n *valueLocMapNode) (uint64, uint64, bool) {
	if start > n.rangeStop || stop < n.rangeStart {
		n.lock.RUnlock()
		return max, stop, false
	}
	if n.a != nil {
		var stopped uint64
		var more bool
		a := n.a
		b := n.b
		n.lock.RUnlock()
		a.lock.RLock() // Will be released by scanCallback
		max, stopped, more = tlm.scanCallback(start, stop, mask, notMask, cutoff, max, callback, a)
		if !more {
			if b != nil {
				b.lock.RLock() // Will be released by scanCallback
				max, stopped, more = tlm.scanCallback(start, stop, mask, notMask, cutoff, max, callback, b)
			}
		}
		return max, stopped, more
	}
	if n.used == 0 {
		n.lock.RUnlock()
		return max, stop, false
	}
	if start <= n.rangeStart && stop >= n.rangeStop {
		var stopped uint64
		var more bool
		b := tlm.bits
		lm := tlm.lowMask
		es := n.entries
		ol := &n.overflowLock
		for i := uint32(0); !more && i <= lm; i++ {
			e := &es[i]
			l := &n.entriesLocks[i&tlm.entriesLockMask]
			l.RLock()
			if e.blockID == 0 {
				l.RUnlock()
				continue
			}
			for {
				if (mask == 0 || e.timestamp&mask != 0) && e.timestamp&notMask == 0 && e.timestamp <= cutoff {
					if max < 1 {
						stopped = n.rangeStart
						more = true
						break
					}
					if !callback(e.keyA, e.keyB, e.timestamp, e.length) {
						stopped = n.rangeStart
						more = true
						break
					}
					max--
				}
				if e.next == 0 {
					break
				}
				ol.RLock()
				e = &n.overflow[e.next>>b][e.next&lm]
				ol.RUnlock()
			}
			stopped = n.rangeStop
			l.RUnlock()
		}
		n.lock.RUnlock()
		return max, stopped, more
	}
	var stopped uint64
	var more bool
	b := tlm.bits
	lm := tlm.lowMask
	es := n.entries
	ol := &n.overflowLock
	for i := uint32(0); !more && i <= lm; i++ {
		e := &es[i]
		l := &n.entriesLocks[i&tlm.entriesLockMask]
		l.RLock()
		if e.blockID == 0 {
			l.RUnlock()
			continue
		}
		for {
			if e.keyA >= start && e.keyA <= stop && (mask == 0 || e.timestamp&mask != 0) && e.timestamp&notMask == 0 && e.timestamp <= cutoff {
				if max < 1 {
					stopped = n.rangeStart
					more = true
					break
				}
				if !callback(e.keyA, e.keyB, e.timestamp, e.length) {
					stopped = n.rangeStart
					more = true
					break
				}
				max--
			}
			if e.next == 0 {
				break
			}
			ol.RLock()
			e = &n.overflow[e.next>>b][e.next&lm]
			ol.RUnlock()
		}
		stopped = n.rangeStop
		l.RUnlock()
	}
	n.lock.RUnlock()
	return max, stopped, more
}

func (tlm *valueLocMap) SetInactiveMask(mask uint64) {
	tlm.inactiveMask = mask
}

func (tlm *valueLocMap) Stats(debug bool) *Stats {
	s := &Stats{
		inactiveMask:      tlm.inactiveMask,
		statsDebug:        debug,
		workers:           tlm.workers,
		roots:             uint32(len(tlm.roots)),
		entryPageSize:     uint64(tlm.lowMask) + 1,
		entryLockPageSize: uint64(tlm.entriesLockMask) + 1,
		splitLevel:        uint32(tlm.splitLevel),
	}
	for i := uint32(0); i < s.roots; i++ {
		n := &tlm.roots[i]
		n.lock.RLock() // Will be released by stats
		if s.statsDebug && (n.a != nil || n.entries != nil) {
			s.usedRoots++
		}
		tlm.stats(s, n, 0)
	}
	return s
}

// Will call n.lock.RUnlock()
func (tlm *valueLocMap) stats(s *Stats, n *valueLocMapNode, depth int) {
	if s.statsDebug {
		s.nodes++
		for len(s.depthCounts) <= depth {
			s.depthCounts = append(s.depthCounts, 0)
		}
		s.depthCounts[depth]++
	}
	if n.a != nil {
		a := n.a
		b := n.b
		n.lock.RUnlock()
		a.lock.RLock() // Will be released by stats
		tlm.stats(s, a, depth+1)
		if b != nil {
			b.lock.RLock() // Will be released by stats
			tlm.stats(s, b, depth+1)
		}
	} else if n.used == 0 {
		if s.statsDebug {
			s.allocedEntries += uint64(len(n.entries))
			n.overflowLock.RLock()
			for _, o := range n.overflow {
				s.allocedEntries += uint64(len(o))
				s.allocedInOverflow += uint64(len(o))
			}
			n.overflowLock.RUnlock()
		}
		n.lock.RUnlock()
	} else {
		if s.statsDebug {
			s.allocedEntries += uint64(len(n.entries))
			n.overflowLock.RLock()
			for _, o := range n.overflow {
				s.allocedEntries += uint64(len(o))
				s.allocedInOverflow += uint64(len(o))
			}
			n.overflowLock.RUnlock()
		}
		b := tlm.bits
		lm := tlm.lowMask
		es := n.entries
		ol := &n.overflowLock
		for i := uint32(0); i <= lm; i++ {
			e := &es[i]
			l := &n.entriesLocks[i&tlm.entriesLockMask]
			o := false
			l.RLock()
			if e.blockID == 0 {
				l.RUnlock()
				continue
			}
			for {
				if s.statsDebug {
					s.usedEntries++
					if o {
						s.usedInOverflow++
					}
					if e.timestamp&s.inactiveMask == 0 {
						s.ActiveCount++
						s.ActiveBytes += uint64(e.length)
					} else {
						s.inactive++
					}
				} else if e.timestamp&s.inactiveMask == 0 {
					s.ActiveCount++
					s.ActiveBytes += uint64(e.length)
				}
				if e.next == 0 {
					break
				}
				ol.RLock()
				e = &n.overflow[e.next>>b][e.next&lm]
				ol.RUnlock()
				o = true
			}
			l.RUnlock()
		}
		n.lock.RUnlock()
	}
}
