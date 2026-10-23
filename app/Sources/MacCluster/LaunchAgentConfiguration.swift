import Foundation

/// Writing a plist does not replace launchd's already-loaded job. Compare the effective arguments
/// before kickstart, especially after Finder App Translocation or moving/updating the app bundle.
enum LaunchAgentConfiguration {
    static func matches(_ output: Data, arguments: [String]) throws -> Bool {
        let lines = String(decoding: output, as: UTF8.self).components(separatedBy: .newlines)
            .map { $0.trimmingCharacters(in: .whitespaces) }
        guard let start = lines.firstIndex(of: "arguments = {"),
              let end = lines[(start + 1)...].firstIndex(of: "}") else {
            throw ClientError.message("無法確認背景服務的啟動設定，請稍後重試。")
        }
        return Array(lines[(start + 1)..<end]) == arguments
    }
}
