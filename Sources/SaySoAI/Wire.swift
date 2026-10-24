import Foundation

/// **What went wrong while a response body was being read.** One small vocabulary shared by the provider and the
/// catalogue; each maps it into its own error type, because an answer's failure and a list's failure are worded
/// differently to the reader.
enum WireFailure: Error, Equatable, Sendable {
    /// The body passed the ceiling: only the caller knows whether that is a bad shape or a stream to cut short.
    case tooLarge
    case cancelled
    /// A failure below HTTP, named by its code so each caller keeps its own classification.
    case transport(URLError.Code)
    case unexpected
}

/// A body that was read, and whether the ceiling cut it. **A cut body can end in the middle of anything** — including a
/// key the server echoed back — so the caller that words a message from it must know.
struct WireBody: Sendable, Equatable {
    let data: Data
    let truncated: Bool
}

/// The two things every OpenAI-compatible request does the same way.
enum Wire {
    /// Put the key on the request the way the profile says. An absent or empty key sends no header at all — a local
    /// server is asked without one, not with an empty `Authorization`.
    static func authorize(_ request: inout URLRequest, auth: AIProviderAuth, key: String?) {
        guard let key, !key.isEmpty else { return }
        switch auth {
        case .bearer:
            request.setValue("Bearer \(key)", forHTTPHeaderField: "Authorization")
        case .header(let name):
            request.setValue(key, forHTTPHeaderField: name)
        case .none:
            break
        }
    }

    /// **Read a body with a ceiling.** `data(for:)` buffers whatever the server sends; a hostile or broken server could
    /// exhaust the app's memory. This streams, and one byte past `cap` cancels this transfer and only this one — never
    /// the session, which may be serving the caller's other work. `truncating` keeps what fits and cancels the rest
    /// (a refusal's sentence is worth at most a few hundred bytes); otherwise the overflow is `tooLarge`.
    static func collect(_ bytes: URLSession.AsyncBytes, cap: Int,
                        truncating: Bool = false) async throws(WireFailure) -> WireBody {
        var data = Data()
        data.reserveCapacity(min(64 << 10, cap))
        do {
            for try await byte in bytes {
                data.append(byte)
                if data.count > cap {
                    bytes.task.cancel()
                    if truncating { return WireBody(data: data, truncated: true) }
                    throw WireFailure.tooLarge
                }
            }
        } catch let failure as WireFailure {
            throw failure
        } catch is CancellationError {
            throw .cancelled
        } catch let error as URLError {
            throw .transport(error.code)
        } catch {
            throw .unexpected
        }
        return WireBody(data: data, truncated: false)
    }

    /// **A refusal's body, best-effort**: its status is already known, so a broken or oversized body loses its sentence,
    /// not the verdict — and `nil` is the one failure that must not be absorbed: the caller's cancellation.
    static func collectForMessage(_ bytes: URLSession.AsyncBytes, cap: Int) async -> WireBody? {
        do {
            return try await collect(bytes, cap: cap, truncating: true)
        } catch WireFailure.cancelled {
            return nil
        } catch WireFailure.transport(let code) where code == .cancelled {
            return nil
        } catch {
            return WireBody(data: Data(), truncated: false)
        }
    }
}
