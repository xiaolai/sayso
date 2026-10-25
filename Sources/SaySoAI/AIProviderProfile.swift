import Foundation

/// **How a bearer goes on the wire**: most services take `Authorization: Bearer <key>`; Azure takes `api-key`; a local
/// server takes none.
public enum AIProviderAuth: Sendable, Equatable {
    case bearer
    case header(String)
    case none
}

/// **Which token-limit field to send first**: OpenAI's current models prefer `max_completion_tokens`; most other
/// compatible services document `max_tokens`. The provider retries once with the other on a 400 or 422, so this is a
/// preference, not a contract.
public enum AIProviderTokenField: Sendable, Equatable {
    case completionFirst
    case maxTokensFirst
}

/// **Whether the request carries a temperature**: `0` where the service takes it; none where its models refuse one
/// (Anthropic's current models reject `temperature: 0`), so the server's default applies.
public enum AIProviderTemperature: Sendable, Equatable {
    case zero
    case serverDefault
}

/// **What a model list can be filtered to**: some services mix chat models with embeddings, speech and images; others
/// say in the list itself which models take text.
public enum AIProviderModelFilter: Sendable, Equatable {
    case all
    /// Keep entries whose `architecture.output_modalities` contains `text` (OpenRouter) or that are otherwise known to
    /// answer text; a list with no such metadata keeps everything.
    case textOutput
}

/// **One service on the OpenAI-compatible wire, as data.** A profile is a value: a caller picks one, copies it with
/// any field changed, and builds the provider from it; adding a service is a row here, never code. A profile with no
/// `modelsPath` cannot list its models (Azure's deployments are per resource), and says so through the catalogue.
public struct AIProviderProfile: Sendable, Equatable, Identifiable {
    public let id: String
    public let name: String
    /// The base address `chat/completions` is appended to. Nil for `.custom`, where the caller supplies it.
    public let baseURL: URL?
    public let auth: AIProviderAuth
    public let keyRequired: Bool
    public let tokenField: AIProviderTokenField
    public let temperature: AIProviderTemperature
    /// The path appended to the base for `GET`: `models` for nearly every service; nil when it lists none.
    public let modelsPath: String?
    /// Whether the list request **requires** the key (a cloud's list usually does; a local server's is unauthenticated).
    public let modelsNeedKey: Bool
    /// Whether the list request carries the key **when one is present**. A custom endpoint may take one without
    /// requiring it; a public list (OpenRouter's) is refused the key even when one is saved for completion.
    public let modelsSendKey: Bool
    public let modelFilter: AIProviderModelFilter
    /// A few names worth offering before anything is fetched (offline, or before a key is saved).
    public let hints: [String]
    /// What a reader should know before choosing it — an unsupported layer, a required deployment name.
    public let note: String?

    public init(id: String, name: String, baseURL: URL?, auth: AIProviderAuth, keyRequired: Bool,
                tokenField: AIProviderTokenField, modelsPath: String?, modelsNeedKey: Bool, modelsSendKey: Bool,
                modelFilter: AIProviderModelFilter, hints: [String] = [], note: String? = nil,
                temperature: AIProviderTemperature = .zero) {
        self.id = id
        self.name = name
        self.baseURL = baseURL
        self.auth = auth
        self.keyRequired = keyRequired
        self.tokenField = tokenField
        self.temperature = temperature
        self.modelsPath = modelsPath
        self.modelsNeedKey = modelsNeedKey
        self.modelsSendKey = modelsSendKey
        self.modelFilter = modelFilter
        self.hints = hints
        self.note = note
    }

    /// The free-form row: no default base address (the caller supplies one); a key is optional and sent when saved.
    public static let custom = AIProviderProfile(
        id: "custom", name: "Custom (OpenAI-compatible)", baseURL: nil, auth: .bearer, keyRequired: false,
        tokenField: .completionFirst, modelsPath: "models", modelsNeedKey: false, modelsSendKey: true,
        modelFilter: .all, hints: [], note: "The address is yours; a key is sent only if you save one.")

    /// **The services measured on 2026-10-25.** Base addresses and auth are the providers' own documentation; the token
    /// preference is the field each documents first; hints are the models each listed on that date (the catalogue
    /// refreshes them).
    public static let known: [AIProviderProfile] = [
        custom,
        AIProviderProfile(id: "openai", name: "OpenAI", baseURL: URL(string: "https://api.openai.com/v1"),
                          auth: .bearer, keyRequired: true, tokenField: .completionFirst, modelsPath: "models",
                          modelsNeedKey: true, modelsSendKey: true, modelFilter: .all,
                          hints: ["gpt-5.4", "gpt-5.4-mini", "gpt-5-nano"],
                          note: "Current models take max_completion_tokens; older ones take max_tokens."),
        AIProviderProfile(id: "anthropic", name: "Anthropic (OpenAI-compatible)",
                          baseURL: URL(string: "https://api.anthropic.com/v1"), auth: .bearer, keyRequired: true,
                          tokenField: .completionFirst, modelsPath: "models", modelsNeedKey: true,
                          modelsSendKey: true, modelFilter: .all,
                          hints: ["claude-opus-5-5", "claude-sonnet-5-5"],
                          note: "Anthropic's OpenAI SDK compatibility layer; the native Claude API has more features.",
                          temperature: .serverDefault),
        AIProviderProfile(id: "deepseek", name: "DeepSeek", baseURL: URL(string: "https://api.deepseek.com"),
                          auth: .bearer, keyRequired: true, tokenField: .maxTokensFirst, modelsPath: "models",
                          modelsNeedKey: true, modelsSendKey: true, modelFilter: .all,
                          hints: ["deepseek-flash", "deepseek-v4-pro"],
                          note: "A model is required; DeepSeek names the two it serves in its refusal."),
        AIProviderProfile(id: "ollama", name: "Ollama", baseURL: URL(string: "http://127.0.0.1:11434/v1"),
                          auth: .none, keyRequired: false, tokenField: .maxTokensFirst, modelsPath: "models",
                          modelsNeedKey: false, modelsSendKey: false, modelFilter: .all, hints: [],
                          note: "A local server: pull a model first, then pick it here."),
        AIProviderProfile(id: "lmstudio", name: "LM Studio", baseURL: URL(string: "http://127.0.0.1:1234/v1"),
                          auth: .none, keyRequired: false, tokenField: .maxTokensFirst, modelsPath: "models",
                          modelsNeedKey: false, modelsSendKey: false, modelFilter: .all, hints: [],
                          note: "A local server; the model is the one loaded in LM Studio."),
        AIProviderProfile(id: "llamacpp", name: "llama.cpp server",
                          baseURL: URL(string: "http://127.0.0.1:9931/v1"), auth: .none, keyRequired: false,
                          tokenField: .maxTokensFirst, modelsPath: "models", modelsNeedKey: false,
                          modelsSendKey: false, modelFilter: .all, hints: [],
                          note: "The loaded model is the only one it serves."),
        AIProviderProfile(id: "groq", name: "Groq", baseURL: URL(string: "https://api.groq.com/openai/v1"),
                          auth: .bearer, keyRequired: true, tokenField: .maxTokensFirst, modelsPath: "models",
                          modelsNeedKey: true, modelsSendKey: true, modelFilter: .all, hints: []),
        AIProviderProfile(id: "openrouter", name: "OpenRouter", baseURL: URL(string: "https://openrouter.ai/api/v1"),
                          auth: .bearer, keyRequired: true, tokenField: .maxTokensFirst, modelsPath: "models",
                          modelsNeedKey: false, modelsSendKey: false, modelFilter: .textOutput, hints: [],
                          note: "Model ids are vendor/model; the list is public."),
        AIProviderProfile(id: "gemini", name: "Google Gemini (OpenAI-compatible)",
                          baseURL: URL(string: "https://generativelanguage.googleapis.com/v1beta/openai/"),
                          auth: .bearer, keyRequired: true, tokenField: .maxTokensFirst, modelsPath: "models",
                          modelsNeedKey: true, modelsSendKey: true, modelFilter: .all, hints: []),
        AIProviderProfile(id: "xai", name: "xAI", baseURL: URL(string: "https://api.x.ai/v1"), auth: .bearer,
                          keyRequired: true, tokenField: .maxTokensFirst, modelsPath: "models",
                          modelsNeedKey: true, modelsSendKey: true, modelFilter: .all,
                          hints: ["grok-4.7", "grok-4.20"]),
        AIProviderProfile(id: "mistral", name: "Mistral", baseURL: URL(string: "https://api.mistral.ai/v1"),
                          auth: .bearer, keyRequired: true, tokenField: .maxTokensFirst, modelsPath: "models",
                          modelsNeedKey: true, modelsSendKey: true, modelFilter: .all, hints: []),
    ]

    public static func profile(id: String) -> AIProviderProfile? {
        known.first { $0.id == id }
    }
}
