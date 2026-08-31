package book

import "sync/atomic"

// atomicView stores an immutable SnapshotView for lock-free readers.
type atomicView struct {
	v atomic.Value
}

func (a *atomicView) Load() (SnapshotView, bool) {
	x := a.v.Load()
	if x == nil {
		return SnapshotView{}, false
	}
	return x.(SnapshotView), true
}

func (a *atomicView) Store(s SnapshotView) { a.v.Store(s) }
