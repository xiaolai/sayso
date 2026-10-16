import Foundation
import XCTest

@testable import RealtimeConnection

/// The session below the fixtures: its write path, its unknown-type accounting, and opening.
final class SessionWriteTests: XCTestCase {
    private final class Log: @unchecked Sendable {
        private let lock = NSLock()
        private var entries: [String] = []
        func add(_ s: String) { lock.withLock { entries.append(s) } }
        var values: [String] { lock.withLock { entries } }
    }

    private func waitUntil(_ condition: () -> Bool, file: StaticString = #filePath, line: UInt = #line) async throws {
        let deadline = Date().addingTimeInterval(5)
        while !condition(), Date() < deadline { try await Task.sleep(nanoseconds: 2_000_000) }
        XCTAssertTrue(condition(), "the condition never held", file: file, line: line)
    }

    // MARK: The write queue

    func testCloseReleasesEveryQueuedWriter() async throws {
        let gate = WriteGate()
        try await gate.acquire()
        let outcomes = Log()
        let waiters = (0..<3).map { _ in
            Task {
                do {
                    try await gate.acquire()
                    outcomes.add("turn")
                } catch is RealtimeSession.Closed { outcomes.add("closed") } catch { outcomes.add("\(error)") }
            }
        }
        try await waitUntil { gate.waiting == 3 }
        gate.close()
        try await waitUntil { outcomes.values.count == 3 }  // bounded: a writer left queued would hang the test
        XCTAssertEqual(outcomes.values, ["closed", "closed", "closed"])
        for w in waiters { w.cancel() }
        let late = Task {
            do {
                try await gate.acquire()
                outcomes.add("late turn")
            } catch is RealtimeSession.Closed { outcomes.add("late closed") } catch { outcomes.add("late \(error)") }
        }
        try await waitUntil { outcomes.values.count == 4 }
        XCTAssertEqual(outcomes.values.last, "late closed", "a writer got the turn, or queued, after close")
        late.cancel()
        gate.release()
    }

    func testCancelledWaitersAreSkippedInOrder() async throws {
        let gate = WriteGate()
        try await gate.acquire()
        let order = Log()
        var tasks: [Task<Void, Error>] = []
        for name in ["a", "b", "c", "d"] {
            tasks.append(
                Task {
                    try await gate.acquire()
                    order.add(name)
                    gate.release()
                })
            try await waitUntil { gate.waiting == tasks.count }
        }
        tasks[1].cancel()
        tasks[2].cancel()
        try await waitUntil { gate.waiting == 2 }
        gate.release()
        try await waitUntil { order.values.count == 2 }  // bounded: a waiter left behind a cancelled one would hang
        XCTAssertEqual(order.values, ["a", "d"])
        for t in tasks { t.cancel() }
        try await gate.acquire()  // free again: the cancelled ids left nothing holding the turn
        gate.release()
    }
}
