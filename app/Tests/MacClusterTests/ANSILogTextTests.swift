import XCTest
import AppKit
@testable import MacCluster

final class ANSILogTextTests: XCTestCase {
    func testColorResetAndCopyText() {
        let text = ANSILogText.render("原文\u{1b}[31;1m紅字\u{1b}[0m正常")
        XCTAssertEqual(text.string, "原文紅字正常")
        let red = text.attribute(.foregroundColor, at: 2, effectiveRange: nil) as! NSColor
        XCTAssertGreaterThan(red.redComponent, red.greenComponent)
        XCTAssertEqual(text.attribute(.foregroundColor, at: 4, effectiveRange: nil) as? NSColor, NSColor.textColor)
        let bold = text.attribute(.font, at: 2, effectiveRange: nil) as! NSFont
        XCTAssertTrue(NSFontManager.shared.traits(of: bold).contains(.boldFontMask))
    }
    func testExtendedColorsAndBackgroundReset() {
        let text = ANSILogText.render("\u{1b}[38;5;196mA\u{1b}[48;2;12;34;56;4mB\u{1b}[49;24mC")
        let red = text.attribute(.foregroundColor, at: 0, effectiveRange: nil) as! NSColor
        XCTAssertEqual(red.redComponent, 1)
        XCTAssertEqual(red.greenComponent, 0)
        let bg = text.attribute(.backgroundColor, at: 1, effectiveRange: nil) as! NSColor
        XCTAssertEqual(bg.blueComponent, 56.0 / 255, accuracy: 0.001)
        XCTAssertNotNil(text.attribute(.underlineStyle, at: 1, effectiveRange: nil))
        XCTAssertNil(text.attribute(.backgroundColor, at: 2, effectiveRange: nil))
        XCTAssertNil(text.attribute(.underlineStyle, at: 2, effectiveRange: nil))
    }
    func testControlSequencesAndTruncatedInput() {
        XCTAssertEqual(ANSILogText.render("a\u{1b}[2Kb\u{1b}]8;;https://example.com\u{1b}\\link\u{1b}]8;;\u{7}c\u{1b}[31").string, "ablinkc")
        XCTAssertEqual(ANSILogText.render("🙂\n\t中文").string, "🙂\n\t中文")
        XCTAssertEqual(ANSILogText.render("\u{1b}[38;2;999;0;0mX").attribute(.foregroundColor, at: 0, effectiveRange: nil) as? NSColor, NSColor.textColor)
    }
}
