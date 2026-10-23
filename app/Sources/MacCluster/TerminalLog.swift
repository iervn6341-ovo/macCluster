import Foundation
import CryptoKit
import SwiftTerm

/// Keeps what an environment's embedded terminal printed, so it can still be read after the terminal is closed.
/// Only the output is saved (never keystrokes). Files live in the state directory, readable by this account only,
/// and are bounded: the live file is rotated at `limit`, and the previous terminal session is kept separately.
///
///   <state>/terminals/<id>.log        the current session (the live file)
///   <state>/terminals/<id>.log.1      the part of the current session that was rotated out
///   <state>/terminals/<id>.prev.log   the previous session, kept until the next one replaces it
enum TerminalLog {
    static let limit = 1 << 20
    /// A closed terminal's files expire after this long without being written.
    static let retention: TimeInterval = 7 * 24 * 3600
    static let skippedNote = "\r\n（全螢幕程式輸出略過）\r\n"

    static func directory(_ state: URL) -> URL { state.appendingPathComponent("terminals", isDirectory: true) }
    static func id(_ environment: String) -> String {
        SHA256.hash(data: Data(environment.utf8)).prefix(8).map { String(format: "%02x", $0) }.joined()
    }
    static func live(_ state: URL, _ environment: String) -> URL { directory(state).appendingPathComponent(id(environment) + ".log") }
    static func rotated(_ state: URL, _ environment: String) -> URL { directory(state).appendingPathComponent(id(environment) + ".log.1") }
    static func previous(_ state: URL, _ environment: String) -> URL { directory(state).appendingPathComponent(id(environment) + ".prev.log") }

    private static func ensureDirectory(_ state: URL) {
        try? FileManager.default.createDirectory(at: directory(state), withIntermediateDirectories: true, attributes: [.posixPermissions: 0o700])
    }

    /// The live file of an earlier session becomes "previous"; a fresh live file starts with a header line.
    static func beginSession(_ state: URL, _ environment: String, shell: String, now: Date = Date()) {
        ensureDirectory(state)
        let manager = FileManager.default
        let liveURL = live(state, environment)
        let hasOutput = ((try? manager.attributesOfItem(atPath: liveURL.path)[.size]) as? Int ?? 0) > 0
        if hasOutput {
            // The previous session is its rotated part followed by its live part.
            let old = (try? Data(contentsOf: rotated(state, environment))) ?? Data()
            let cur = (try? Data(contentsOf: liveURL)) ?? Data()
            let both = Data((old + cur).suffix(limit))
            try? manager.removeItem(at: previous(state, environment))
            manager.createFile(atPath: previous(state, environment).path, contents: both, attributes: [.posixPermissions: 0o600])
        }
        try? manager.removeItem(at: rotated(state, environment))
        try? manager.removeItem(at: liveURL)
        let stamp = ISO8601DateFormatter().string(from: now)
        let header = "── 終端開啟 \(stamp) · \(environment) · \(shell) ──\r\n"
        manager.createFile(atPath: liveURL.path, contents: Data(header.utf8), attributes: [.posixPermissions: 0o600])
    }

    /// What the log area shows: the previous session (marked), then the current one.
    static func read(_ state: URL, _ environment: String) -> String {
        func text(_ url: URL, tail: Int = limit) -> String {
            guard let data = try? Data(contentsOf: url) else { return "" }
            return String(decoding: data.suffix(tail), as: UTF8.self)
        }
        let prev = text(previous(state, environment))
        let current = String(decoding: Data(((try? Data(contentsOf: rotated(state, environment))) ?? Data()) + ((try? Data(contentsOf: live(state, environment))) ?? Data())).suffix(limit), as: UTF8.self)
        var out = ""
        if !prev.isEmpty { out += "══ 上一次的終端 ══\r\n" + prev + "\r\n══ 本次的終端 ══\r\n" }
        return out + current
    }

    static func delete(_ state: URL, _ environment: String) {
        for url in [live(state, environment), rotated(state, environment), previous(state, environment)] { try? FileManager.default.removeItem(at: url) }
    }

    /// Removes files of environments that are no longer registered, and files untouched for `retention`.
    /// Files of a terminal that is open right now are never removed.
    static func sweep(_ state: URL, registered: Set<String>, open: Set<String>, now: Date = Date()) {
        let manager = FileManager.default
        let keepIDs = Set(registered.map(id))
        let openIDs = Set(open.map(id))
        guard let files = try? manager.contentsOfDirectory(at: directory(state), includingPropertiesForKeys: [.contentModificationDateKey]) else { return }
        for file in files {
            let fileID = String(file.lastPathComponent.prefix(16))
            if openIDs.contains(fileID) { continue }
            let modified = (try? file.resourceValues(forKeys: [.contentModificationDateKey]))?.contentModificationDate ?? now
            if !keepIDs.contains(fileID) || now.timeIntervalSince(modified) > retention { try? manager.removeItem(at: file) }
        }
    }
}

/// Drops what a full-screen program (vim, top, less) draws on the alternate screen: as bytes in a log it is
/// unreadable. A note replaces it. A sequence that is split across two chunks is held back until it is complete.
struct AltScreenFilter {
    private(set) var inAlternate = false
    private var held: [UInt8] = []
    private static let enter: [[UInt8]] = ["\u{1b}[?1049h", "\u{1b}[?1047h", "\u{1b}[?47h"].map { Array($0.utf8) }
    private static let leave: [[UInt8]] = ["\u{1b}[?1049l", "\u{1b}[?1047l", "\u{1b}[?47l"].map { Array($0.utf8) }
    private static let longest = 8

    mutating func process(_ chunk: [UInt8]) -> [UInt8] {
        var data = held + chunk
        held = []
        var out: [UInt8] = []
        var i = 0
        while i < data.count {
            if data[i] == 0x1b {
                let rest = data[i...]
                let patterns = inAlternate ? Self.leave : Self.enter
                if let hit = patterns.first(where: { rest.starts(with: $0) }) {
                    if inAlternate { inAlternate = false } else {
                        inAlternate = true
                        out += Array(TerminalLog.skippedNote.utf8)
                    }
                    i += hit.count
                    continue
                }
                // Could the end of this chunk be the start of a sequence we are looking for?
                if rest.count < Self.longest, (Self.enter + Self.leave).contains(where: { $0.starts(with: Array(rest)) }) {
                    held = Array(rest)
                    break
                }
            }
            if !inAlternate { out.append(data[i]) }
            i += 1
        }
        data = []
        return out
    }
}

/// Appends to a log file on its own queue, so a burst of output never waits for the disk on the main thread.
final class TerminalLogWriter: @unchecked Sendable {
    private let queue = DispatchQueue(label: "maccluster.terminal-log")
    private let state: URL
    private let environment: String
    private let limit: Int
    private var filter = AltScreenFilter()
    private var handle: FileHandle?
    private var size = 0

    init(state: URL, environment: String, shell: String, limit: Int = TerminalLog.limit) {
        self.state = state; self.environment = environment; self.limit = limit
        queue.sync {
            TerminalLog.beginSession(state, environment, shell: shell)
            openLive()
        }
    }

    private func openLive() {
        handle = try? FileHandle(forWritingTo: TerminalLog.live(state, environment))
        size = Int((try? handle?.seekToEnd()) ?? 0)
    }

    func write(_ bytes: [UInt8]) {
        queue.async { [self] in
            let kept = filter.process(bytes)
            guard !kept.isEmpty, let handle else { return }
            if size + kept.count > limit {
                // Rotate: what has been written so far becomes the ".1" part; the live file starts empty.
                try? handle.close()
                let manager = FileManager.default
                try? manager.removeItem(at: TerminalLog.rotated(state, environment))
                try? manager.moveItem(at: TerminalLog.live(state, environment), to: TerminalLog.rotated(state, environment))
                manager.createFile(atPath: TerminalLog.live(state, environment).path, contents: nil, attributes: [.posixPermissions: 0o600])
                openLive()
            }
            try? self.handle?.write(contentsOf: Data(kept.suffix(limit)))
            size += min(kept.count, limit)
        }
    }

    /// Waits until everything written so far is on disk (used before reading, and by tests).
    func flush() { queue.sync {} }
    func close() { queue.sync { try? handle?.close(); handle = nil } }
}

/// The terminal view that also hands everything it receives to the log.
final class LoggedTerminalView: LocalProcessTerminalView {
    var logWriter: TerminalLogWriter?
    override func dataReceived(slice: ArraySlice<UInt8>) {
        logWriter?.write(Array(slice))
        super.dataReceived(slice: slice)
    }
}
