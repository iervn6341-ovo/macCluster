import Foundation

struct AcceptedJoin: Decodable { let id: String }

enum JoinValidation {
    static let nodeNameHint = "Node 名稱須符合 Kubernetes 的小寫 DNS 名稱格式；也可以留空讓管理程式自動產生。"

    static func defaultNodeName(hostName: String, identifier: UUID = UUID()) -> String {
        let host = hostName.components(separatedBy: ".").first ?? hostName
        var stem = host.lowercased().replacingOccurrences(of: "[^a-z0-9]+", with: "-", options: .regularExpression)
            .trimmingCharacters(in: CharacterSet(charactersIn: "-"))
        stem = String(stem.prefix(54)).trimmingCharacters(in: CharacterSet(charactersIn: "-"))
        if stem.isEmpty { stem = "node" }
        return stem + "-" + identifier.uuidString.prefix(8).lowercased()
    }

    static func validateNodeName(_ name: String) throws {
        if name.isEmpty { return }
        let dns = #"^[a-z0-9]([-a-z0-9]*[a-z0-9])?(\.[a-z0-9]([-a-z0-9]*[a-z0-9])?)*$"#
        guard name.utf8.count <= 253,
              name.range(of: dns, options: .regularExpression) == (name.startIndex..<name.endIndex) else {
            throw ClientError.message(nodeNameHint)
        }
    }

    static func explanation(_ message: String) -> String {
        message.contains("node name must be DNS-compatible") ? nodeNameHint : message
    }
}
