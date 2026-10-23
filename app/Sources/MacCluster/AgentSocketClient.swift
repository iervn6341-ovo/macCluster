import Foundation
import Darwin

/// HTTP over the existing agent socket. Never starts the agent or invokes its CLI.
enum AgentSocketClient {
    static func request(socketPath: String, method: String, path: String, body: Data? = nil, timeout: TimeInterval = 20) async throws -> Data {
        try await Task.detached(priority: .utility) {
            try perform(socketPath: socketPath, method: method, path: path, body: body, timeout: timeout)
        }.value
    }

    private static func perform(socketPath: String, method: String, path: String, body: Data?, timeout: TimeInterval) throws -> Data {
        guard !method.contains("\r"), !method.contains("\n"), !path.contains("\r"), !path.contains("\n") else {
            throw ClientError.message("無效的管理程式要求。")
        }
        var address = sockaddr_un()
        address.sun_family = sa_family_t(AF_UNIX)
        let bytes = Array(socketPath.utf8) + [0]
        guard bytes.count <= MemoryLayout.size(ofValue: address.sun_path) else { throw disconnected() }
        withUnsafeMutableBytes(of: &address.sun_path) { $0.copyBytes(from: bytes) }
        let descriptor = Darwin.socket(AF_UNIX, SOCK_STREAM, 0)
        guard descriptor >= 0 else { throw disconnected() }
        defer { Darwin.close(descriptor) }
        var noSigPipe: Int32 = 1
        _ = setsockopt(descriptor, SOL_SOCKET, SO_NOSIGPIPE, &noSigPipe, socklen_t(MemoryLayout<Int32>.size))
        guard fcntl(descriptor, F_SETFL, O_NONBLOCK) >= 0 else { throw disconnected() }
        let deadline = ProcessInfo.processInfo.systemUptime + max(0, timeout)
        let connected = withUnsafePointer(to: &address) {
            $0.withMemoryRebound(to: sockaddr.self, capacity: 1) {
                Darwin.connect(descriptor, $0, socklen_t(MemoryLayout<sockaddr_un>.size))
            }
        }
        if connected != 0 {
            guard errno == EINPROGRESS || errno == EAGAIN else { throw disconnected() }
            try wait(descriptor, events: Int16(POLLOUT), deadline: deadline)
            var socketError: Int32 = 0
            var size = socklen_t(MemoryLayout<Int32>.size)
            guard getsockopt(descriptor, SOL_SOCKET, SO_ERROR, &socketError, &size) == 0, socketError == 0 else { throw disconnected() }
        }
        let payload = body ?? Data()
        var request = Data("\(method) \(path) HTTP/1.1\r\nHost: localhost\r\nConnection: close\r\nContent-Type: application/json\r\nContent-Length: \(payload.count)\r\n\r\n".utf8)
        request.append(payload)
        var written = 0
        while written < request.count {
            try wait(descriptor, events: Int16(POLLOUT), deadline: deadline)
            let count = request.withUnsafeBytes { Darwin.send(descriptor, $0.baseAddress!.advanced(by: written), request.count - written, 0) }
            if count < 0, errno == EINTR || errno == EAGAIN || errno == EWOULDBLOCK { continue }
            guard count > 0 else { throw disconnected() }
            written += count
        }
        var received = Data()
        var buffer = [UInt8](repeating: 0, count: 16384)
        while true {
            try wait(descriptor, events: Int16(POLLIN), deadline: deadline)
            let count = Darwin.recv(descriptor, &buffer, buffer.count, 0)
            if count < 0, errno == EINTR || errno == EAGAIN || errno == EWOULDBLOCK { continue }
            guard count >= 0 else { throw disconnected() }
            if count > 0 { received.append(contentsOf: buffer.prefix(count)) }
            guard received.count <= 64 * 1024 * 1024 else { throw ClientError.message("管理程式回應過大。") }
            if let response = try parse(received, ended: count == 0) { return response }
            if count == 0 { throw disconnected() }
        }
    }

    private static func disconnected() -> ClientError { .agentUnavailable }

    private static func wait(_ descriptor: Int32, events: Int16, deadline: TimeInterval) throws {
        while true {
            let remaining = deadline - ProcessInfo.processInfo.systemUptime
            guard remaining > 0 else { throw ClientError.message("等待管理程式回應逾時。") }
            var item = pollfd(fd: descriptor, events: events, revents: 0)
            let result = Darwin.poll(&item, 1, Int32(min(remaining * 1000 + 1, Double(Int32.max))))
            if result < 0, errno == EINTR { continue }
            if result == 0 { continue }
            guard result > 0, item.revents & Int16(POLLNVAL | POLLERR) == 0 else { throw disconnected() }
            // POLLHUP may accompany the final readable bytes or EOF.
            return
        }
    }

    private static func parse(_ data: Data, ended: Bool) throws -> Data? {
        guard let separator = data.range(of: Data("\r\n\r\n".utf8)),
              let header = String(data: data[..<separator.lowerBound], encoding: .utf8) else { return nil }
        let lines = header.components(separatedBy: "\r\n")
        guard let first = lines.first, let status = Int(first.split(separator: " ").dropFirst().first ?? "") else { throw disconnected() }
        var headers: [String: String] = [:]
        for line in lines.dropFirst() {
            guard let colon = line.firstIndex(of: ":") else { continue }
            headers[String(line[..<colon]).lowercased()] = line[line.index(after: colon)...].trimmingCharacters(in: .whitespaces)
        }
        let rawBody = Data(data[separator.upperBound...])
        let response: Data
        if headers["transfer-encoding"]?.lowercased().contains("chunked") == true {
            guard let decoded = try decodeChunks(rawBody) else { return nil }
            response = decoded
        } else if let lengthText = headers["content-length"] {
            guard let length = Int(lengthText), length >= 0 else { throw disconnected() }
            guard rawBody.count >= length else { return nil }
            response = Data(rawBody.prefix(length))
        } else {
            guard ended else { return nil }
            response = rawBody
        }
        guard (200..<300).contains(status) else {
            let object = (try? JSONSerialization.jsonObject(with: response)) as? [String: Any]
            let message = object?["error"] as? String ?? String(data: response, encoding: .utf8)?.trimmingCharacters(in: .whitespacesAndNewlines)
            throw ClientError.message(message.flatMap { $0.isEmpty ? nil : $0 } ?? "管理程式要求失敗（HTTP \(status)）。")
        }
        return response
    }

    private static func decodeChunks(_ data: Data) throws -> Data? {
        var offset = 0
        var output = Data()
        let delimiter = Data("\r\n".utf8)
        while true {
            guard let line = data.range(of: delimiter, in: offset..<data.count),
                  let text = String(data: data[offset..<line.lowerBound], encoding: .utf8),
                  let size = Int(text.split(separator: ";", maxSplits: 1).first ?? "", radix: 16), size >= 0 else {
                if data.range(of: delimiter, in: offset..<data.count) != nil { throw disconnected() }
                return nil
            }
            offset = line.upperBound
            if size == 0 {
                if data.count >= offset + 2, data[offset..<offset + 2] == delimiter { return output }
                if data.range(of: Data("\r\n\r\n".utf8), in: offset..<data.count) != nil { return output }
                return nil
            }
            guard size <= 64 * 1024 * 1024 else { throw ClientError.message("管理程式回應過大。") }
            guard data.count - offset >= size + 2 else { return nil }
            output.append(data[offset..<offset + size])
            offset += size
            guard data[offset..<offset + 2] == delimiter else { throw disconnected() }
            offset += 2
        }
    }
}
