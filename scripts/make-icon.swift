// Builds an .iconset from a QuickLook render of app/Resources/AppIcon.svg.
// QuickLook keeps the SVG's blur filters but flattens onto white, while AppKit's own
// SVG renderer keeps alpha but drops the filters. The SVG clips everything to one rounded
// square, so that shape is used as the alpha mask and the white matte is removed.
// usage: swift make-icon.swift <quicklook-render.png> <out.iconset>
import AppKit

let args = CommandLine.arguments
guard args.count == 3, let source = NSBitmapImageRep(data: try! Data(contentsOf: URL(fileURLWithPath: args[1]))) else {
    print("usage: make-icon.swift <render.png> <out.iconset>"); exit(1)
}
let size = 1024
precondition(source.pixelsWide == size && source.pixelsHigh == size, "expected a 1024x1024 render")

// Anti-aliased mask for rect x=32 y=32 w=960 h=960 rx=214 (same numbers as the SVG's clipPath).
let maskRep = NSBitmapImageRep(bitmapDataPlanes: nil, pixelsWide: size, pixelsHigh: size, bitsPerSample: 8, samplesPerPixel: 4, hasAlpha: true, isPlanar: false, colorSpaceName: .deviceRGB, bytesPerRow: 0, bitsPerPixel: 0)!
NSGraphicsContext.saveGraphicsState()
NSGraphicsContext.current = NSGraphicsContext(bitmapImageRep: maskRep)
NSColor.clear.set(); NSRect(x: 0, y: 0, width: size, height: size).fill(using: .copy)
NSColor.white.setFill()
NSBezierPath(roundedRect: NSRect(x: 32, y: 32, width: 960, height: 960), xRadius: 214, yRadius: 214).fill()
NSGraphicsContext.restoreGraphicsState()

let master = NSBitmapImageRep(bitmapDataPlanes: nil, pixelsWide: size, pixelsHigh: size, bitsPerSample: 8, samplesPerPixel: 4, hasAlpha: true, isPlanar: false, colorSpaceName: .deviceRGB, bytesPerRow: 0, bitsPerPixel: 0)!
let s = source.bitmapData!, m = maskRep.bitmapData!, o = master.bitmapData!
let sBpp = source.bitsPerPixel / 8, sRow = source.bytesPerRow, mRow = maskRep.bytesPerRow, oRow = master.bytesPerRow
for y in 0..<size {
    for x in 0..<size {
        let alpha = Double(m[y * mRow + x * 4 + 3]) / 255
        let outIndex = y * oRow + x * 4
        if alpha < 0.004 { o[outIndex] = 0; o[outIndex + 1] = 0; o[outIndex + 2] = 0; o[outIndex + 3] = 0; continue }
        for c in 0..<3 {
            let composite = Double(s[y * sRow + x * sBpp + c])
            // composite = alpha * colour + (1 - alpha) * white  ->  recover the colour
            let colour = min(255, max(0, (composite - (1 - alpha) * 255) / alpha))
            // Store premultiplied, as NSBitmapImageRep expects.
            o[outIndex + c] = UInt8((colour * alpha).rounded())
        }
        o[outIndex + 3] = UInt8((alpha * 255).rounded())
    }
}

try? FileManager.default.removeItem(atPath: args[2])
try! FileManager.default.createDirectory(atPath: args[2], withIntermediateDirectories: true)
let masterImage = NSImage(size: NSSize(width: size, height: size)); masterImage.addRepresentation(master)

func write(_ pixels: Int, _ name: String) {
    let rep = NSBitmapImageRep(bitmapDataPlanes: nil, pixelsWide: pixels, pixelsHigh: pixels, bitsPerSample: 8, samplesPerPixel: 4, hasAlpha: true, isPlanar: false, colorSpaceName: .deviceRGB, bytesPerRow: 0, bitsPerPixel: 0)!
    NSGraphicsContext.saveGraphicsState()
    let context = NSGraphicsContext(bitmapImageRep: rep)!
    NSGraphicsContext.current = context
    context.imageInterpolation = .high
    NSColor.clear.set(); NSRect(x: 0, y: 0, width: pixels, height: pixels).fill(using: .copy)
    masterImage.draw(in: NSRect(x: 0, y: 0, width: pixels, height: pixels), from: .zero, operation: .sourceOver, fraction: 1)
    NSGraphicsContext.restoreGraphicsState()
    try! rep.representation(using: .png, properties: [:])!.write(to: URL(fileURLWithPath: args[2] + "/" + name))
}
for base in [16, 32, 128, 256, 512] {
    write(base, "icon_\(base)x\(base).png")
    write(base * 2, "icon_\(base)x\(base)@2x.png")
}
print("wrote iconset to \(args[2])")
