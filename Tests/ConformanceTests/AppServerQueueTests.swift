#if os(macOS)
@testable import CodexAuth
import Darwin
import Foundation
import XCTest

/// The queues the app-server client keeps, mirroring go/codexapp/outbound_test.go and lifecycle_test.go: what is
/// taken is let go at once, a failure drops what is queued, and the order is admission to the one outbound queue.
final class AppServerQueueTests: XCTestCase {
    private final class Payload {
        let n: Int
        init(_ n: Int) { self.n = n }
    }

    /// Each element is released the moment it is taken, not when the front is compacted later.
    func testFIFOReleasesEachElementWhenTaken() {
        var q = FIFO<Payload>()
        weak var taken: Payload?
        for i in 0..<3000 { q.append(Payload(i)) }
        for i in 0..<2000 {
            autoreleasepool {
                let p = q.popFirst()
                XCTAssertEqual(p?.n, i)
                taken = p
            }
            XCTAssertNil(taken, "element \(i) was kept after it was taken")
        }
        XCTAssertEqual(q.count, 1000)
        weak var last: Payload?
        autoreleasepool {
            let p = Payload(-1)
            last = p
            q.append(p)
        }
        q.removeAll()
        XCTAssertNil(last, "removeAll kept an element")
        XCTAssertEqual(q.count, 0)
        XCTAssertNil(q.popFirst())
    }

}
#endif
