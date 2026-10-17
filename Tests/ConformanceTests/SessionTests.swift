import Foundation
@testable import RealtimeConnection
import XCTest

final class SessionTests: XCTestCase {
    func testSession() throws { try runFixtures("session") { c in FakeOpenAI.outcome(try SessionCheck.run(c.object)) } }
}

enum SessionCheck {
    /// Per kind, the case members it reads besides the head, `kind` and `expect`, and the expectation keys its check
    /// reads.
    static let schema: [String: (members: Set<String>, expect: Set<String>)] = [
        "close": (["code", "reason"], FakeOpenAI.errorKeys),
        "error-event": (["event"], FakeOpenAI.errorKeys),
        "size": (["readLimit", "messageBytes"], FakeOpenAI.errorKeys),
        "unknown-types": (["types", "typeBytes", "rounds"], FakeOpenAI.errorKeys.union(["delivered", "distinct"])),
        "handshake": (
            ["reply", "silent", "handshakeTimeoutMs"],
            ["class", "status", "requestId", "code", "withinMs", "request", "notInDescription"]
        ),
        "event": (
            ["raw"],
            [
                "type", "sessionType", "model", "responseId", "metadata", "pcmHex", "delta", "status", "statusReason",
                "usage", "limits", "exhausted", "class", "code", "param", "itemId", "transcript", "ignored", "unknown",
                "protocolError", "notInDescription",
            ]
        ),
    ]

    static func run(_ c: [String: Any]) throws -> [String] {
        let want = c["expect"] as? [String: Any] ?? [:]
        if let kind = c["kind"] as? String, let s = schema[kind] {
            let unknown =
                FakeOpenAI.unknownKeys("case", c, FakeOpenAI.caseHead.union(["kind", "expect"]).union(s.members))
                + FakeOpenAI.unknownKeys("expect", want, s.expect)
            guard unknown.isEmpty else { return unknown }
        }
        switch c["kind"] as? String {
        case "close":
            // The session classifies a received close frame through closeError, as here.
            let error = RealtimeSession.closeError(code: c["code"] as? Int ?? 0, reason: c["reason"] as? String ?? "")
            return FakeOpenAI.checkError(error, want)
        case "error-event":
            let data = try JSONSerialization.data(withJSONObject: c["event"] ?? NSNull(), options: [.fragmentsAllowed])
            guard case .error(let e) = try RealtimeEvent.decode(data) else { return ["not an error event"] }
            return FakeOpenAI.checkError(e, want)
        case "size":
            return try size(readLimit: c["readLimit"] as? Int ?? 0, messageBytes: c["messageBytes"] as? Int ?? 0, want)
        case "event":
            guard let raw = c["raw"] as? String else { return ["event case without raw"] }
            return event(raw, want)
        case "unknown-types":
            return try unknownTypes(
                types: c["types"] as? Int ?? 0, size: c["typeBytes"] as? Int ?? 0, rounds: c["rounds"] as? Int ?? 0, want)
        case "handshake":
            return try handshake(c, want)
        default:
            return ["unknown kind \(c["kind"] ?? "nil")"]
        }
    }

    /// The reply as both harnesses write it: the status line, the headers in name order, a Content-Length when there
    /// is a body, and Connection: close unless the reply is held.
    static func replyBytes(_ r: [String: Any]) -> Data {
        var head = "HTTP/1.1 \(r["status"] as? Int ?? 0) Fixture\r\n"
        let headers = r["headers"] as? [String: String] ?? [:]
        for name in headers.keys.sorted(by: { $0.utf8.lexicographicallyPrecedes($1.utf8) }) {
            head += "\(name): \(headers[name]!)\r\n"
        }
        let body = r["body"] as? String
        if let body { head += "Content-Length: \(body.utf8.count)\r\n" }
        if r["hold"] as? Bool != true { head += "Connection: close\r\n" }
        return Data((head + "\r\n" + (body ?? "")).utf8)
    }

    /// Opens a session against a fake server that reads the upgrade request and answers it with the case's reply,
    /// or never (silent), and compares the refusal and the request with the expectation.
    static func handshake(_ c: [String: Any], _ want: [String: Any]) throws -> [String] {
        let reply = c["reply"] as? [String: Any]
        guard (reply == nil) == (c["silent"] as? Bool == true) else {
            return ["a handshake case has either a reply or silent: true"]
        }
        let server = try FakeHandshakeServer(reply: reply.map(replyBytes), hold: reply?["hold"] as? Bool == true)
        defer { server.stop() }
        let client = try FakeOpenAI.client(port: server.port)
        let configuration = RealtimeSession.Configuration(
            url: RealtimeSession.url(model: "m", base: URL(string: "ws://127.0.0.1:\(server.port)")!),
            handshakeTimeout: Double(c["handshakeTimeoutMs"] as? Int ?? 5000) / 1000, readLimit: 1 << 20)
        let secret = ClientSecret(bearer: "ek_conformance", expiresAt: Date(timeIntervalSince1970: 1_800_000_000))
        let start = Date()
        let result = FakeOpenAI.blocking { () async throws -> Bool in
            let session = try await RealtimeSession.open(client: client, secret: secret, configuration: configuration)
            await session.close()
            return true
        }
        let elapsed = Date().timeIntervalSince(start)
        guard case .failure(let error) = result else { return ["the handshake opened a session"] }
        guard let e = error as? RealtimeError else { return ["not a classified error: \(error)"] }
        var p: [String] = []
        let fields: [(String, Any)] = [
            ("class", e.errorClass.rawValue), ("status", e.status), ("requestId", e.requestID), ("code", e.code),
        ]
        for (key, got) in fields {
            if let w = want[key], !FakeOpenAI.same(got, w) { p.append("\(key) \(got), want \(w)") }
        }
        if let ms = want["withinMs"] as? Int, elapsed > Double(ms) / 1000 { p.append("took \(elapsed) s, limit \(ms) ms") }
        if let w = want["request"] as? [String: Any] {
            let got: [String: Any] = ["target": server.target ?? "", "authorization": server.authorization ?? ""]
            for (key, v) in w where !FakeOpenAI.same(got[key], v) { p.append("request \(key) \(got[key] ?? ""), want \(v)") }
        }
        var description = ""
        dump(e, to: &description)
        return p + FakeOpenAI.leaks(FakeOpenAI.strings(want["notInDescription"]), "\(e) \(description)")
    }

    /// The probe message of exactly `bytes` bytes, and the length of its delta.
    static func probe(bytes: Int) -> (Data, Int)? {
        let prefix = #"{"type":"response.output_audio_transcript.delta","response_id":"r","delta":""#, suffix = #""}"#
        let pad = bytes - prefix.utf8.count - suffix.utf8.count
        guard pad >= 0 else { return nil }
        return (Data((prefix + String(repeating: "a", count: pad) + suffix).utf8), pad)
    }

    /// Sends one message of `messageBytes` over a real WebSocket to a session with `readLimit`.
    static func size(readLimit: Int, messageBytes: Int, _ want: [String: Any]) throws -> [String] {
        guard let (message, pad) = probe(bytes: messageBytes) else { return ["message size too small for the probe event"] }
        let server = try FakeWebSocketServer(message: message)
        defer { server.stop() }
        let client = try FakeOpenAI.client(port: server.port)
        let configuration = RealtimeSession.Configuration(
            url: RealtimeSession.url(model: "m", base: URL(string: "ws://127.0.0.1:\(server.port)")!),
            handshakeTimeout: 5, readLimit: readLimit)
        let secret = ClientSecret(bearer: "ek_conformance", expiresAt: Date(timeIntervalSince1970: 1_800_000_000))
        let result = FakeOpenAI.blocking { () async throws -> RealtimeMessage in
            let session = try await RealtimeSession.open(client: client, secret: secret, configuration: configuration)
            do {
                let message = try await session.next()
                await session.close()
                return message
            } catch {
                await session.close()
                throw error
            }
        }
        if want["class"] == nil || want["class"] is NSNull {
            guard case .success(let m) = result, case .transcriptDelta(_, let delta) = m.event, delta.utf8.count == pad
            else { return ["message at the limit: \(result)"] }
            return []
        }
        if case .failure(let error) = result { return FakeOpenAI.checkError(error, want) }
        return ["a message over the limit was delivered"]
    }

    /// The fixtures' unknown type `i`: "drift.<i>." padded with "a" to `size` bytes.
    static func driftType(_ i: Int, size: Int) -> String {
        let head = "drift.\(i)."
        return head + String(repeating: "a", count: max(0, size - head.utf8.count))
    }

    /// Sends `rounds` rounds of `types` distinct unknown types and compares what is delivered and counted, and the
    /// error that ends the session, if any, with the budget.
    static func unknownTypes(types: Int, size: Int, rounds: Int, _ want: [String: Any]) throws -> [String] {
        let messages = (0..<rounds).flatMap { _ in
            (0..<types).map { Data(#"{"type":"\#(driftType($0, size: size))"}"#.utf8) }
        }
        let server = try FakeWebSocketServer(messages: messages)
        defer { server.stop() }
        let client = try FakeOpenAI.client(port: server.port)
        let configuration = RealtimeSession.Configuration(
            url: RealtimeSession.url(model: "m", base: URL(string: "ws://127.0.0.1:\(server.port)")!),
            handshakeTimeout: 5, readLimit: 1 << 20)
        let secret = ClientSecret(bearer: "ek_conformance", expiresAt: Date(timeIntervalSince1970: 1_800_000_000))
        let total = types * rounds
        let result = FakeOpenAI.blocking { () async throws -> (Int, Error?, [UnknownTypeCount]) in
            let session = try await RealtimeSession.open(client: client, secret: secret, configuration: configuration)
            var delivered = 0
            var failure: Error?
            while delivered < total {
                do {
                    _ = try await session.next()
                    delivered += 1
                } catch {
                    failure = error
                    break
                }
            }
            let counts = session.unknownCounts
            await session.close()
            return (delivered, failure, counts)
        }
        guard case .success(let (delivered, failure, counts)) = result else { return ["open: \(result)"] }
        var p: [String] = []
        if !FakeOpenAI.same(delivered, want["delivered"] ?? NSNull()) { p.append("delivered \(delivered), want \(want["delivered"] ?? "nil")") }
        if !FakeOpenAI.same(counts.count, want["distinct"] ?? NSNull()) { p.append("counted \(counts.count) types, want \(want["distinct"] ?? "nil")") }
        if want["class"] == nil || want["class"] is NSNull {
            if let failure { p.append("ended: \(failure)") }
            if let off = counts.first(where: { $0.count != rounds }) { p.append("\(off.type) counted \(off.count) times, want \(rounds)") }
            return p
        }
        guard let failure else { return p + ["the session did not end past the budget"] }
        return p + FakeOpenAI.checkError(failure, want)
    }

    /// Flattens a decoded event into the fixture's expectation keys.
    static func observe(_ event: RealtimeEvent) -> [String: Any] {
        var m: [String: Any] = ["type": event.type]
        func meta(_ v: [String: String]?) -> Any { v.map { $0 as Any } ?? NSNull() }
        switch event {
        case .sessionCreated(let sessionType, let model): (m["sessionType"], m["model"]) = (sessionType, model)
        case .responseCreated(let id, let metadata): (m["responseId"], m["metadata"]) = (id, meta(metadata))
        case .audioDelta(let id, let pcm):
            (m["responseId"], m["pcmHex"]) = (id, pcm.map { String(format: "%02x", $0) }.joined())
        case .transcriptDelta(let id, let delta): (m["responseId"], m["delta"]) = (id, delta)
        case .responseDone(let d):
            (m["responseId"], m["status"], m["statusReason"], m["metadata"]) =
                (d.responseID, d.status.rawValue, d.statusReason, meta(d.metadata))
            m["usage"] = [
                "inputTokens": d.usage.inputTokens, "outputTokens": d.usage.outputTokens,
                "inputTextTokens": d.usage.inputTextTokens, "outputTextTokens": d.usage.outputTextTokens,
                "outputAudioTokens": d.usage.outputAudioTokens,
            ]
        case .rateLimitsUpdated(let limits):
            m["limits"] = limits.map {
                ["name": $0.name, "limit": $0.limit, "remaining": $0.remaining, "resetSeconds": $0.resetSeconds] as [String: Any]
            }
            m["exhausted"] = RealtimeEvent.exhausted(limits) != nil
        case .error(let e): (m["class"], m["code"], m["param"]) = (e.errorClass.rawValue, e.code, e.param)
        case .inputCommitted(let item): m["itemId"] = item
        case .transcriptionDelta(let item, let delta): (m["itemId"], m["delta"]) = (item, delta)
        case .transcriptionCompleted(let item, let transcript): (m["itemId"], m["transcript"]) = (item, transcript)
        case .transcriptionFailed(let item, let e): (m["itemId"], m["class"]) = (item, e.errorClass.rawValue)
        case .ignored(let type): m = ["ignored": type]
        case .unknown(let type): m = ["unknown": type]
        }
        return m
    }

    /// Decodes `raw` and compares it with `want`: the typed fields, or a protocol error naming the type
    /// (protocolError null: the type need not be named).
    static func event(_ raw: String, _ want: [String: Any]) -> [String] {
        let result = Result { try RealtimeEvent.decode(Data(raw.utf8)) }
        if let pe = want["protocolError"] {
            guard case .failure(let error) = result else { return ["decoded \(result), want a protocol error"] }
            var p: [String] = []
            let e = error as? RealtimeError
            if e?.errorClass != .protocol { p.append("class \(String(describing: e?.errorClass)), want protocol") }
            let text = String(describing: error)
            if let type = pe as? String, !text.contains(type) { p.append("error \"\(text)\" does not name \(type)") }
            if e?.eventType != (pe as? String ?? "") { p.append("eventType \(String(describing: e?.eventType)), want \(pe)") }
            return p + FakeOpenAI.leaks(FakeOpenAI.strings(want["notInDescription"]), text)
        }
        let event: RealtimeEvent
        switch result {
        case .failure(let error): return ["decode: \(error)"]
        case .success(let e): event = e
        }
        let got = observe(event)
        var p: [String] = []
        for (key, w) in want where key != "notInDescription" {
            if !FakeOpenAI.same(got[key], w) { p.append("\(key) = \(FakeOpenAI.normal(got[key])), want \(w)") }
        }
        return p
    }
}

/// The session beyond the fixtures: a message far over the limit, a refused handshake, delivery of unknown events
/// with their bytes, cancellation of a wait, and close.
final class SessionBehaviourTests: XCTestCase {
    private func open(_ server: FakeWebSocketServer, readLimit: Int) async throws -> RealtimeSession {
        let client = try FakeOpenAI.client(port: server.port)
        return try await RealtimeSession.open(
            client: client, secret: ClientSecret(bearer: "ek_behaviour", expiresAt: Date()),
            configuration: RealtimeSession.Configuration(
                url: RealtimeSession.url(model: "m", base: URL(string: "ws://127.0.0.1:\(server.port)")!),
                handshakeTimeout: 5, readLimit: readLimit))
    }

    func testFarOverTheLimitIsProtocol() async throws {
        let (message, _) = try XCTUnwrap(SessionCheck.probe(bytes: 300_000))
        let server = try FakeWebSocketServer(message: message)
        defer { server.stop() }
        let session = try await open(server, readLimit: 1000)
        do {
            _ = try await session.next()
            XCTFail("a message over the limit was delivered")
        } catch let e as RealtimeError {
            XCTAssertEqual(e.errorClass, .protocol, "\(e)")
            XCTAssertEqual(e.code, "1009")
        }
        do {
            _ = try await session.next()
            XCTFail("the session continued")
        } catch let e as RealtimeError {
            XCTAssertEqual(e.errorClass, .protocol, "the session stays ended with the same error")
        }
        await session.close()
    }

    func testUnknownEventsArriveWithTheirBytes() async throws {
        let raw = Data(#"{"type":"response.function_call_arguments.done","call_id":"c","arguments":"{}"}"#.utf8)
        let server = try FakeWebSocketServer(message: raw)
        defer { server.stop() }
        let session = try await open(server, readLimit: 1 << 20)
        let m = try await session.next()
        XCTAssertEqual(m.event, .unknown(type: "response.function_call_arguments.done"))
        XCTAssertEqual(m.raw, raw)
        XCTAssertEqual(session.unknownCounts, [UnknownTypeCount(type: "response.function_call_arguments.done", count: 1)])
        await session.close()
    }

    func testCancellingAWaitKeepsTheSession() async throws {
        // The server sends one event; a cancelled wait before it is read must not lose it.
        let raw = Data(#"{"type":"input_audio_buffer.committed","item_id":"i"}"#.utf8)
        let server = try FakeWebSocketServer(message: raw)
        defer { server.stop() }
        let session = try await open(server, readLimit: 1 << 20)
        // The message is buffered first, so the cancelled wait has one to steal; and the wait is cancelled before it
        // calls next(), so its outcome does not depend on timing.
        let buffered = await eventually { session.holdsBufferedMessage }
        XCTAssertTrue(buffered, "the message never arrived")
        let waiter = Task {
            withUnsafeCurrentTask { $0?.cancel() }
            return try await session.next()
        }
        do {
            _ = try await waiter.value
            XCTFail("a cancelled wait returned a message")
        } catch is CancellationError {}
        // Bounded: a lost message would otherwise leave this wait hanging.
        let m = try await RealtimeDeadline.run(seconds: 5, close: { Task { await session.close() } }) {
            try await session.next()
        }
        XCTAssertEqual(m.event, .inputCommitted(itemID: "i"))
        await session.close()
        do {
            _ = try await session.next()
            XCTFail("next after close")
        } catch is RealtimeSession.Closed {}
        do {
            try await session.send(Data("{}".utf8))
            XCTFail("send after close")
        } catch is RealtimeSession.Closed {}
    }

    func testRefusedHandshakeIsClassifiedLikeHTTP() async throws {
        let server = try FakeHTTPServer(replies: [
            FakeHTTPReply(status: 401, headers: [("x-request-id", "req_ws")], body: Data())
        ])
        let port = try server.start()
        defer { server.stop() }
        let client = try FakeOpenAI.client(port: port)
        do {
            _ = try await RealtimeSession.open(
                client: client, secret: ClientSecret(bearer: "ek_refused", expiresAt: Date()),
                configuration: RealtimeSession.Configuration(
                    url: RealtimeSession.url(model: "m", base: URL(string: "ws://127.0.0.1:\(port)")!),
                    handshakeTimeout: 5, readLimit: 1000))
            XCTFail("a refused handshake opened")
        } catch let e as RealtimeError {
            XCTAssertEqual(e.errorClass, .authExpired, "\(e)")
            XCTAssertEqual(e.status, 401)
        }
        XCTAssertEqual(server.requests.first?.headers["authorization"], "Bearer ek_refused")
    }

    func testOpenRefusesTheWrongOrigin() async throws {
        let client = HTTPClient()
        do {
            _ = try await RealtimeSession.open(
                client: client, secret: ClientSecret(bearer: "ek_x", expiresAt: Date()),
                configuration: RealtimeSession.Configuration(
                    url: URL(string: "wss://elsewhere.invalid/v1/realtime")!, handshakeTimeout: 5, readLimit: 10))
            XCTFail("opened")
        } catch let e as RealtimeError {
            XCTAssertEqual(e.errorClass, .internal)
        }
    }

    func testDeadlineClosesOnExpiry() async throws {
        let closed = FakeOpenAI.Flag()
        do {
            _ = try await RealtimeDeadline.run(seconds: 0.05, close: { closed.set() }) {
                // Stands for a receive that close unblocks.
                while !closed.value { try await Task.sleep(nanoseconds: 5_000_000) }
                return 1
            }
            XCTFail("the deadline did not expire")
        } catch is RealtimeDeadline.Expired {}
        XCTAssertTrue(closed.value)
        let untouched = FakeOpenAI.Flag()
        let value = try await RealtimeDeadline.run(seconds: 5, close: { untouched.set() }) { 7 }
        XCTAssertEqual(value, 7)
        XCTAssertFalse(untouched.value)
    }
}
