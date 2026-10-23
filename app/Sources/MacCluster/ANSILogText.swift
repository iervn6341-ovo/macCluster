import AppKit

/// Renders log styling only. Cursor commands and OSC payloads never execute.
enum ANSILogText {
    private struct Style {
        var foreground: NSColor = .textColor
        var background: NSColor?
        var bold = false
        var underline = false
        func attributes(fontSize: CGFloat) -> [NSAttributedString.Key: Any] {
            var result: [NSAttributedString.Key: Any] = [
                .font: NSFont.monospacedSystemFont(ofSize: fontSize, weight: bold ? .bold : .regular),
                .foregroundColor: foreground
            ]
            if let background { result[.backgroundColor] = background }
            if underline { result[.underlineStyle] = NSUnderlineStyle.single.rawValue }
            return result
        }
        mutating func apply(_ parameters: String) {
            let codes = parameters.isEmpty ? [0] : parameters.split(separator: ";", omittingEmptySubsequences: false).map { Int($0) ?? ($0.isEmpty ? 0 : -1) }
            var i = 0
            while i < codes.count {
                let code = codes[i]
                switch code {
                case 0: self = Style()
                case 1: bold = true
                case 22: bold = false
                case 4: underline = true
                case 24: underline = false
                case 30...37: foreground = ANSILogText.color(code - 30)
                case 90...97: foreground = ANSILogText.color(code - 90 + 8)
                case 40...47: background = ANSILogText.color(code - 40)
                case 100...107: background = ANSILogText.color(code - 100 + 8)
                case 39: foreground = .textColor
                case 49: background = nil
                case 38, 48:
                    var color: NSColor?
                    if i + 1 < codes.count, codes[i + 1] == 5 {
                        guard i + 2 < codes.count else { return }
                        if (0...255).contains(codes[i + 2]) { color = ANSILogText.color(codes[i + 2]) }
                        i += 2
                    } else if i + 1 < codes.count, codes[i + 1] == 2 {
                        guard i + 4 < codes.count else { return }
                        let rgb = Array(codes[(i + 2)...(i + 4)])
                        if rgb.allSatisfy({ (0...255).contains($0) }) {
                            color = NSColor(srgbRed: CGFloat(rgb[0]) / 255, green: CGFloat(rgb[1]) / 255, blue: CGFloat(rgb[2]) / 255, alpha: 1)
                        }
                        i += 4
                    } else { return }
                    if let color {
                        if code == 38 { foreground = color } else { background = color }
                    }
                default: break
                }
                i += 1
            }
        }
    }

    private static func color(_ index: Int) -> NSColor {
        let basic: [UInt32] = [0x000000, 0xCD3131, 0x0DBC79, 0xE5B50A, 0x2472C8, 0xBC3FBC, 0x11A8CD, 0xE5E5E5,
                               0x666666, 0xF14C4C, 0x23D18B, 0xF5F543, 0x3B8EEA, 0xD670D6, 0x29B8DB, 0xFFFFFF]
        if index < 16 {
            let value = basic[index]
            return NSColor(srgbRed: CGFloat((value >> 16) & 255) / 255, green: CGFloat((value >> 8) & 255) / 255, blue: CGFloat(value & 255) / 255, alpha: 1)
        }
        if index >= 232 {
            return NSColor(srgbRed: CGFloat(8 + (index - 232) * 10) / 255, green: CGFloat(8 + (index - 232) * 10) / 255, blue: CGFloat(8 + (index - 232) * 10) / 255, alpha: 1)
        }
        let n = index - 16
        let levels: [CGFloat] = [0, 95, 135, 175, 215, 255]
        return NSColor(srgbRed: levels[n / 36] / 255, green: levels[(n / 6) % 6] / 255, blue: levels[n % 6] / 255, alpha: 1)
    }

    static func render(_ source: String, fontSize: CGFloat = 12, lineSpacing: CGFloat = 0, characterSpacing: CGFloat = 0) -> NSAttributedString {
        let scalars = Array(source.unicodeScalars)
        let result = NSMutableAttributedString(string: "")
        var style = Style()
        var buffer = String.UnicodeScalarView()
        func flush() {
            guard !buffer.isEmpty else { return }
            result.append(NSAttributedString(string: String(buffer), attributes: style.attributes(fontSize: fontSize)))
            buffer = String.UnicodeScalarView()
        }
        var i = 0
        while i < scalars.count {
            guard scalars[i].value == 27 else { buffer.append(scalars[i]); i += 1; continue }
            flush()
            i += 1
            guard i < scalars.count else { break }
            if scalars[i] == "[" {
                i += 1
                let start = i
                while i < scalars.count && !(0x40...0x7E).contains(scalars[i].value) { i += 1 }
                guard i < scalars.count else { break }
                if scalars[i] == "m" {
                    style.apply(String(String.UnicodeScalarView(scalars[start..<i])))
                }
                i += 1
            } else if scalars[i] == "]" {
                // OSC hyperlinks, titles and clipboard sequences are omitted.
                i += 1
                while i < scalars.count {
                    if scalars[i].value == 7 { i += 1; break }
                    if scalars[i].value == 27 && i + 1 < scalars.count && scalars[i + 1] == "\\" { i += 2; break }
                    i += 1
                }
            } else {
                // Skip other ESC sequences (including charset selection).
                while i < scalars.count && (0x20...0x2F).contains(scalars[i].value) { i += 1 }
                if i < scalars.count { i += 1 }
            }
        }
        flush()
        let paragraph = NSMutableParagraphStyle()
        paragraph.lineSpacing = lineSpacing
        result.addAttributes([.paragraphStyle: paragraph, .kern: characterSpacing], range: NSRange(location: 0, length: result.length))
        return result
    }
}
