import Foundation
import UniformTypeIdentifiers

// MARK: - UTType Extension

extension UTType {
    static let rootshellBackup = UTType(exportedAs: "com.rootshell.backup")
}

// MARK: - Backup Category

enum BackupCategory: String, CaseIterable, Identifiable, Codable, Sendable {
    case sshKeys
    case sshPasswords
    case connectionHistory
    case knownHosts
    case connectionProfiles
    case customThemes
    case customFonts
    case keybindOverrides
    case hssConfig
    case cloudAccounts
    case aiSettings
    case appPreferences
    case wifiAPManualEntries

    var id: String { rawValue }

    var displayName: String {
        switch self {
        case .sshKeys: "SSH Keys"
        case .sshPasswords: "Saved Passwords"
        case .connectionHistory: "Connection History"
        case .knownHosts: "Known Hosts"
        case .connectionProfiles: "Connection Profiles"
        case .customThemes: "Custom Themes"
        case .customFonts: "Custom Fonts"
        case .keybindOverrides: "Keyboard Shortcuts"
        case .hssConfig: "HSS Config"
        case .cloudAccounts: "Cloud Accounts"
        case .aiSettings: "AI Settings"
        case .appPreferences: "App Preferences"
        case .wifiAPManualEntries: "WiFi AP Manual Entries"
        }
    }

    var systemImage: String {
        switch self {
        case .sshKeys: "key"
        case .sshPasswords: "lock"
        case .connectionHistory: "clock"
        case .knownHosts: "checkmark.shield"
        case .connectionProfiles: "person.crop.rectangle.stack"
        case .customThemes: "paintpalette"
        case .customFonts: "textformat"
        case .keybindOverrides: "keyboard"
        case .hssConfig: "doc.text"
        case .cloudAccounts: "cloud"
        case .aiSettings: "brain"
        case .appPreferences: "gearshape"
        case .wifiAPManualEntries: "wifi.router"
        }
    }

    var isSensitive: Bool {
        switch self {
        case .sshKeys, .sshPasswords, .cloudAccounts, .aiSettings:
            true
        case .connectionHistory, .knownHosts, .connectionProfiles, .customThemes,
             .customFonts, .keybindOverrides, .hssConfig, .appPreferences,
             .wifiAPManualEntries:
            false
        }
    }
}

// MARK: - Backup Payload

struct BackupPayload: Codable {
    let version: Int
    let createdAt: Date
    let appVersion: String
    let appBuild: String
    let deviceName: String
    let categories: BackupCategories
}

struct BackupCategories: Codable {
    var sshKeys: SSHKeysBackup?
    var sshPasswords: SSHPasswordsBackup?
    var connectionHistory: [SSHConnectionHistoryEntry]?
    var knownHosts: [KnownHost]?
    var connectionProfiles: [ConnectionProfile]?
    var customThemes: CustomThemesBackup?
    var customFonts: CustomFontsBackup?
    var keybindOverrides: Data?
    var importedKeybindConfig: ImportedKeybindConfigBackup?
    var hssConfig: HSSConfigBackup?
    var cloudAccounts: CloudAccountsBackup?
    var aiSettings: AISettingsBackup?
    var appSettings: [String: CodableValue]?
    var wifiAPManualEntries: WiFiAPManualEntriesBackup?
}

// MARK: - SSH Keys Backup

struct SSHKeysBackup: Codable {
    var entries: [SSHKeyBackupEntry]
    var defaultKeyIDs: [UUID]?
}

struct SSHKeyBackupEntry: Codable {
    var metadata: SSHKey
    var privateKeyData: Data?
    /// Read from legacy backups for restore; never written to new backups —
    /// exported key data is normalized (decrypted) instead.
    var passphrase: String?

    private enum CodingKeys: String, CodingKey {
        case metadata, privateKeyData, passphrase
    }

    init(metadata: SSHKey, privateKeyData: Data?, passphrase: String? = nil) {
        self.metadata = metadata
        self.privateKeyData = privateKeyData
        self.passphrase = passphrase
    }

    init(from decoder: Decoder) throws {
        let container = try decoder.container(keyedBy: CodingKeys.self)
        metadata = try container.decode(SSHKey.self, forKey: .metadata)
        privateKeyData = try container.decodeIfPresent(Data.self, forKey: .privateKeyData)
        passphrase = try container.decodeIfPresent(String.self, forKey: .passphrase)
    }

    func encode(to encoder: Encoder) throws {
        var container = encoder.container(keyedBy: CodingKeys.self)
        try container.encode(metadata, forKey: .metadata)
        try container.encodeIfPresent(privateKeyData, forKey: .privateKeyData)
    }
}

// MARK: - SSH Passwords Backup

struct SSHPasswordsBackup: Codable {
    var entries: [SSHPasswordBackupEntry]
}

struct SSHPasswordBackupEntry: Codable {
    var metadata: SSHSavedPassword
    var password: String
}

// MARK: - Custom Themes Backup

struct CustomThemesBackup: Codable {
    var themes: [CustomThemeBackupEntry]
}

struct CustomThemeBackupEntry: Codable {
    var theme: CustomTheme
    var themeFileContent: String?
}

// MARK: - Custom Fonts Backup

struct CustomFontsBackup: Codable {
    var families: [CustomFontBackupEntry]
    var replacedBundledFamilies: [String]?
}

struct CustomFontBackupEntry: Codable {
    var family: FontManager.CustomFontFamily
    var fontFiles: [FontFileData]
}

struct FontFileData: Codable {
    var filename: String
    var originalName: String
    var data: Data
}

// MARK: - Imported Keybind Config Backup

struct ImportedKeybindConfigBackup: Codable {
    var configContent: String
    var originalFilename: String?
}

// MARK: - HSS Config Backup

struct HSSConfigBackup: Codable {
    var yamlContent: String
    var filename: String?
}

// MARK: - Cloud Accounts Backup

struct CloudAccountsBackup: Codable {
    var entries: [CloudAccountBackupEntry]
}

struct CloudAccountBackupEntry: Codable {
    var account: CloudAccount
    var credentialsData: Data?
}

// MARK: - AI Settings Backup

struct AISettingsBackup: Codable {
    var apiKeys: [AIAPIKeyEntry]
    var settings: [String: CodableValue]
}

struct AIAPIKeyEntry: Codable {
    var accountName: String
    var apiKey: String
}

// MARK: - WiFi AP Manual Entries Backup

struct WiFiAPManualEntriesBackup: Codable {
    var accessPoints: [WiFiAccessPoint]
    var vendorDomains: [String: String]  // MAC → domain for favicon fetching
}

// MARK: - CodableValue (Type-erasing wrapper for UserDefaults values)

nonisolated enum CodableValue: Codable, Equatable, Hashable, Sendable {
    case string(String)
    case int(Int)
    case double(Double)
    case bool(Bool)
    case data(Data)
    case stringArray([String])

    enum CodingKeys: String, CodingKey {
        case type, value
    }

    enum ValueType: String, Codable {
        case string, int, double, bool, data, stringArray
    }

    init(from decoder: Decoder) throws {
        let container = try decoder.container(keyedBy: CodingKeys.self)
        let type = try container.decode(ValueType.self, forKey: .type)
        switch type {
        case .string:
            self = .string(try container.decode(String.self, forKey: .value))
        case .int:
            self = .int(try container.decode(Int.self, forKey: .value))
        case .double:
            self = .double(try container.decode(Double.self, forKey: .value))
        case .bool:
            self = .bool(try container.decode(Bool.self, forKey: .value))
        case .data:
            self = .data(try container.decode(Data.self, forKey: .value))
        case .stringArray:
            self = .stringArray(try container.decode([String].self, forKey: .value))
        }
    }

    func encode(to encoder: Encoder) throws {
        var container = encoder.container(keyedBy: CodingKeys.self)
        switch self {
        case .string(let v):
            try container.encode(ValueType.string, forKey: .type)
            try container.encode(v, forKey: .value)
        case .int(let v):
            try container.encode(ValueType.int, forKey: .type)
            try container.encode(v, forKey: .value)
        case .double(let v):
            try container.encode(ValueType.double, forKey: .type)
            try container.encode(v, forKey: .value)
        case .bool(let v):
            try container.encode(ValueType.bool, forKey: .type)
            try container.encode(v, forKey: .value)
        case .data(let v):
            try container.encode(ValueType.data, forKey: .type)
            try container.encode(v, forKey: .value)
        case .stringArray(let v):
            try container.encode(ValueType.stringArray, forKey: .type)
            try container.encode(v, forKey: .value)
        }
    }

    var anyValue: Any {
        switch self {
        case .string(let v): v
        case .int(let v): v
        case .double(let v): v
        case .bool(let v): v
        case .data(let v): v
        case .stringArray(let v): v
        }
    }

    init?(from value: Any) {
        switch value {
        case let v as String:
            self = .string(v)
        case let v as Int:
            self = .int(v)
        case let v as Double:
            self = .double(v)
        case let v as Bool:
            self = .bool(v)
        case let v as Data:
            self = .data(v)
        case let v as [String]:
            self = .stringArray(v)
        default:
            return nil
        }
    }
}

// MARK: - Backup Manifest (for validation/preview)

struct BackupManifest: Sendable {
    let version: Int
    let createdAt: Date
    let appVersion: String
    let appBuild: String
    let deviceName: String
    let categoryCounts: [BackupCategory: Int]
}

// MARK: - Backup Error

enum BackupError: LocalizedError {
    case wrongPassword
    case invalidFileFormat
    case unsupportedVersion(Int)
    case encryptionFailed(Error)
    case decryptionFailed(Error)
    case encodingFailed(Error)
    case decodingFailed(Error)
    case fileWriteFailed(Error)
    case fileReadFailed(Error)
    case noDataToBackup
    case cancelled

    var errorDescription: String? {
        switch self {
        case .wrongPassword:
            "Incorrect password. Please try again."
        case .invalidFileFormat:
            "This file is not a valid \(FlightdeckBrand.name) backup."
        case .unsupportedVersion(let v):
            "This backup was created with a newer version (v\(v)) and cannot be read."
        case .encryptionFailed(let e):
            "Encryption failed: \(e.localizedDescription)"
        case .decryptionFailed(let e):
            "Decryption failed: \(e.localizedDescription)"
        case .encodingFailed(let e):
            "Failed to encode backup data: \(e.localizedDescription)"
        case .decodingFailed(let e):
            "Failed to decode backup data: \(e.localizedDescription)"
        case .fileWriteFailed(let e):
            "Failed to write backup file: \(e.localizedDescription)"
        case .fileReadFailed(let e):
            "Failed to read backup file: \(e.localizedDescription)"
        case .noDataToBackup:
            "No data found to include in the backup."
        case .cancelled:
            "Backup was cancelled."
        }
    }
}

// MARK: - Restore Summary

struct RestoreSummary: Sendable {
    var results: [BackupCategory: CategoryResult] = [:]

    struct CategoryResult: Sendable {
        var restored: Int
        var skipped: Int
        var errors: [String]
    }

    var totalRestored: Int { results.values.reduce(0) { $0 + $1.restored } }
    var totalSkipped: Int { results.values.reduce(0) { $0 + $1.skipped } }
    var totalErrors: Int { results.values.reduce(0) { $0 + $1.errors.count } }
}

// MARK: - Export Summary

struct ExportSummary: Sendable {
    var categoryCounts: [BackupCategory: Int] = [:]
    var skippedKeys: [String] = []  // Key names that were skipped (e.g., biometric cancelled)
    var totalItems: Int { categoryCounts.values.reduce(0, +) }
}
