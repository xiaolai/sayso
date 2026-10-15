import Foundation

/// Runs a connection-level step under a deadline that is enforced by closing the transport (spec/session.md and the
/// plan's 3.4). On expiry, or on the caller's cancellation, it calls `close`, and once the operation unwinds it throws
/// ``Expired`` or `CancellationError`. It is correct only when `close` unblocks the operation, as closing a
/// WebSocket unblocks a pending receive. Completion and expiry are arbitrated under one lock: whichever settles first
/// wins, so a deadline that fires after the operation finished never closes a transport that is still in use.
///
/// Use it for steps whose failure ends the connection anyway. A wait the session should survive (a model that is
/// slow to answer) needs its own timer, not this.
public enum RealtimeDeadline {
    /// The deadline expired before the operation finished.
    public struct Expired: Error, Sendable, Equatable {
        public init() {}
    }

    public static func run<T>(
        seconds: TimeInterval, close: @escaping @Sendable () -> Void, operation: () async throws -> T
    ) async throws -> T {
        let state = State()
        let nanoseconds = UInt64(max(0, seconds) * 1_000_000_000)
        let timer = Task {
            do { try await Task.sleep(nanoseconds: nanoseconds) } catch { return }
            guard !Task.isCancelled, state.expire() else { return }
            close()
        }
        defer { timer.cancel() }
        return try await withTaskCancellationHandler(
            operation: {
                do {
                    try Task.checkCancellation()
                    let result = try await operation()
                    guard state.finish() else { throw Expired() }
                    try Task.checkCancellation()
                    return result
                } catch {
                    let inTime = state.finish()
                    if Task.isCancelled { throw CancellationError() }
                    if !inTime { throw Expired() }
                    throw error
                }
            }, onCancel: close)
    }

    private final class State: @unchecked Sendable {
        private enum Outcome { case running, finished, expired }
        private let lock = NSLock()
        private var outcome = Outcome.running

        /// True when this call expired the deadline; false when the operation had already finished.
        func expire() -> Bool {
            lock.lock()
            defer { lock.unlock() }
            guard outcome == .running else { return false }
            outcome = .expired
            return true
        }

        /// True when the operation finished in time; false when the deadline had already expired.
        func finish() -> Bool {
            lock.lock()
            defer { lock.unlock() }
            guard outcome == .running else { return outcome == .finished }
            outcome = .finished
            return true
        }
    }
}
