/// A first-in, first-out queue that lets go of each element as it is taken, so what it holds is only what it still
/// queues: a consumed slot is emptied at once, and the emptied front is cut off once it is most of the storage, which
/// keeps taking O(1) amortised. Shared by this package's modules: CodexAuth's app-server queues and the realtime
/// session's waiters.
package struct FIFO<Element> {
    private var slots: [Element?] = []
    private var head = 0

    package init() {}

    package var count: Int { slots.count - head }

    package var first: Element? { head < slots.count ? slots[head] : nil }

    package mutating func append(_ e: Element) { slots.append(e) }

    package mutating func popFirst() -> Element? {
        guard head < slots.count else { return nil }
        let e = slots[head]
        slots[head] = nil
        head += 1
        if head == slots.count {
            slots = []
            head = 0
        } else if head > 1024 && head * 2 > slots.count {
            slots.removeFirst(head)
            head = 0
        }
        return e
    }

    package mutating func removeAll() {
        slots = []
        head = 0
    }

    /// Drops the elements `keep` refuses, keeping the order of the rest; O(count).
    package mutating func keepAll(where keep: (Element) -> Bool) {
        slots = slots[head...].filter { $0.map(keep) ?? false }
        head = 0
    }
}
