package bufferbudget

// Cache entries participate in both process and account eviction order. Keeping
// their links inside the entry avoids two independent heap allocations per block.
type cacheLink struct {
	previous, next *cacheLink
	entry          evictionCandidate
}

type cacheList struct {
	first, last *cacheLink
}

func (l *cacheList) pushFront(link *cacheLink) {
	link.previous, link.next = nil, l.first
	if l.first != nil {
		l.first.previous = link
	} else {
		l.last = link
	}
	l.first = link
}

func (l *cacheList) remove(link *cacheLink) {
	if link.previous != nil {
		link.previous.next = link.next
	} else {
		l.first = link.next
	}
	if link.next != nil {
		link.next.previous = link.previous
	} else {
		l.last = link.previous
	}
	link.previous, link.next = nil, nil
}
