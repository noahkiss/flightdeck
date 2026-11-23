#if os(iOS)
import AVFoundation
import SwiftUI
import UIKit

/// The camera, reading QR codes. Calls `found` once with the first code.
struct QRScannerView: UIViewControllerRepresentable {
    let found: (String) -> Void

    func makeUIViewController(context: Context) -> ScannerController {
        let c = ScannerController()
        c.found = found
        return c
    }

    func updateUIViewController(_ c: ScannerController, context: Context) {}

    final class ScannerController: UIViewController, AVCaptureMetadataOutputObjectsDelegate {
        var found: ((String) -> Void)?
        private let session = AVCaptureSession()
        /// Starts and stops the session in order, off the main thread, as AVFoundation asks.
        private let sessionQueue = DispatchQueue(label: "flightdeck.qr.session")
        private var preview: AVCaptureVideoPreviewLayer?
        private var done = false
        private let label = UILabel()

        override func viewDidLoad() {
            super.viewDidLoad()
            view.backgroundColor = .black
            label.textColor = .white
            label.numberOfLines = 0
            label.textAlignment = .center
            label.font = .preferredFont(forTextStyle: .callout)
            label.translatesAutoresizingMaskIntoConstraints = false
            view.addSubview(label)
            NSLayoutConstraint.activate([
                label.centerXAnchor.constraint(equalTo: view.centerXAnchor),
                label.centerYAnchor.constraint(equalTo: view.centerYAnchor),
                label.leadingAnchor.constraint(greaterThanOrEqualTo: view.leadingAnchor, constant: 16),
            ])
            switch AVCaptureDevice.authorizationStatus(for: .video) {
            case .authorized:
                configure()
            case .notDetermined:
                AVCaptureDevice.requestAccess(for: .video) { ok in
                    DispatchQueue.main.async { ok ? self.configure() : self.show("Camera access is off. Paste the code below.") }
                }
            default:
                show("Camera access is off. Turn it on in Settings, or paste the code below.")
            }
        }

        override func viewDidLayoutSubviews() {
            super.viewDidLayoutSubviews()
            preview?.frame = view.bounds
        }

        override func viewWillDisappear(_ animated: Bool) {
            super.viewWillDisappear(animated)
            let s = CaptureSessionHandle(session)
            sessionQueue.async { if s.session.isRunning { s.session.stopRunning() } }
        }

        private func show(_ text: String) {
            label.text = text
        }

        private func configure() {
            guard let device = AVCaptureDevice.default(for: .video),
                  let input = try? AVCaptureDeviceInput(device: device),
                  session.canAddInput(input)
            else {
                show("No camera. Paste the code below.")
                return
            }
            session.addInput(input)
            let output = AVCaptureMetadataOutput()
            guard session.canAddOutput(output) else {
                show("The camera cannot read codes. Paste the code below.")
                return
            }
            session.addOutput(output)
            output.setMetadataObjectsDelegate(self, queue: .main)
            output.metadataObjectTypes = [.qr]
            let layer = AVCaptureVideoPreviewLayer(session: session)
            layer.videoGravity = .resizeAspectFill
            layer.frame = view.bounds
            view.layer.insertSublayer(layer, at: 0)
            preview = layer
            let s = CaptureSessionHandle(session)
            sessionQueue.async { s.session.startRunning() }
        }

        func metadataOutput(_ output: AVCaptureMetadataOutput, didOutput objects: [AVMetadataObject], from connection: AVCaptureConnection) {
            guard !done, let code = (objects.first as? AVMetadataMachineReadableCodeObject)?.stringValue else { return }
            done = true
            let s = CaptureSessionHandle(session)
            sessionQueue.async { s.session.stopRunning() }
            found?(code)
        }
    }
}

/// The capture session, handed to the session queue. AVCaptureSession is not marked
/// Sendable, but AVFoundation documents it as safe to start and stop from another thread
/// and asks for exactly that; the queue only calls `startRunning`, `stopRunning` and
/// `isRunning`, one at a time.
private struct CaptureSessionHandle: @unchecked Sendable {
    let session: AVCaptureSession
    init(_ session: AVCaptureSession) { self.session = session }
}
#endif
