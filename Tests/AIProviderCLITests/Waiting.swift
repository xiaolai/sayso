import Foundation

/// **One wait for the CLI suites**: a condition that is not main-actor isolated (a child process's exit, a staging
/// folder's state) is polled until its deadline, then asked once more.
func eventuallyOffMain(_ seconds: Double = 3, step: Duration = .milliseconds(20),
                       _ condition: () -> Bool) async -> Bool {
    let deadline = ContinuousClock.now + .seconds(seconds)
    while ContinuousClock.now < deadline {
        if condition() { return true }
        try? await Task.sleep(for: step)
    }
    return condition()
}
