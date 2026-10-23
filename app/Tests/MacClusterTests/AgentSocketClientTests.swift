import XCTest
import Foundation
import Darwin
@testable import MacCluster

@MainActor
final class AgentSocketClientTests: XCTestCase {
    func testGETContentLengthResponse() async throws {
        let fixture = try SocketFixture(response: "HTTP/1.1 200 OK\r\nContent-Length: 11\r\n\r\n{\"ok\":true}")
        defer { fixture.close() }
        let data = try await AgentSocketClient.request(socketPath: fixture.path, method: "GET", path: "/v1/status")
        XCTAssertEqual(String(decoding: data, as: UTF8.self), "{\"ok\":true}")
        XCTAssertTrue(fixture.request.hasPrefix("GET /v1/status HTTP/1.1\r\n"))
    }

    func testPOSTAndChunkedResponse() async throws {
        let fixture = try SocketFixture(response: "HTTP/1.1 200 OK\r\nTransfer-Encoding: chunked\r\n\r\n4;test=yes\r\n{\"ok\r\n7\r\n\":true}\r\n0\r\n\r\n")
        defer { fixture.close() }
        let data = try await AgentSocketClient.request(socketPath: fixture.path, method: "POST", path: "/v1/settings", body: Data("{\"enabled\":true}".utf8))
        XCTAssertEqual(String(decoding: data, as: UTF8.self), "{\"ok\":true}")
        XCTAssertTrue(fixture.request.hasSuffix("{\"enabled\":true}"))
        XCTAssertTrue(fixture.request.contains("Content-Length: 16\r\n"))
    }

    func testHTTPErrorPreservesServerMessage() async throws {
        let fixture = try SocketFixture(response: "HTTP/1.1 400 Bad Request\r\nContent-Length: 19\r\n\r\n{\"error\":\"invalid\"}")
        defer { fixture.close() }
        do {
            _ = try await AgentSocketClient.request(socketPath: fixture.path, method: "POST", path: "/v1/settings")
            XCTFail("Expected HTTP error")
        } catch { XCTAssertEqual(error.localizedDescription, "invalid") }
    }

    func testMissingSocketDoesNotLaunchAnything() async {
        do {
            _ = try await AgentSocketClient.request(socketPath: "/tmp/maccluster-missing-\(UUID().uuidString).sock", method: "GET", path: "/v1/status")
            XCTFail("Expected connection error")
        } catch ClientError.agentUnavailable {
            // Explicit offline state, without subprocess fallback.
        } catch { XCTFail("Unexpected error: \(error)") }
    }

    func testRefusedSocketDoesNotLaunchAnything() async throws {
        let path = "/tmp/mc-refused-\(UUID().uuidString).sock"
        let descriptor = Darwin.socket(AF_UNIX, SOCK_STREAM, 0)
        guard descriptor >= 0 else { XCTFail("Cannot create fixture"); return }
        var address = sockaddr_un()
        address.sun_family = sa_family_t(AF_UNIX)
        let bytes = Array(path.utf8) + [0]
        withUnsafeMutableBytes(of: &address.sun_path) { $0.copyBytes(from: bytes) }
        let result = withUnsafePointer(to: &address) {
            $0.withMemoryRebound(to: sockaddr.self, capacity: 1) {
                Darwin.bind(descriptor, $0, socklen_t(MemoryLayout<sockaddr_un>.size))
            }
        }
        Darwin.close(descriptor)
        defer { unlink(path) }
        XCTAssertEqual(result, 0)
        do {
            _ = try await AgentSocketClient.request(socketPath: path, method: "GET", path: "/v1/status")
            XCTFail("Expected refused connection")
        } catch ClientError.agentUnavailable {
            // Explicit offline state, without subprocess fallback.
        } catch { XCTFail("Unexpected error: \(error)") }
    }

    func testAbruptDisconnectIsConnectionError() async throws {
        let fixture = try SocketFixture(response: "HTTP/1.1 200 OK\r\nContent-Length: 100\r\n\r\nshort")
        defer { fixture.close() }
        do {
            _ = try await AgentSocketClient.request(socketPath: fixture.path, method: "GET", path: "/v1/status")
            XCTFail("Expected truncated response error")
        } catch ClientError.agentUnavailable {
            // Explicit offline state, without subprocess fallback.
        } catch { XCTFail("Unexpected error: \(error)") }
    }

    func testUnresponsiveSocketTimesOut() async throws {
        let fixture = try SocketFixture(response: "", delay: 0.6)
        defer { fixture.close() }
        let start = ProcessInfo.processInfo.systemUptime
        do {
            _ = try await AgentSocketClient.request(socketPath: fixture.path, method: "GET", path: "/v1/status", timeout: 0.1)
            XCTFail("Expected timeout")
        } catch { XCTAssertEqual(error.localizedDescription, "等待管理程式回應逾時。") }
        XCTAssertLessThan(ProcessInfo.processInfo.systemUptime - start, 0.5)
    }
}

/// Each test uses an isolated Unix socket; no launchd job or agent process is involved.
private final class SocketFixture: @unchecked Sendable {
    let path = "/tmp/mc-http-\(UUID().uuidString).sock"
    private let listener: Int32
    private let lock = NSLock()
    private var captured = ""
    var request: String { lock.lock(); defer { lock.unlock() }; return captured }

    init(response: String, delay: TimeInterval = 0) throws {
        listener = Darwin.socket(AF_UNIX, SOCK_STREAM, 0)
        guard listener >= 0 else { throw FixtureError.socket }
        var address = sockaddr_un()
        address.sun_family = sa_family_t(AF_UNIX)
        let bytes = Array(path.utf8) + [0]
        withUnsafeMutableBytes(of: &address.sun_path) { $0.copyBytes(from: bytes) }
        let result = withUnsafePointer(to: &address) {
            $0.withMemoryRebound(to: sockaddr.self, capacity: 1) {
                Darwin.bind(listener, $0, socklen_t(MemoryLayout<sockaddr_un>.size))
            }
        }
        guard result == 0, Darwin.listen(listener, 1) == 0 else {
            Darwin.close(listener)
            throw FixtureError.socket
        }
        DispatchQueue.global().async { [self] in
            let connection = Darwin.accept(listener, nil, nil)
            guard connection >= 0 else { return }
            defer { Darwin.close(connection) }
            var noSigPipe: Int32 = 1
            _ = setsockopt(connection, SOL_SOCKET, SO_NOSIGPIPE, &noSigPipe, socklen_t(MemoryLayout<Int32>.size))
            var receiveTimeout = timeval(tv_sec: 2, tv_usec: 0)
            _ = setsockopt(connection, SOL_SOCKET, SO_RCVTIMEO, &receiveTimeout, socklen_t(MemoryLayout<timeval>.size))
            var input = Data()
            var buffer = [UInt8](repeating: 0, count: 1024)
            while true {
                let count = Darwin.recv(connection, &buffer, buffer.count, 0)
                guard count > 0 else { break }
                input.append(contentsOf: buffer.prefix(count))
                guard let end = input.range(of: Data("\r\n\r\n".utf8)) else { continue }
                let header = String(decoding: input[..<end.lowerBound], as: UTF8.self)
                let lengthLine = header.components(separatedBy: "\r\n").first { $0.lowercased().hasPrefix("content-length:") }
                let length = Int(lengthLine?.split(separator: ":").last?.trimmingCharacters(in: .whitespaces) ?? "0") ?? 0
                if input.count >= end.upperBound + length { break }
            }
            lock.lock(); captured = String(decoding: input, as: UTF8.self); lock.unlock()
            if delay > 0 { Thread.sleep(forTimeInterval: delay) }
            let bytes = Array(response.utf8)
            var sent = 0
            while sent < bytes.count {
                let count = bytes.withUnsafeBytes { Darwin.send(connection, $0.baseAddress!.advanced(by: sent), bytes.count - sent, 0) }
                guard count > 0 else { break }
                sent += count
            }
        }
    }

    func close() {
        Darwin.close(listener)
        unlink(path)
    }
    private enum FixtureError: Error { case socket }
}
