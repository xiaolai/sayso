import Foundation

/// **A value behind an `NSLock`**, for the small pieces of shared state the providers keep (a redirect that was
/// refused, a reader thread's accumulated output). The standard library's `Mutex` starts at macOS 15 / iOS 18; this
/// package builds for macOS 14 / iOS 17, so it uses Foundation's lock and says so once. `package` access: the CLI
/// target and the tests share it without a second implementation. `Value: Sendable` is the promise the `@unchecked`
/// stands in for: the value crosses threads, so what is inside must cross threads too.
package final class Locked<Value: Sendable>: @unchecked Sendable {
    private let lock = NSLock()
    private var value: Value

    package init(_ value: Value) {
        self.value = value
    }

    @discardableResult
    package func withLock<R>(_ body: (inout Value) throws -> R) rethrows -> R {
        lock.lock()
        defer { lock.unlock() }
        return try body(&value)
    }
}
