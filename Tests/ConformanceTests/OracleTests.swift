#if os(macOS)
@testable import CodexAuth
import Foundation
import PCM16
@testable import RealtimeConnection
@testable import SaidGuard
import XCTest

/// Answers tools/differential.py from the Swift implementation, as go/internal/oracle does from Go's: it reads
/// `{"id", "op", "input"}` lines from the file `SAYSO_ORACLE_IN` names and writes one `{"id", "op", "output"}` line per
/// case to `SAYSO_ORACLE_OUT`. Without `SAYSO_ORACLE_IN` it runs its built-in checks through the same files instead, so
/// every test run exercises the oracle and none skips it. Each observation is built as the Go oracle builds it.
final class OracleTests: XCTestCase {
    /// Every operation answered here; each must have a built-in check.
    static let ops = [
        "said.matches", "said.tokens", "han.parse", "jwt.exp", "echo", "wire.valid", "codex.valid", "event.decode",
        "pcm.encode", "pcm.decode", "pcm.wav", "pcm.validWAV", "pcm.plausible",
    ]

    func testOracle() throws {
        let env = ProcessInfo.processInfo.environment
        guard let input = env["SAYSO_ORACLE_IN"], !input.isEmpty else { return try selfCheck() }
        guard let output = env["SAYSO_ORACLE_OUT"], !output.isEmpty else {
            return XCTFail("SAYSO_ORACLE_IN is set without SAYSO_ORACLE_OUT")
        }
        let n = try Self.answer(URL(fileURLWithPath: input), URL(fileURLWithPath: output))
        print("oracle: \(n) cases")
    }

    /// A case as the harness writes it. Read with JSONDecoder and written by ``json(_:)``, never through
    /// JSONSerialization: a String bridged from NSString loses a leading U+FEFF, which is input like any other.
    struct Case: Decodable {
        let id: String
        let op: String
        let input: [String: Value]
    }

    enum Value: Decodable {
        case text(String), integer(Int), list([Value])

        init(from decoder: Decoder) throws {
            let c = try decoder.singleValueContainer()
            if let s = try? c.decode(String.self) {
                self = .text(s)
            } else if let n = try? c.decode(Int.self) {
                self = .integer(n)
            } else {
                self = .list(try c.decode([Value].self))
            }
        }
    }

    /// Writes each case's line as soon as it is computed, so a case that crashes the process is the first one missing
    /// from `output`.
    static func answer(_ input: URL, _ output: URL) throws -> Int {
        let lines = try Data(contentsOf: input).split(separator: UInt8(ascii: "\n"))
        guard FileManager.default.createFile(atPath: output.path, contents: nil) else {
            throw FixtureError("cannot create \(output.path)")
        }
        let out = try FileHandle(forWritingTo: output)
        defer { try? out.close() }
        var n = 0
        for line in lines where !line.allSatisfy({ $0 == 0x20 || $0 == 0x0D }) {
            let c = try JSONDecoder().decode(Case.self, from: Data(line))
            let v = try observe(c.op, c.input)
            try out.write(contentsOf: Data((json(["id": c.id, "op": c.op, "output": v]) + "\n").utf8))
            n += 1
        }
        return n
    }

    /// The JSON text of an observation, scalar by scalar.
    static func json(_ v: Any) -> String {
        if let s = v as? String { return quoted(s) }
        if type(of: v) == Bool.self, let b = v as? Bool { return b ? "true" : "false" }
        if type(of: v) == Int.self, let n = v as? Int { return String(n) }
        if v is NSNull { return "null" }
        if let a = v as? [Any] { return "[" + a.map(json).joined(separator: ",") + "]" }
        if let o = v as? [String: Any] {
            return "{" + o.keys.sorted().map { quoted($0) + ":" + json(o[$0]!) }.joined(separator: ",") + "}"
        }
        preconditionFailure("an observation of type \(type(of: v))")
    }

    private static func quoted(_ s: String) -> String {
        var out = String.UnicodeScalarView()
        out.append("\"")
        for u in s.unicodeScalars {
            switch u {
            case "\"", "\\":
                out.append("\\")
                out.append(u)
            case "\u{0}"..."\u{1F}":
                out.append(contentsOf: String(format: "\\u%04x", u.value).unicodeScalars)
            default:
                out.append(u)
            }
        }
        out.append("\"")
        return String(out)
    }

    // MARK: operations

    static func observe(_ op: String, _ c: [String: Value]) throws -> Any {
        switch op {
        case "said.matches":
            return saidMatches(try text(c, "said"), try text(c, "spoken"), language: try text(c, "language"))
        case "said.tokens":
            return svTranscriptTokens(try text(c, "text"), try text(c, "language")).map {
                ["text": $0.text, "link": $0.link, "lead": $0.lead, "trail": $0.trail] as [String: Any]
            }
        case "han.parse":
            return parseHanNumeral(try text(c, "text")).map { String($0) } ?? NSNull()  // as text: beyond 2^53
        case "jwt.exp":
            return jwtObservation(try text(c, "token"))
        case "echo":
            return hex(Data(try text(c, "text").utf8))
        case "wire.valid":
            return WireJSON.isValid(try bytes(c))
        case "codex.valid":
            return isPlainJSON(try bytes(c))
        case "event.decode":
            do {
                return ["event": eventObservation(try RealtimeEvent.decode(try bytes(c)))]
            } catch let e as RealtimeError {
                return ["failure": errorObservation(e)]
            } catch let e as FixtureError {
                throw e
            } catch {
                return ["unclassified": "\(error)"]
            }
        case "pcm.encode":
            guard case .list(let bits)? = c["bits"] else { throw FixtureError("no bits") }
            let samples = try bits.map { b -> Float in
                guard case .integer(let n) = b, let u = UInt32(exactly: n) else { throw FixtureError("bad bits") }
                return Float(bitPattern: u)
            }
            return hex(PCM.encode(samples))
        case "pcm.decode":
            return PCM.decode(try bytes(c)).map { String(format: "%08x", $0.bitPattern) }
        case "pcm.wav":
            return (try? PCM.wav(try bytes(c), rate: try integer(c, "rate"))).map(hex) ?? NSNull()
        case "pcm.validWAV":
            return PCM.isValidWAV(try bytes(c), maxData: try integer(c, "max"))
        case "pcm.plausible":
            return PCM.isPlausible(try bytes(c), script: try text(c, "script"))
        default:
            throw FixtureError("op \(op) is not answered by the Swift oracle")
        }
    }

    /// As go/codexauth/oracle_test.go: exact below 2^52 s, beyond it only far in the future or the past, where a Date
    /// counted from 2001 in a Double and Go's time.Time both stop holding every exp as given.
    static func jwtObservation(_ token: String) -> Any {
        guard let d = jwtExpiry(token) else { return NSNull() }
        let s = d.timeIntervalSince1970
        let far = 4_503_599_627_370_496.0  // 2^52
        if s >= far { return "far-future" }
        if s <= -far { return "far-past" }
        return String(Int64(s))
    }

    static func errorObservation(_ e: RealtimeError) -> [String: Any] {
        ["class": e.errorClass.rawValue, "type": e.type, "code": e.code, "param": e.param, "message": e.message, "fatal": e.fatal]
    }

    static func eventObservation(_ ev: RealtimeEvent) -> [String: Any] {
        func metadata(_ m: [String: String]?) -> Any { m ?? NSNull() }
        let fields: [String: Any]
        switch ev {
        case .sessionCreated(let sessionType, let model):
            fields = ["sessionType": sessionType, "model": model]
        case .responseCreated(let id, let m):
            fields = ["responseID": id, "metadata": metadata(m)]
        case .audioDelta(let id, let pcm):
            fields = ["responseID": id, "pcm": hex(pcm)]
        case .transcriptDelta(let id, let delta):
            fields = ["responseID": id, "delta": delta]
        case .responseDone(let d):
            let u = d.usage
            let usage: [String: Any] = [
                "input": String(u.inputTokens), "output": String(u.outputTokens), "inputText": String(u.inputTextTokens),
                "outputText": String(u.outputTextTokens), "outputAudio": String(u.outputAudioTokens), "detailed": u.detailed,
            ]
            fields = [
                "responseID": d.responseID, "status": d.status.rawValue, "statusReason": d.statusReason,
                "metadata": metadata(d.metadata), "usage": usage,
            ]
        case .rateLimitsUpdated(let limits):
            fields = [
                "limits": limits.map {
                    [
                        "name": $0.name, "limit": String($0.limit), "remaining": String($0.remaining),
                        "resetSeconds": String(format: "%016llx", $0.resetSeconds.bitPattern),
                    ]
                }
            ]
        case .error(let e):
            fields = ["error": errorObservation(e)]
        case .inputCommitted(let item):
            fields = ["itemID": item]
        case .transcriptionDelta(let item, let delta):
            fields = ["itemID": item, "delta": delta]
        case .transcriptionCompleted(let item, let transcript):
            fields = ["itemID": item, "transcript": transcript]
        case .transcriptionFailed(let item, let e):
            fields = ["itemID": item, "error": errorObservation(e)]
        case .ignored:
            fields = ["ignored": true]
        case .unknown:
            fields = ["unknown": true]
        }
        return fields.merging(["type": ev.type]) { $1 }
    }

    private static func text(_ c: [String: Value], _ key: String) throws -> String {
        guard case .text(let s)? = c[key] else { throw FixtureError("no \(key)") }
        return s
    }

    private static func integer(_ c: [String: Value], _ key: String) throws -> Int {
        guard case .integer(let n)? = c[key] else { throw FixtureError("no \(key)") }
        return n
    }

    private static func bytes(_ c: [String: Value]) throws -> Data {
        let s = Array(try text(c, "hex").utf8)
        guard s.count % 2 == 0 else { throw FixtureError("odd hex") }
        var out = Data(capacity: s.count / 2)
        for i in stride(from: 0, to: s.count, by: 2) {
            guard let b = UInt8(String(decoding: s[i..<i + 2], as: UTF8.self), radix: 16) else { throw FixtureError("bad hex") }
            out.append(b)
        }
        return out
    }

    private static func hex(_ d: Data) -> String { d.map { String(format: "%02x", $0) }.joined() }

    // MARK: built-in checks

    /// The checks of the Go oracles (go/saidguard, go/codexauth and go/conformance oracle_test.go), run through the
    /// same files as a harness run and compared as canonical JSON.
    private func selfCheck() throws {
        let checks = Self.checks
        for op in Self.ops where !checks.contains(where: { $0.op == op }) { XCTFail("op \(op) has no built-in check") }
        let dir = FileManager.default.temporaryDirectory.appendingPathComponent("oracle-\(UUID().uuidString)")
        try FileManager.default.createDirectory(at: dir, withIntermediateDirectories: true)
        defer { try? FileManager.default.removeItem(at: dir) }
        let input = dir.appendingPathComponent("in.jsonl"), output = dir.appendingPathComponent("out.jsonl")
        let lines = checks.enumerated().map { #"{"id":"check-\#($0)","op":"\#($1.op)","input":\#($1.input)}"# }
        try Data((lines.joined(separator: "\n") + "\n").utf8).write(to: input)
        XCTAssertEqual(try Self.answer(input, output), checks.count)
        let got = try Data(contentsOf: output).split(separator: UInt8(ascii: "\n"))
        XCTAssertEqual(got.count, checks.count)
        func canonical(_ v: Any) throws -> String {
            String(decoding: try JSONSerialization.data(withJSONObject: [v], options: [.sortedKeys]), as: UTF8.self)
        }
        for (i, (c, line)) in zip(checks, got).enumerated() {
            let r = try JSONSerialization.jsonObject(with: Data(line)) as? [String: Any] ?? [:]
            let want = try JSONSerialization.jsonObject(with: Data(c.output.utf8), options: [.fragmentsAllowed])
            XCTAssertEqual(r["id"] as? String, "check-\(i)")
            XCTAssertEqual(try canonical(r["output"] ?? "missing"), try canonical(want), "check \(i): \(c.op) \(c.input)")
        }
        print("oracle: \(checks.count) built-in checks")
    }

    private static func hexOf(_ s: String) -> String { #"{"hex":"\#(hex(Data(s.utf8)))"}"# }

    private static let wav2 =
        "524946462600000057415645666d74201000000001000100c05d000080bb00000200100064617461020000000100"
    private static let jwtHeader = "eyJhbGciOiJub25lIiwidHlwIjoiSldUIn0."

    private static let checks: [(op: String, input: String, output: String)] = [
        ("said.matches", #"{"said":"Hello, world.","spoken":"hello world","language":"en"}"#, "true"),
        ("said.matches", #"{"said":"twenty one","spoken":"21","language":"en"}"#, "true"),
        ("said.matches", #"{"said":"21","spoken":"-21","language":"en"}"#, "false"),
        ("said.matches", #"{"said":"一百一","spoken":"110","language":"zh"}"#, "true"),
        (
            "said.tokens", #"{"text":"E-mail ‘me’, 1,2%","language":"en"}"#,
            #"[{"text":"e","link":true,"lead":0,"trail":0},{"text":"mail","link":false,"lead":0,"trail":0},"#
                + #"{"text":"me","link":false,"lead":1,"trail":1},{"text":"1","link":false,"lead":0,"trail":0},"#
                + #"{"text":"2","link":false,"lead":0,"trail":0},{"text":"%","link":false,"lead":0,"trail":0}]"#
        ),
        ("said.tokens", #"{"text":"","language":"zh"}"#, "[]"),
        ("han.parse", #"{"text":"两万五"}"#, #""25000""#),
        ("han.parse", #"{"text":"一千万亿"}"#, #""1000000000000000""#),
        ("han.parse", #"{"text":"三五"}"#, "null"),
        ("jwt.exp", #"{"token":"\#(jwtHeader)eyJleHAiOjE4MDAwMDM2MDB9.c2ln"}"#, #""1800003600""#),
        ("jwt.exp", #"{"token":"\#(jwtHeader)eyJleHAiOi02MjEzNTU5NjgwMH0.c2ln"}"#, #""-62135596800""#),
        ("jwt.exp", #"{"token":"\#(jwtHeader)eyJleHAiOjkyMjMzNzIwMzY4NTQ3NzU4MDd9.c2ln"}"#, #""far-future""#),
        ("jwt.exp", #"{"token":"\#(jwtHeader)eyJleHAiOi05MjIzMzcyMDM2ODU0Nzc1ODA4fQ.c2ln"}"#, #""far-past""#),
        ("jwt.exp", #"{"token":"\#(jwtHeader)eyJleHAiOjEuNX0.c2ln"}"#, "null"),
        ("jwt.exp", #"{"token":"\#(jwtHeader)eyJFWFAiOjV9.c2ln"}"#, "null"),
        ("jwt.exp", #"{"token":"not a jwt"}"#, "null"),
        ("echo", #"{"text":"a\u00e9\ud83d\ude00"}"#, #""61c3a9f09f9880""#),
        ("echo", #"{"text":"\ufeffa"}"#, #""efbbbf61""#),  // a leading U+FEFF survives the transport
        ("wire.valid", hexOf(#" {"a":[1,-0.5e3,"\ud83d\ude00"]} "#), "true"),
        ("wire.valid", hexOf(#"{"a":1,"a":2}"#), "false"),
        ("wire.valid", #"{"hex":"7b2261223a22ff227d"}"#, "false"),
        ("codex.valid", hexOf(#"{"n":1e308}"#), "true"),
        ("codex.valid", hexOf(#"{"n":1e309}"#), "false"),
        (
            "event.decode",
            hexOf(#"{"type":"response.done","response":{"id":"r","status":"completed","usage":{"input_tokens":9223372036854775807}}}"#),
            #"{"event":{"type":"response.done","responseID":"r","status":"completed","statusReason":"","metadata":null,"#
                + #""usage":{"input":"9223372036854775807","output":"0","inputText":"0","outputText":"0","outputAudio":"0","detailed":false}}}"#
        ),
        (
            "event.decode",
            hexOf(#"{"type":"rate_limits.updated","rate_limits":[{"name":"n","limit":1,"remaining":0,"reset_seconds":0.5}]}"#),
            #"{"event":{"type":"rate_limits.updated","limits":[{"name":"n","limit":"1","remaining":"0","resetSeconds":"3fe0000000000000"}]}}"#
        ),
        (
            "event.decode",
            hexOf(#"{"type":"error","error":{"type":"invalid_request_error","code":5,"param":"p","message":"m"}}"#),
            #"{"event":{"type":"error","error":{"class":"invalid-request","type":"invalid_request_error","code":"5","param":"p","message":"m","fatal":false}}}"#
        ),
        (
            "event.decode", hexOf(#"{"type":"response.output_audio.delta","response_id":"r","delta":"AAE="}"#),
            #"{"event":{"type":"response.output_audio.delta","responseID":"r","pcm":"0001"}}"#
        ),
        ("event.decode", hexOf(#"{"type":"session.updated","x":1e999}"#), #"{"event":{"type":"session.updated","ignored":true}}"#),
        (
            "event.decode", hexOf(#"{"type":"response.created","response":{"id":1}}"#),
            #"{"failure":{"class":"protocol","type":"","code":"","param":"","message":"realtime event response.created: a field has the wrong type","fatal":false}}"#
        ),
        (
            "event.decode", hexOf("["),
            #"{"failure":{"class":"protocol","type":"","code":"","param":"","message":"realtime event: not valid JSON","fatal":false}}"#
        ),
        ("pcm.encode", #"{"bits":[1065353216,3212836864,2143289344,1056964608]}"#, #""ff7f01800000ff3f""#),
        ("pcm.decode", #"{"hex":"0080ff7f01"}"#, #"["bf800000","3f7ffe00"]"#),
        ("pcm.wav", #"{"hex":"0100","rate":24000}"#, #""\#(wav2)""#),
        ("pcm.wav", #"{"hex":"","rate":0}"#, "null"),
        ("pcm.validWAV", #"{"hex":"\#(wav2)","max":2}"#, "true"),
        ("pcm.validWAV", #"{"hex":"\#(wav2)","max":1}"#, "false"),
        ("pcm.plausible", #"{"hex":"4801","script":"a"}"#, "false"),
        ("pcm.plausible", #"{"hex":"\#(String(repeating: "4801", count: 960))","script":"a"}"#, "true"),
    ]
}
#endif
