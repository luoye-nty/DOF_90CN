package pvf

import "sync"

// indexFlight coalesces concurrent first reads of one PVF file or body chunk.
// It is intentionally archive-local, so replacing the PVF archive (and its
// SHA-256 identity) also replaces every in-flight and cached value.
type indexFlight struct {
	mu    sync.Mutex
	calls map[int]*indexFlightCall
}

type indexFlightCall struct {
	done  chan struct{}
	value any
	err   error
}

func (f *indexFlight) do(key int, load func() (any, error)) (any, error) {
	f.mu.Lock()
	if f.calls == nil {
		f.calls = make(map[int]*indexFlightCall)
	}
	if call, ok := f.calls[key]; ok {
		f.mu.Unlock()
		<-call.done
		return call.value, call.err
	}
	call := &indexFlightCall{done: make(chan struct{})}
	f.calls[key] = call
	f.mu.Unlock()

	call.value, call.err = load()
	close(call.done)
	f.mu.Lock()
	delete(f.calls, key)
	f.mu.Unlock()
	return call.value, call.err
}
