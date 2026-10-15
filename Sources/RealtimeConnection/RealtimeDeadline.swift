import Foundation

/// Runs a connection-level step under a deadline that is enforced by closing the transport (spec/session.md and the
/// plan's 3.4). On expiry, or on the caller's cancellation, it calls `close`, and once the operation unwinds it throws
/// ``Expired`` or `CancellationError`. It is correct only when `close` unblocks the operation, as closing a
/// WebSocket unblocks a pending receive. Completion, expiry and cancellation are arbitrated by one state change:
/// whichever comes first wins, so `close` runs at most once and never after the operation finished in time. `run`
/// returns only after the timer has stopped, so `close` never runs after `run` returns.
///
/// Use it for steps whose failure ends the connection anyway. A wait the session should survive (a model that is
/// slow to answer) needs its own timer, not this.
public enum RealtimeDeadline {
    /// The deadline expired before the operation finished.
    public struct Expired: Error, Sendable, Equatable {
        public init() {}
    }

    /// `seconds` at or below zero expires at once, before the operation runs; a value that is not finite, or beyond
    /// what a timer can represent (about 584 years), is refused as `internal`. The deadline is an instant fixed when
    /// `run` starts, so a late timer cannot extend it, and an operation that finishes after it is expired too.
    public static func run<T>(
        seconds: TimeInterval, close: @escaping @Sendable () -> Void, operation: () async throws -> T
    ) async throws -> T {
        guard seconds.isFinite, seconds <= 0 || Deadline.nanoseconds(seconds) != nil else {
            throw RealtimeError(.internal, message: "RealtimeDeadline needs a finite deadline")
        }
        let state = State()
        if seconds <= 0 {
            if state.settle(.expired) { close() }
            throw Expired()
        }
        let clock = ContinuousClock()
        let deadline = clock.now.advanced(by: .nanoseconds(Int64(Deadline.nanoseconds(seconds)!)))
        let timer = Task {
            do { try await Task.sleep(until: deadline, clock: clock) } catch { return }
            if state.settle(.expired) { close() }
        }
        let outcome: Result<T, Error>
        do {
            let value = try await withTaskCancellationHandler(
                operation: { () async throws -> T in
                    try Task.checkCancellation()
                    return try await operation()
                },
                onCancel: { if state.settle(.cancelled) { close() } })
            outcome = .success(value)
        } catch {
            outcome = .failure(error)
        }
        // Finished after the instant but before the timer ran: expired all the same.
        if clock.now >= deadline, state.settle(.expired) { close() }
        _ = state.settle(.finished)
        timer.cancel()
        await timer.value  // a close already under way completes before run returns
        switch state.outcome {
        case .expired: throw Expired()
        case .cancelled: throw CancellationError()
        default: return try outcome.get()
        }
    }

    private final class State: @unchecked Sendable {
        enum Outcome { case running, finished, expired, cancelled }
        private let lock = NSLock()
        private var value = Outcome.running

        var outcome: Outcome { lock.withLock { value } }

        /// True when this call ended the running state; only that caller may close.
        func settle(_ to: Outcome) -> Bool {
            lock.withLock {
                guard value == .running else { return false }
                value = to
                return true
            }
        }
    }
}
