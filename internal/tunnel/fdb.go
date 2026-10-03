package tunnel

import (
	"sync"
	"sync/atomic"
	"time"
)

const (
	// fdbAgeing is how long a learned MAC stays valid without being seen
	// again. Same as the Linux bridge and VXLAN default.
	fdbAgeing = 300 * time.Second
	// fdbMax bounds memory if a peer sends from many source MACs. Once
	// full, new MACs are not learned and frames to them are flooded.
	fdbMax = 16384
	// fdbRefresh limits how often a busy entry's timestamp is rewritten.
	fdbRefresh = time.Second
)

type mac [6]byte

// fdbEntry is updated in place under the read lock, so its fields are atomic.
type fdbEntry struct {
	peer atomic.Int32
	seen atomic.Int64 // monotonic nanoseconds, see Tunnel.now
}

// fdb maps overlay MAC addresses to the peer they were last seen behind.
// The receive loop learns, the send loop looks up.
type fdb struct {
	mu sync.RWMutex
	m  map[mac]*fdbEntry
}

func newFDB() *fdb { return &fdb{m: make(map[mac]*fdbEntry)} }

// learn records that src was seen behind peer at time now.
func (f *fdb) learn(src mac, peer int, now int64) {
	f.mu.RLock()
	e := f.m[src]
	f.mu.RUnlock()
	if e == nil {
		f.mu.Lock()
		if e = f.m[src]; e == nil {
			if len(f.m) >= fdbMax {
				f.mu.Unlock()
				return
			}
			e = new(fdbEntry)
			e.peer.Store(int32(peer))
			e.seen.Store(now)
			f.m[src] = e
			f.mu.Unlock()
			return
		}
		f.mu.Unlock()
	}
	if int(e.peer.Load()) != peer {
		e.peer.Store(int32(peer)) // the MAC moved
	}
	if now-e.seen.Load() >= int64(fdbRefresh) {
		e.seen.Store(now)
	}
}

// lookup returns the peer dst was last seen behind, if that was recent.
func (f *fdb) lookup(dst mac, now int64) (int, bool) {
	f.mu.RLock()
	e := f.m[dst]
	f.mu.RUnlock()
	if e == nil || now-e.seen.Load() >= int64(fdbAgeing) {
		return 0, false
	}
	return int(e.peer.Load()), true
}

// expire removes aged-out entries and returns how many remain.
func (f *fdb) expire(now int64) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	for k, e := range f.m {
		if now-e.seen.Load() >= int64(fdbAgeing) {
			delete(f.m, k)
		}
	}
	return len(f.m)
}
