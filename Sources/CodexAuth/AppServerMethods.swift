#if os(macOS)
import Darwin
import Foundation
import RealtimeConnection

/// The methods the credentials call, each read with exact keys: a field of the wrong type is an error, as in Go.
extension AppServerClient {
    /// getAuthStatus. Any token in the reply is registered with the scrubber before it is returned.
    public func getAuthStatus(includeToken: Bool, refreshToken: Bool) throws -> AuthStatus {
        let result = try call("getAuthStatus", params: ["includeToken": includeToken, "refreshToken": refreshToken])
        let bad = AppServerError("Codex app-server sent an unexpected getAuthStatus result")
        guard let o = jsonObject(result) else { throw bad }
        guard let token = jsonOptionalString(o["authToken"]) else { throw bad }
        Secret.register(token ?? "")  // before anything else can fail or format it
        guard let method = jsonOptionalString(o["authMethod"]) else { throw bad }
        var requires = true
        if let r = o["requiresOpenaiAuth"], !(r is NSNull) {
            guard let b = jsonBool(r) else { throw bad }
            requires = b
        }
        return AuthStatus(method: method, requiresOpenAIAuth: requires, token: token ?? "")
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
