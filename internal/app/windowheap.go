package app

import "container/heap"

// The two orders an attemptLimiter keeps its windows in, and the index of
// each in attemptWindow.at.
const (
	byStartOrder = 0
	byCountOrder = 1
)

// windowHeap is a heap of attempt windows in one of the two orders. Each
// window records its position in it, so that one whose count changed is put
// in place, and one that is forgotten taken out, in logarithmic time.
type windowHeap struct {
	order   int
	windows []*attemptWindow
}

// top returns the first window in the heap's order, or nil.
func (h *windowHeap) top() *attemptWindow {
	if len(h.windows) == 0 {
		return nil
	}
	return h.windows[0]
}

func (h *windowHeap) push(w *attemptWindow) { heap.Push(h, w) }

func (h *windowHeap) fix(w *attemptWindow) { heap.Fix(h, w.at[h.order]) }

func (h *windowHeap) remove(w *attemptWindow) { heap.Remove(h, w.at[h.order]) }

// Len implements heap.Interface.
func (h *windowHeap) Len() int { return len(h.windows) }

// Less implements heap.Interface.
func (h *windowHeap) Less(i, j int) bool {
	a, b := h.windows[i], h.windows[j]
	if h.order == byCountOrder && a.count != b.count {
		return a.count < b.count
	}
	return a.since.Before(b.since)
}

// Swap implements heap.Interface.
func (h *windowHeap) Swap(i, j int) {
	h.windows[i], h.windows[j] = h.windows[j], h.windows[i]
	h.windows[i].at[h.order] = i
	h.windows[j].at[h.order] = j
}

// Push implements heap.Interface.
func (h *windowHeap) Push(x any) {
	w, ok := x.(*attemptWindow)
	if !ok {
		return
	}
	w.at[h.order] = len(h.windows)
	h.windows = append(h.windows, w)
}

// Pop implements heap.Interface.
func (h *windowHeap) Pop() any {
	last := len(h.windows) - 1
	w := h.windows[last]
	h.windows[last] = nil
	h.windows = h.windows[:last]
	w.at[h.order] = -1
	return w
}
