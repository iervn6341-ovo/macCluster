import XCTest
@testable import MacCluster

final class TerminalLogTests: XCTestCase {
    private func state() -> URL {
        let url = FileManager.default.temporaryDirectory.appendingPathComponent("tl-" + UUID().uuidString, isDirectory: true)
        try? FileManager.default.createDirectory(at: url, withIntermediateDirectories: true)
        addTeardownBlock { try? FileManager.default.removeItem(at: url) }
        return url
    }
    private func bytes(_ s: String) -> [UInt8] { Array(s.utf8) }

    func testOutputSurvivesCloseAndFileIsPrivate() throws {
        let dir = state()
        let w = TerminalLogWriter(state: dir, environment: "env-a", shell: "/bin/zsh")
        w.write(bytes("hello\r\n")); w.write(bytes("world\r\n")); w.flush(); w.close()
        let text = TerminalLog.read(dir, "env-a")
        XCTAssertTrue(text.contains("hello\r\nworld\r\n"))
        XCTAssertTrue(text.hasPrefix("── 終端開啟"))
        let mode = try FileManager.default.attributesOfItem(atPath: TerminalLog.live(dir, "env-a").path)[.posixPermissions] as? Int
        XCTAssertEqual(mode, 0o600)
        let dmode = try FileManager.default.attributesOfItem(atPath: TerminalLog.directory(dir).path)[.posixPermissions] as? Int
        XCTAssertEqual(dmode, 0o700)
    }

    func testPreviousSessionIsKeptAndSeparatedPerEnvironment() {
        let dir = state()
        var w = TerminalLogWriter(state: dir, environment: "a", shell: "sh")
        w.write(bytes("first-session\r\n")); w.flush(); w.close()
        let other = TerminalLogWriter(state: dir, environment: "b", shell: "sh")
        other.write(bytes("other-env\r\n")); other.flush(); other.close()
        w = TerminalLogWriter(state: dir, environment: "a", shell: "sh")
        w.write(bytes("second-session\r\n")); w.flush(); w.close()
        let text = TerminalLog.read(dir, "a")
        XCTAssertTrue(text.contains("══ 上一次的終端 ══"))
        XCTAssertLessThan(text.range(of: "first-session")!.lowerBound, text.range(of: "second-session")!.lowerBound)
        XCTAssertFalse(text.contains("other-env"))
    }

    func testRotationBoundsTheFilesAndKeepsTheNewest() throws {
        let dir = state()
        let w = TerminalLogWriter(state: dir, environment: "a", shell: "sh", limit: 1000)
        for i in 0..<100 { w.write(bytes("line-\(i)-" + String(repeating: "x", count: 40) + "\r\n")) }
        w.write(bytes("THE-END\r\n")); w.flush(); w.close()
        let live = try Data(contentsOf: TerminalLog.live(dir, "a")), rotated = try Data(contentsOf: TerminalLog.rotated(dir, "a"))
        XCTAssertLessThanOrEqual(live.count, 1000); XCTAssertLessThanOrEqual(rotated.count, 1000)
        let text = TerminalLog.read(dir, "a")
        XCTAssertTrue(text.contains("THE-END"))
        XCTAssertFalse(text.contains("line-0-"))
    }

    func testFullScreenProgramOutputIsSkippedWithANote() {
        var f = AltScreenFilter()
        var out = f.process(bytes("before\r\n\u{1b}[?1049hGARBAGE\u{1b}[H\u{1b}[2Jmore\u{1b}[?1049lafter\r\n"))
        XCTAssertEqual(String(decoding: out, as: UTF8.self), "before\r\n" + TerminalLog.skippedNote + "after\r\n")
        XCTAssertFalse(f.inAlternate)
        // A sequence split over two chunks is still recognised, and nothing of it leaks.
        f = AltScreenFilter()
        out = f.process(bytes("a\u{1b}[?10")); out += f.process(bytes("49hHIDDEN\u{1b}[?10")); out += f.process(bytes("49lb"))
        XCTAssertEqual(String(decoding: out, as: UTF8.self), "a" + TerminalLog.skippedNote + "b")
        // Other escape sequences pass through untouched.
        f = AltScreenFilter()
        XCTAssertEqual(String(decoding: f.process(bytes("\u{1b}[31mred\u{1b}[0m")), as: UTF8.self), "\u{1b}[31mred\u{1b}[0m")
    }

    func testRemovalAndSweep() throws {
        let dir = state()
        for name in ["keep", "gone", "open", "old"] {
            let w = TerminalLogWriter(state: dir, environment: name, shell: "sh"); w.write(bytes("x\r\n")); w.flush(); w.close()
        }
        // "old" is registered but untouched for longer than the retention; "gone" is no longer registered.
        let past = Date(timeIntervalSinceNow: -(TerminalLog.retention + 3600))
        try FileManager.default.setAttributes([.modificationDate: past], ofItemAtPath: TerminalLog.live(dir, "old").path)
        TerminalLog.sweep(dir, registered: ["keep", "open", "old"], open: ["open"])
        XCTAssertTrue(FileManager.default.fileExists(atPath: TerminalLog.live(dir, "keep").path))
        XCTAssertTrue(FileManager.default.fileExists(atPath: TerminalLog.live(dir, "open").path))
        XCTAssertFalse(FileManager.default.fileExists(atPath: TerminalLog.live(dir, "gone").path))
        XCTAssertFalse(FileManager.default.fileExists(atPath: TerminalLog.live(dir, "old").path))
        TerminalLog.delete(dir, "keep")
        XCTAssertEqual(TerminalLog.read(dir, "keep"), "")
    }
}
