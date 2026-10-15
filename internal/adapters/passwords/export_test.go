package passwords

// HoldConceal takes every slot of the pool of Conceal, as a flood of calls
// would, and returns what gives them back.
func HoldConceal(h *Hasher) func() {
	for range cap(h.concealSlots) {
		h.concealSlots <- struct{}{}
	}
	return func() {
		for range cap(h.concealSlots) {
			<-h.concealSlots
		}
	}
}
