#if os(macOS)
import Darwin
import Foundation
import RealtimeConnection

/// The methods the credentials call, each read with exact keys: a field of the wrong type is an error, as in Go.
extension AppServerClient {
    /// getAuthStatus. Any token in the reply is registered with the scrubber before it is returned.
    ///
    /// The result is read where it lies (``WireJSON/Borrowed``), null being an empty object, and every field is
    /// checked before a `String` is made, so a refused result leaves no copy of the token; the token is decoded once,
    /// into the `String` kept.
    public func getAuthStatus(includeToken: Bool, refreshToken: Bool) throws -> AuthStatus {
        let bad = AppServerError("Codex app-server sent an unexpected getAuthStatus result")
        return try call("getAuthStatus", params: ["includeToken": includeToken, "refreshToken": refreshToken]) { raw in
            guard let doc = plainJSON(raw) else { throw bad }
            let root = doc.root
            if doc.isNull(root) { return AuthStatus(method: nil, requiresOpenAIAuth: true, token: "") }
            guard doc.kind(root) == UInt8(ascii: "{") else { throw bad }
            func present(_ name: String) -> Range<Int>? {  // absent or null is not present
                guard let v = doc.member(root, name), !doc.isNull(v) else { return nil }
                return v
            }
            let token = present("authToken"), method = present("authMethod"), requires = present("requiresOpenaiAuth")
            let isString = { (v: Range<Int>) in doc.kind(v) == UInt8(ascii: "\"") }
            guard token.map(isString) ?? true, method.map(isString) ?? true,
                requires.map({ doc.kind($0) == UInt8(ascii: "t") || doc.kind($0) == UInt8(ascii: "f") }) ?? true
            else { throw bad }
            return AuthStatus(
                method: method.flatMap { doc.string($0) },
                requiresOpenAIAuth: requires.map { doc.kind($0) == UInt8(ascii: "t") } ?? true,
                token: token.flatMap { doc.string($0) } ?? "")  // registered by AuthStatus before anything else
        }
    }

    /// config/read's effective `cli_auth_credentials_store`; "" when absent, which Codex treats as "file".
    public func credentialsStore() throws -> String {
        let result = try call("config/read", params: ["includeLayers": false])
        let bad = AppServerError("Codex app-server sent an unexpected config/read result")
        guard let o = jsonObject(result) else { throw bad }
        var config: [String: Any] = [:]
        if let c = o["config"], !(c is NSNull) {
            guard let c = c as? [String: Any] else { throw bad }
            config = c
        }
        guard let store = jsonOptionalString(config["cli_auth_credentials_store"]) else { throw bad }
        return Secret.scrub(store ?? "")
    }

    /// account/read's `account.planType`, "" when absent or null. Only this field is read; a present field of the
    /// wrong type is an error, as in Go. account/read may reach the network.
    public func planType() throws -> String {
        let result = try call("account/read")
        let bad = AppServerError("Codex app-server sent an unexpected account/read result")
        guard let o = jsonObject(result) else { throw bad }
        guard let a = o["account"], !(a is NSNull) else { return "" }
        guard let account = a as? [String: Any], let plan = jsonOptionalString(account["planType"]) else { throw bad }
        return Secret.scrub(plan ?? "")
    }
}
#endif
