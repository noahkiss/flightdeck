import CoreGraphics
import Foundation
import HabAPI
import ImageIO
import UniformTypeIdentifiers

/// Invented rich content for demo mode (app.md 4.7): a chart image and a
/// notes document the agent showed, and the files it changed. Every name,
/// path and line here is made up.
enum DemoContent {
    static let root = "/work/tides"

    static func item(_ session: String, _ rel: String, kind: ContentKind, type: String, size: Int?, minutesAgo: Double?,
                     title: String? = nil, shown: Bool = false, change: ContentChange? = nil, message: String? = nil) -> ContentItem {
        let path = root + "/" + rel
        return ContentItem(at: minutesAgo.map { Fixture.stamp(DemoFleet.ago($0)) } ?? "", change: change, id: "demo-" + rel,
                           kind: kind, message: message, name: (rel as NSString).lastPathComponent, path: path,
                           session: session, shown: shown ? true : nil, size: size, title: title, type: type)
    }

    static let notes = """
    # Tide chart

    The station page now draws the last 48 hours of readings.

    - Heights come from the `readings` table, one query per station.
    - High and low tides are marked where the curve turns.
    - The new index keeps the query under 20 ms on the largest station.

    ## Next

    Run the migration on the staging database before release.
    """

    static let stationsDiff = """
    diff --git a/src/api/stations.ts b/src/api/stations.ts
    --- a/src/api/stations.ts
    +++ b/src/api/stations.ts
    @@ -12,6 +12,7 @@ import { station } from './handlers'
     const router = Router()

     router.get('/stations/:id', station)
    +router.get('/stations/:id/readings', readings)

     export default router
    """

    static let chartDiff = """
    --- /dev/null
    +++ b/src/ui/TideChart.tsx
    @@ -0,0 +1,6 @@
    +export function TideChart({ readings }: Props) {
    +  const points = readings.map(r => [r.at, r.height_m])
    +  return <Line points={points} marks={turns(points)} />
    +}
    +
    +export default TideChart
    """

    /// A small invented chart, drawn here so the demo needs no image file.
    static func chartPNG() -> Data {
        let w = 480, h = 240
        guard let ctx = CGContext(data: nil, width: w, height: h, bitsPerComponent: 8, bytesPerRow: 0,
                                  space: CGColorSpaceCreateDeviceRGB(), bitmapInfo: CGImageAlphaInfo.premultipliedLast.rawValue) else { return Data() }
        ctx.setFillColor(CGColor(red: 0.96, green: 0.97, blue: 0.99, alpha: 1))
        ctx.fill(CGRect(x: 0, y: 0, width: w, height: h))
        ctx.setStrokeColor(CGColor(red: 0.8, green: 0.83, blue: 0.88, alpha: 1))
        for y in stride(from: 40, to: h, by: 40) {
            ctx.move(to: CGPoint(x: 0, y: y))
            ctx.addLine(to: CGPoint(x: w, y: y))
        }
        ctx.strokePath()
        ctx.setStrokeColor(CGColor(red: 0.1, green: 0.42, blue: 0.85, alpha: 1))
        ctx.setLineWidth(4)
        for x in 0...w {
            let y = Double(h) / 2 + 70 * sin(Double(x) / Double(w) * 4 * .pi)
            if x == 0 { ctx.move(to: CGPoint(x: 0, y: y)) } else { ctx.addLine(to: CGPoint(x: Double(x), y: y)) }
        }
        ctx.strokePath()
        guard let img = ctx.makeImage() else { return Data() }
        let out = NSMutableData()
        guard let dest = CGImageDestinationCreateWithData(out, UTType.png.identifier as CFString, 1, nil) else { return Data() }
        CGImageDestinationAddImage(dest, img, nil)
        CGImageDestinationFinalize(dest)
        return out as Data
    }

    static func populate(_ hab: FakeHab, session s: String) async {
        let png = chartPNG()
        let md = Data(notes.utf8)
        let chart = item(s, "docs/tide-chart.png", kind: .image, type: "image/png", size: png.count, minutesAgo: 3.5,
                         title: "Tide chart, station 4", shown: true)
        let doc = item(s, "docs/notes.md", kind: .markdown, type: "text/markdown", size: md.count, minutesAgo: 3.4,
                       title: "Release notes", shown: true)
        let stations = item(s, "src/api/stations.ts", kind: .text, type: "text/plain", size: 220, minutesAgo: 10,
                            change: .modified, message: "\(s)-m7")
        let ui = item(s, "src/ui/TideChart.tsx", kind: .text, type: "text/plain", size: 180, minutesAgo: nil, change: .untracked)
        await hab.setContent(session: s, items: [stations, chart, doc, ui],
                             files: [chart.id: png, doc.id: md],
                             diffs: [stations.id: stationsDiff, ui.id: chartDiff])
    }
}
