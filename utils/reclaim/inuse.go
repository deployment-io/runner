package reclaim

import "sync"

// inUse counts, per image ref, how many jobs in this process are between
// "pulled the image" and "done with the container that runs it".
//
// The container-reference check in protectedImages covers an image once a
// container exists for it. It does NOT cover the window between the pull and
// the ContainerCreate — and that window is exactly where a concurrent Step
// pulling a newer agentbox tag would otherwise decide the older tag is
// superseded and remove it out from under a Step that is about to start.
// Removing an image mid-Step kills a live job, so the pull path brackets
// itself with MarkImageInUse and every removal consults this map.
//
// Refcounted rather than a flag: two Steps can legitimately hold the same
// ref, and the first to finish must not clear the second's protection.
var inUse = struct {
	sync.Mutex
	refs map[string]int
}{refs: map[string]int{}}

// MarkImageInUse protects ref from removal until the returned release is
// called. Callers defer the release for the lifetime of the work that needs
// the image — for a Step, from before the pull until the container is gone.
//
// Release is idempotent, so a defer plus an early explicit call is safe.
func MarkImageInUse(ref string) (release func()) {
	key := normalizeRef(ref)
	inUse.Lock()
	inUse.refs[key]++
	inUse.Unlock()
	released := false
	var once sync.Mutex
	return func() {
		once.Lock()
		defer once.Unlock()
		if released {
			return
		}
		released = true
		inUse.Lock()
		defer inUse.Unlock()
		if inUse.refs[key] <= 1 {
			delete(inUse.refs, key)
			return
		}
		inUse.refs[key]--
	}
}

// inUseRefs snapshots the refs currently held by in-flight work.
func inUseRefs() map[string]bool {
	inUse.Lock()
	defer inUse.Unlock()
	held := make(map[string]bool, len(inUse.refs))
	for ref := range inUse.refs {
		held[ref] = true
	}
	return held
}
