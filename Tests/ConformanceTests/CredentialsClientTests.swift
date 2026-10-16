#if os(macOS)
import CodexAuth
import Darwin
import Foundation
import RealtimeConnection

/// The client cases of the credentials fixtures (`setup.client`, spec/fixtures.md): they drive the app-server client
/// itself, mirroring go/conformance/client_test.go.
extension CredentialWorld {
    /// The expectations checked here; the rest go to ``check(_:state:token:mode:info:elapsed:description:)``.
    private static let clientKeys: Set<String> = ["startFails", "calls", "events", "eventCount", "eventSequence"]

    func client(_ s: [String: Any], want: [String: Any]) throws -> [String] {
        let events = Box<[AppServerEvent]>([])
        let block = DispatchSemaphore(value: 0)
        defer { block.signal() }  // a blocked handler ends with the case
        var config = AppServerClient.Configuration(
            bin: binDir + "/codex", environment: self.cfg.environment ?? [:], timeout: self.cfg.timeout,
            guardStart: { [root] p in
                guard p.hasPrefix(root + "/") else { throw FixtureError("a path outside the case's temporary folder") }
            })
        switch s["handler"] as? String ?? "" {
        case "": break
        case "record": config.onEvent = { e in events.value.append(e) }
        case "block": config.onEvent = { _ in block.wait() }
        case let other: return ["unknown handler \(other)"]
        }
        let work = root + "/work"
        switch s["dir"] as? String ?? "" {
        case "": break
        case "work":
            try FileManager.default.createDirectory(atPath: work, withIntermediateDirectories: false)
            config.workingDirectory = work
        case "relative": config.workingDirectory = "work"
        case "missing": config.workingDirectory = root + "/missing"
        case "file":
            config.workingDirectory = root + "/file"
            FileManager.default.createFile(atPath: root + "/file", contents: Data())
        case let other: return ["unknown dir \(other)"]
        }

        var p: [String] = []
        var client: AppServerClient?
        do { client = try AppServerClient.start(config) } catch {
            if want["startFails"] == nil { p.append("start: \(error)") }
        }
        if let fails = want["startFails"] as? Bool, fails != (client == nil) {
            p.append("start failed: \(client == nil), want \(fails)")
        }
        var outcomes: [String] = []
        if let client {
            for call in s["calls"] as? [[String: Any]] ?? [] {
                let method = call["method"] as? String ?? ""
                do {
                    _ = try client.call(method, params: call["params"] as? [String: Any] ?? [:])
                    outcomes.append("ok")
                } catch { outcomes.append("error") }
            }
            // Events are delivered from a thread of their own: wait for the expected number, then a moment for any
            // that should not come.
            var n = (want["events"] as? [Any])?.count ?? 0
            if let count = (want["eventCount"] as? NSNumber)?.intValue { n = count }
            let deadline = Date().addingTimeInterval(5)
            while events.value.count < n, Date() < deadline { usleep(5000) }
            usleep(100_000)
            client.close()
        }
        let got = events.value

        let callerCwd = FileManager.default.currentDirectoryPath.withCString { c -> String in
            guard let r = realpath(c, nil) else { return String(cString: c) }
            defer { free(r) }
            return String(cString: r)
        }
        func expand(_ v: Any) -> Any {
            if let t = v as? String {
                return t.replacingOccurrences(of: "$work", with: work).replacingOccurrences(of: "$callerCwd", with: callerCwd)
            }
            if let o = v as? [String: Any] { return o.mapValues(expand) }
            if let a = v as? [Any] { return a.map(expand) }
            return v
        }
        var rest: [String: Any] = [:]
        for (key, v) in want {
            switch key {
            case "startFails": break
            case "calls":
                if !Self.same(outcomes, v) { p.append("calls \(outcomes), want \(v)") }
            case "events":
                let list = v as? [[String: Any]] ?? []
                guard got.count == list.count else {
                    p.append("\(got.count) events, want \(list.count): \(got.map(\.method).joined(separator: ","))")
                    break
                }
                for (i, (e, w)) in zip(got, list).enumerated() {
                    if e.method != w["method"] as? String { p.append("event \(i) is \(e.method), want \(w["method"] ?? "")") }
                    if e.isServerRequest != (w["serverRequest"] as? Bool ?? false) {
                        p.append("event \(i): server request \(e.isServerRequest)")
                    }
                    if w["noParams"] as? Bool == true, !e.params.isEmpty { p.append("event \(i) has params") }
                    if let wp = w["params"] {
                        let params = e.params.isEmpty
                            ? nil : try? JSONSerialization.jsonObject(with: e.params, options: [.fragmentsAllowed])
                        if params == nil || !Self.same(params!, expand(wp)) { p.append("event \(i) params differ") }
                    }
                }
            case "eventCount":
                if !Self.same(got.count, v) { p.append("\(got.count) events, want \(v)") }
            case "eventSequence":
                let field = v as? String ?? ""
                for (i, e) in got.enumerated() where !Self.same(e.paramsObject()?[field] ?? NSNull(), i) {
                    p.append("event \(i) out of order")
                    break
                }
            default:
                if !Self.clientKeys.contains(key) { rest[key] = v }
            }
        }
        return p + (try check(rest, state: "", token: nil, mode: "", info: CodexAuthInfo(), elapsed: 0, description: ""))
    }
}
#endif
