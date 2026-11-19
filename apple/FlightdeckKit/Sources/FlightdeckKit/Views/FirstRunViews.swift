#if os(iOS)
import SwiftUI

/// First run (app.md 3.9): with no hab in the app, the app opens on "Pair your computer".
struct FirstRunView: View {
    let fleet: Fleet
    @State private var sheet: Sheet?

    enum Sheet: String, Identifiable {
        case scan, type, ssh
        var id: String { rawValue }
    }

    var body: some View {
        ScrollView {
            VStack(alignment: .leading, spacing: 28) {
                header
                steps
                actions
            }
            .padding(.horizontal, 24)
            .padding(.top, 40)
            .padding(.bottom, 24)
            .frame(maxWidth: 560, alignment: .leading)
            .frame(maxWidth: .infinity)
        }
        .background(Color(uiColor: .systemGroupedBackground))
        .sheet(item: $sheet) { s in
            if let ssh = fleet.ssh {
                NavigationStack {
                    switch s {
                    case .scan: PairingView(setup: ssh, offer: nil, startScanning: true) { sheet = nil }
                    case .type: PairingView(setup: ssh, offer: nil) { sheet = nil }
                    case .ssh: AddHabView(setup: ssh, prefill: nil) { sheet = nil }
                    }
                }
            }
        }
    }

    private var header: some View {
        VStack(alignment: .leading, spacing: 10) {
            Image(systemName: "macbook.and.iphone")
                .font(.system(size: 28, weight: .medium))
                .foregroundStyle(.white)
                .frame(width: 56, height: 56)
                .background(Color.accentColor, in: RoundedRectangle(cornerRadius: 16))
                .accessibilityHidden(true)
            Text("Pair your computer")
                .font(.largeTitle.weight(.bold))
                .accessibilityAddTraits(.isHeader)
            Text("Flightdeck shows the agents that run on your computers. Pair one to see its sessions here.")
                .font(.body)
                .foregroundStyle(.secondary)
        }
    }

    private var steps: some View {
        VStack(alignment: .leading, spacing: 0) {
            step(1) {
                Text("On the computer, run")
                    .font(.body.weight(.semibold))
                Text("gff-hab pair")
                    .font(.body.monospaced())
                    .textSelection(.enabled)
                    .padding(.horizontal, 12)
                    .padding(.vertical, 10)
                    .frame(maxWidth: .infinity, alignment: .leading)
                    .background(Color(uiColor: .systemGroupedBackground), in: RoundedRectangle(cornerRadius: 10))
                Text("It shows a code on the screen for 10 minutes.")
                    .font(.subheadline)
                    .foregroundStyle(.secondary)
            }
            Divider().padding(.leading, 58)
            step(2) {
                Text("Scan the code with this \(DeviceLabel.kind)")
                    .font(.body.weight(.semibold))
                Text("The two devices exchange keys. Nothing goes through a server of ours.")
                    .font(.subheadline)
                    .foregroundStyle(.secondary)
            }
        }
        .background(Color(uiColor: .secondarySystemGroupedBackground), in: RoundedRectangle(cornerRadius: 16))
    }

    private func step<Content: View>(_ n: Int, @ViewBuilder content: () -> Content) -> some View {
        HStack(alignment: .top, spacing: 14) {
            Text("\(n)")
                .font(.subheadline.weight(.bold))
                .foregroundStyle(Color.accentColor)
                .frame(width: 28, height: 28)
                .background(Color.accentColor.opacity(0.12), in: Circle())
                .accessibilityLabel("Step \(n)")
            VStack(alignment: .leading, spacing: 8, content: content)
                .frame(maxWidth: .infinity, alignment: .leading)
        }
        .padding(16)
    }

    private var actions: some View {
        VStack(spacing: 12) {
            Button {
                sheet = .scan
            } label: {
                Label("Scan the code", systemImage: "qrcode.viewfinder")
                    .frame(maxWidth: .infinity)
            }
            .buttonStyle(.borderedProminent)
            .disabled(fleet.ssh == nil)
            Button {
                sheet = .type
            } label: {
                Text("Type the code")
                    .frame(maxWidth: .infinity)
            }
            .buttonStyle(.bordered)
            .disabled(fleet.ssh == nil)
            ViewThatFits(in: .horizontal) {
                HStack {
                    sshLink
                    Spacer()
                    demoLink
                }
                VStack(spacing: 12) {
                    sshLink
                    demoLink
                }
            }
            .padding(.top, 4)
        }
        .controlSize(.large)
    }

    private var sshLink: some View {
        Button("Set up over SSH") { sheet = .ssh }
            .buttonStyle(.borderless)
            .disabled(fleet.ssh == nil)
    }

    private var demoLink: some View {
        Button("Look around with demo data") {
            Task { await DemoFleet.start(fleet) }
        }
        .buttonStyle(.borderless)
    }
}
#endif
