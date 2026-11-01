import AVFoundation
import Foundation

/// Ogg Opus (RFC 7845) for the hab engine's clips: Opus packets from the
/// system encoder, framed in Ogg pages. A 15 s turn at 24 kbit/s is about 50 KB.
public enum OpusEncoder {
    public static let bitRate = 24_000
    /// 20 ms packets at 16 kHz.
    static let framesPerPacket: AVAudioFrameCount = 320

    public struct Failure: Error, CustomStringConvertible {
        public var description: String
    }

    /// Encodes a clip as Ogg Opus. Throws when the system has no Opus encoder.
    public static func oggOpus(_ clip: SpeechClip) throws -> Data {
        let (packets, preSkip) = try encode(clip.samples)
        return OggOpusWriter.file(packets: packets, preSkip: preSkip, inputSampleRate: UInt32(SpeechClip.sampleRate), sampleCount: clip.samples.count)
    }

    /// Opus packets of 20 ms each, and the encoder's delay in 48 kHz samples.
    static func encode(_ samples: [Float]) throws -> ([Data], UInt16) {
        let rate = Double(SpeechClip.sampleRate)
        guard let pcm = AVAudioFormat(commonFormat: .pcmFormatFloat32, sampleRate: rate, channels: 1, interleaved: false) else {
            throw Failure(description: "no PCM format")
        }
        var desc = AudioStreamBasicDescription(
            mSampleRate: rate, mFormatID: kAudioFormatOpus, mFormatFlags: 0, mBytesPerPacket: 0,
            mFramesPerPacket: framesPerPacket, mBytesPerFrame: 0, mChannelsPerFrame: 1, mBitsPerChannel: 0, mReserved: 0)
        guard let opus = AVAudioFormat(streamDescription: &desc),
              let converter = AVAudioConverter(from: pcm, to: opus) else {
            throw Failure(description: "no Opus encoder")
        }
        converter.bitRate = bitRate
        let leading = converter.primeInfo.leadingFrames
        // The encoder's delay, in 48 kHz samples; 312 is libopus's usual one.
        let preSkip = UInt16(clamping: leading > 0 ? Int(leading) * 3 : 312)

        // Pad to whole packets so the tail is encoded.
        let total = Int(framesPerPacket) * max(1, (samples.count + Int(framesPerPacket) - 1) / Int(framesPerPacket))
        guard let input = AVAudioPCMBuffer(pcmFormat: pcm, frameCapacity: AVAudioFrameCount(total)) else {
            throw Failure(description: "no input buffer")
        }
        input.frameLength = AVAudioFrameCount(total)
        let ch = input.floatChannelData![0]
        ch.initialize(repeating: 0, count: total)
        samples.withUnsafeBufferPointer { src in
            if let b = src.baseAddress { ch.update(from: b, count: samples.count) }
        }

        var packets: [Data] = []
        var supplied = false
        let maxPacket = converter.maximumOutputPacketSize > 0 ? converter.maximumOutputPacketSize : 1500
        while true {
            let out = AVAudioCompressedBuffer(format: opus, packetCapacity: 64, maximumPacketSize: maxPacket)
            var err: NSError?
            let status = converter.convert(to: out, error: &err) { _, st in
                if supplied {
                    st.pointee = .endOfStream
                    return nil
                }
                supplied = true
                st.pointee = .haveData
                return input
            }
            if let err { throw Failure(description: "Opus encode: \(err.localizedDescription)") }
            if let descs = out.packetDescriptions {
                for i in 0..<Int(out.packetCount) {
                    let d = descs[i]
                    packets.append(Data(bytes: out.data.advanced(by: Int(d.mStartOffset)), count: Int(d.mDataByteSize)))
                }
            }
            if status == .endOfStream || status == .error || out.packetCount == 0 { break }
        }
        guard !packets.isEmpty else { throw Failure(description: "the Opus encoder wrote nothing") }
        return (packets, preSkip)
    }
}

/// Frames Opus packets in Ogg pages (RFC 3533, RFC 7845): an ID header page,
/// a comment header page, then audio pages; granule positions count 48 kHz
/// samples.
enum OggOpusWriter {
    /// A packet's length in 48 kHz samples, from its TOC byte (RFC 6716 3.1),
    /// so the granules stay right whatever frame size the encoder picks.
    static func samples48k(_ packet: Data) -> Int64 {
        guard let toc = packet.first else { return 0 }
        let config = Int(toc >> 3)
        let tenthsMs: Int
        switch config {
        case 0...11: tenthsMs = [100, 200, 400, 600][config % 4]
        case 12...15: tenthsMs = [100, 200][config % 2]
        default: tenthsMs = [25, 50, 100, 200][config % 4]
        }
        let frames: Int
        switch toc & 3 {
        case 0: frames = 1
        case 1, 2: frames = 2
        default: frames = packet.count > 1 ? Int(packet[packet.startIndex + 1] & 0x3f) : 0
        }
        return Int64(frames * tenthsMs * 48 / 10)
    }

    static func file(packets: [Data], preSkip: UInt16, inputSampleRate: UInt32, sampleCount: Int, serial: UInt32 = 0x4644_6b31) -> Data {
        var out = Data()
        var seq: UInt32 = 0
        func page(_ segments: [Data], granule: Int64, flags: UInt8) {
            out.append(Self.page(packets: segments, granule: granule, serial: serial, sequence: seq, flags: flags))
            seq += 1
        }
        page([idHeader(preSkip: preSkip, inputSampleRate: inputSampleRate)], granule: 0, flags: 0x02)
        page([commentHeader()], granule: 0, flags: 0)

        // The last granule marks the end of the real audio, so the decoder trims the padding.
        let end = Int64(preSkip) + Int64(sampleCount) * 48_000 / Int64(inputSampleRate)
        var batch: [Data] = []
        var lacing = 0
        var granule = Int64(preSkip)
        var done: Int64 = 0
        for (i, p) in packets.enumerated() {
            let need = p.count / 255 + 1
            if lacing + need > 255, !batch.isEmpty {
                page(batch, granule: min(Int64(preSkip) + done, end), flags: 0)
                batch = []
                lacing = 0
            }
            batch.append(p)
            lacing += need
            done += samples48k(p)
            granule = Int64(preSkip) + done
            if i == packets.count - 1 {
                page(batch, granule: min(granule, end), flags: 0x04)
            }
        }
        return out
    }

    static func idHeader(preSkip: UInt16, inputSampleRate: UInt32) -> Data {
        var d = Data("OpusHead".utf8)
        d.append(1) // version
        d.append(1) // channels
        d.append(le: preSkip)
        d.append(le: inputSampleRate)
        d.append(le: Int16(0)) // output gain
        d.append(0) // mapping family
        return d
    }

    static func commentHeader() -> Data {
        let vendor = Data("Flightdeck".utf8)
        var d = Data("OpusTags".utf8)
        d.append(le: UInt32(vendor.count))
        d.append(vendor)
        d.append(le: UInt32(0)) // no user comments
        return d
    }

    /// One page holding whole packets.
    static func page(packets: [Data], granule: Int64, serial: UInt32, sequence: UInt32, flags: UInt8) -> Data {
        var table: [UInt8] = []
        for p in packets {
            var n = p.count
            while n >= 255 {
                table.append(255)
                n -= 255
            }
            table.append(UInt8(n))
        }
        var d = Data("OggS".utf8)
        d.append(0) // version
        d.append(flags)
        d.append(le: granule)
        d.append(le: serial)
        d.append(le: sequence)
        let crcAt = d.count
        d.append(le: UInt32(0))
        d.append(UInt8(table.count))
        d.append(contentsOf: table)
        for p in packets { d.append(p) }
        let crc = crc32(d)
        d.replaceSubrange(crcAt..<crcAt + 4, with: withUnsafeBytes(of: crc.littleEndian) { Data($0) })
        return d
    }

    /// Ogg's CRC: polynomial 0x04c11db7, initial value 0, not reflected.
    static func crc32(_ data: Data) -> UInt32 {
        var crc: UInt32 = 0
        for b in data {
            crc = (crc << 8) ^ table[Int(((crc >> 24) & 0xff) ^ UInt32(b))]
        }
        return crc
    }

    static let table: [UInt32] = (0..<256).map { i in
        var r = UInt32(i) << 24
        for _ in 0..<8 {
            r = (r & 0x8000_0000) != 0 ? (r << 1) ^ 0x04c1_1db7 : r << 1
        }
        return r
    }
}

extension Data {
    mutating func append<T: FixedWidthInteger>(le v: T) {
        Swift.withUnsafeBytes(of: v.littleEndian) { append(contentsOf: $0) }
    }
}
