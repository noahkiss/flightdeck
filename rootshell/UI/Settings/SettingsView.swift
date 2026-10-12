//
//  SettingsView.swift
//  rootshell
//
//  Settings view for configuring Ghostty
//

import SwiftUI
import UniformTypeIdentifiers

/// Navigation destinations for programmatic push (e.g., from Shortcuts intents).
enum SettingsDestination: Hashable {
    case vpn
    /// Opened by the iPhone keyboard chooser; pushes Terminal › Terminal Keyboard.
    case touchKeyboard
}

/// Sidebar sections for the iPad split-view settings layout.
enum SettingsSection: String, CaseIterable, Identifiable {
    case appearance
    case terminal
    case connections
    case aiAssistant
    /// Privacy & data, notifications, sounds and updates.
    case general
    case vpn
    case about

    var id: String { rawValue }

    var title: String {
        switch self {
        case .appearance: String(localized: "Appearance", comment: "Settings section title")
        case .terminal: String(localized: "Terminal", comment: "Settings section title")
        case .connections: String(localized: "Connections", comment: "Settings section title")
        case .aiAssistant: String(localized: "AI Assistant", comment: "Settings section title")
        case .general: String(localized: "General", comment: "Settings section title")
        case .vpn: String(localized: "VPN", comment: "Settings section title")
        case .about: String(localized: "About", comment: "Settings section title")
        }
    }

    var icon: String {
        switch self {
        case .appearance: "paintpalette"
        case .terminal: "terminal"
        case .connections: "network"
        case .aiAssistant: "sparkles"
        case .general: "gearshape"
        case .vpn: "network.badge.shield.half.filled"
        case .about: "info.circle"
        }
    }
}

struct SettingsHomeList: View {
    @Binding var showDebugSettings: Bool
    @Setting(Settings.System.screenshotMode) private var screenshotMode

    #if !CHINA_BUILD
    /// "Active" while a VPN or in-app Tailscale runs; "On" while in-app
    /// Tailscale waits to start on demand.
    private var vpnRowState: String {
        if VPNManager.shared.status.isActive || TailnetInAppEngine.shared.isStarted {
            return String(localized: "Active", comment: "Settings VPN row: a VPN or Tailscale is running")
        }
        let mode = TailnetModeSettings.load()
        if mode.effectiveMode == .rootshellOnly, mode.inAppEnabled {
            return String(localized: "On", comment: "Settings VPN row: in-app Tailscale waits to start")
        }
        return String(localized: "Off", comment: "Settings VPN row: nothing running")
    }
    #endif

    var body: some View {
        List {
            Section {
                ForEach(SettingsSection.allCases.filter { $0 != .about && $0.isAvailable }) { section in
                    NavigationLink(value: section) {
                        HStack(spacing: 12) {
                            SettingsIcon(systemName: section.icon)
                            Text(section.title)
                            #if !CHINA_BUILD
                            if section == .vpn {
                                Spacer()
                                Text(vpnRowState)
                                    .foregroundColor(.secondary)
                                    .font(.subheadline)
                            }
                            #endif
                        }
                    }
                    .themedRow()
                }
            }

            Section {
                VStack(spacing: 12) {
                    AnimatedAboutIcon(
                        onTap: {},
                        onLongPress: {
                            showDebugSettings = true
                        }
                    )

                    Text(verbatim: FlightdeckBrand.name)
                        .font(.headline)

                    Link("Based on rootshell by Kit Knox", destination: URL(string: "https://github.com/kitknox/rootshell")!)
                        .font(.subheadline)
                        .foregroundColor(.secondary)

                }
                .frame(maxWidth: .infinity, alignment: .center)
                .padding(.vertical, 8)
                .themedRow()

                if !screenshotMode {
                    HStack(spacing: 12) {
                        SettingsIcon(systemName: "info.circle")
                        Text("Version")
                        Spacer()
                        let version = Bundle.main.infoDictionary?["CFBundleShortVersionString"] as? String ?? "Unknown"
                        let build = Bundle.main.infoDictionary?["CFBundleVersion"] as? String ?? "Unknown"
                        VStack(alignment: .trailing, spacing: 2) {
                            Text("\(version) (\(build))")
                            Text(BuildInfo.date)
                        }
                        .foregroundColor(.secondary)
                        .font(.subheadline)
                        .textSelection(.enabled)
                    }
                    .themedRow()
                }

                SettingsReviewLink()

                NavigationLink(value: SettingsSearchDestination.acknowledgements) {
                    HStack(spacing: 12) {
                        SettingsIcon(systemName: "doc.text")
                        Text("Acknowledgements")
                    }
                }
                .themedRow()
            } footer: {
                SettingsOpenSourceFooter()
            }
        }
        .themedList()
        // On the List, not in it: destinations registered inside lazy list
        // content aren't reliably picked up, and the debug screen is pushed by
        // the tap-count gesture above rather than by a visible row.
        .navigationDestination(isPresented: $showDebugSettings) {
            DebugSettingsView()
        }
    }
}

struct SettingsFloatingSearchChrome: View {
    @Environment(\.sheetThemeColors) private var sheetThemeColors
    @Environment(\.horizontalSizeClass) private var horizontalSizeClass
    @FocusState private var isSearchFieldFocused: Bool

    @Binding var reservedHeight: CGFloat
    let onSelect: (SettingsSearchEntry) -> Void

    @State private var isPresented = false
    @State private var searchText = ""
    @State private var collapsedPillHeight: CGFloat = 0

    private var displayedEntries: [SettingsSearchEntry] {
        SettingsSearchEntry.filtered(for: searchText)
    }

    private var chromeHorizontalPadding: CGFloat {
        horizontalSizeClass == .regular ? 20 : 16
    }

    private var panelBackground: some ShapeStyle {
        if let sheetThemeColors {
            return AnyShapeStyle(sheetThemeColors.rowBackground.opacity(0.97))
        }
        return AnyShapeStyle(.ultraThinMaterial)
    }

    private var fieldBackground: Color {
        sheetThemeColors?.background.opacity(0.7) ?? Color(uiColor: .secondarySystemBackground)
    }

    private var borderColor: Color {
        sheetThemeColors == nil ? Color.white.opacity(0.2) : Color.primary.opacity(0.08)
    }

    var body: some View {
        GeometryReader { proxy in
            ZStack {
                if isPresented {
                    Color.black.opacity(sheetThemeColors == nil ? 0.14 : 0.24)
                        .ignoresSafeArea()
                        .onTapGesture {
                            dismissSearch()
                        }
                        .transition(.opacity)

                    expandedPanel
                        .transition(.move(edge: .bottom).combined(with: .opacity))
                        .frame(maxWidth: .infinity, maxHeight: .infinity, alignment: .bottom)
                        .padding(.horizontal, chromeHorizontalPadding)
                        .padding(.bottom, 12)
                        .ignoresSafeArea(.container, edges: .bottom)
                } else {
                    // Let layout use the pill's intrinsic height immediately.
                    // Positioning from its preference measurement required a
                    // second pass and made the bar jump during presentation.
                    collapsedPill
                        .frame(width: collapsedPillWidth(for: proxy.size.width))
                        .padding(.bottom, collapsedPillBottomInset(for: proxy.safeAreaInsets.bottom))
                        .frame(maxWidth: .infinity, maxHeight: .infinity, alignment: .bottom)
                        .transition(.move(edge: .bottom).combined(with: .opacity))
                }
            }
            .frame(maxWidth: .infinity, maxHeight: .infinity)
            .animation(.spring(response: 0.34, dampingFraction: 0.9), value: isPresented)
            .onAppear {
                updateReservedHeight(bottomSafeArea: proxy.safeAreaInsets.bottom)
            }
            .onChange(of: collapsedPillHeight) { _, _ in
                updateReservedHeight(bottomSafeArea: proxy.safeAreaInsets.bottom)
            }
            .onChange(of: proxy.safeAreaInsets.bottom) { _, bottomSafeArea in
                updateReservedHeight(bottomSafeArea: bottomSafeArea)
            }
            .onChange(of: isPresented) { _, _ in
                updateReservedHeight(bottomSafeArea: proxy.safeAreaInsets.bottom)
            }
        }
        // Terminal keyboard dismissal must not move the collapsed bar. Once
        // search opens, its panel follows the search field's keyboard normally.
        .ignoresSafeArea(isPresented ? [] : .keyboard, edges: .bottom)
        .onChange(of: isPresented) { _, newValue in
            if newValue {
                DispatchQueue.main.async {
                    isSearchFieldFocused = true
                }
            } else {
                isSearchFieldFocused = false
                searchText = ""
            }
        }
    }

    private var collapsedPill: some View {
        Button {
            isPresented = true
        } label: {
            HStack(spacing: 12) {
                Image(systemName: "magnifyingglass")
                    .font(.system(size: 16, weight: .semibold))
                    .foregroundStyle(.secondary)

                Text("Search Settings")
                    .foregroundStyle(.primary)

                Spacer(minLength: 12)

                Image(systemName: "chevron.up")
                    .font(.system(size: 13, weight: .semibold))
                    .foregroundStyle(.secondary)
            }
            .padding(.horizontal, 18)
            .padding(.vertical, 16)
            .background(panelBackground, in: Capsule())
            .overlay(
                Capsule()
                    .stroke(borderColor, lineWidth: 1)
            )
            .shadow(color: Color.black.opacity(0.12), radius: 18, y: 8)
        }
        .buttonStyle(.plain)
        .accessibilityLabel("Search Settings")
        .background {
            GeometryReader { proxy in
                Color.clear
                    .preference(key: SettingsFloatingSearchBarHeightPreferenceKey.self, value: proxy.size.height)
            }
        }
        .onPreferenceChange(SettingsFloatingSearchBarHeightPreferenceKey.self) { height in
            guard height > 0 else { return }
            collapsedPillHeight = height
        }
    }

    private var expandedPanel: some View {
        VStack(alignment: .leading, spacing: 14) {
            HStack(spacing: 10) {
                HStack(spacing: 10) {
                    Image(systemName: "magnifyingglass")
                        .foregroundStyle(.secondary)

                    TextField("Search settings", text: $searchText)
                        .textInputAutocapitalization(.words)
                        .disableAutocorrection(true)
                        .focused($isSearchFieldFocused)
                        .submitLabel(.search)
                        .onSubmit {
                            guard let firstEntry = displayedEntries.first else { return }
                            select(firstEntry)
                        }

                    if !searchText.isEmpty {
                        Button {
                            searchText = ""
                        } label: {
                            Image(systemName: "xmark.circle.fill")
                                .foregroundStyle(.secondary)
                        }
                        .buttonStyle(.plain)
                    }
                }
                .padding(.horizontal, 14)
                .padding(.vertical, 12)
                .background(fieldBackground, in: Capsule())

                Button("Cancel") {
                    dismissSearch()
                }
                .buttonStyle(.plain)
                .foregroundStyle(.tint)
            }

            Text(searchText.trimmingCharacters(in: .whitespacesAndNewlines).isEmpty ? "Suggested" : "Results")
                .font(.caption)
                .foregroundStyle(.secondary)
                .padding(.horizontal, 2)

            if displayedEntries.isEmpty {
                ContentUnavailableView(
                    "No Matches",
                    systemImage: "magnifyingglass",
                    description: Text("Try a broader term like SSH, theme, VPN, or notifications.")
                )
                .frame(maxWidth: .infinity)
                .padding(.vertical, 24)
            } else {
                ScrollView {
                    LazyVStack(spacing: 10) {
                        ForEach(displayedEntries) { entry in
                            Button {
                                select(entry)
                            } label: {
                                HStack(spacing: 12) {
                                    SettingsIcon(systemName: entry.systemImage)

                                    VStack(alignment: .leading, spacing: 3) {
                                        Text(entry.title)
                                            .foregroundStyle(.primary)
                                            .frame(maxWidth: .infinity, alignment: .leading)

                                        Text(entry.subtitle)
                                            .font(.subheadline)
                                            .foregroundStyle(.secondary)
                                            .frame(maxWidth: .infinity, alignment: .leading)
                                    }

                                    Image(systemName: "chevron.right")
                                        .font(.system(size: 13, weight: .semibold))
                                        .foregroundStyle(.tertiary)
                                }
                                .padding(.horizontal, 14)
                                .padding(.vertical, 12)
                                .background(fieldBackground, in: RoundedRectangle(cornerRadius: 18, style: .continuous))
                            }
                            .buttonStyle(.plain)
                        }
                    }
                    .padding(.bottom, 4)
                }
                .frame(maxHeight: 320)
            }
        }
        .padding(16)
        .background(panelBackground, in: RoundedRectangle(cornerRadius: 28, style: .continuous))
        .overlay(
            RoundedRectangle(cornerRadius: 28, style: .continuous)
                .stroke(borderColor, lineWidth: 1)
        )
        .shadow(color: Color.black.opacity(0.14), radius: 24, y: 10)
    }

    private func dismissSearch() {
        isPresented = false
    }

    private func collapsedPillWidth(for containerWidth: CGFloat) -> CGFloat {
        max(0, min(containerWidth - (chromeHorizontalPadding * 2), 520))
    }

    private func collapsedPillBottomInset(for bottomSafeArea: CGFloat) -> CGFloat {
        #if targetEnvironment(macCatalyst)
        return 8
        #else
        if UIDevice.current.userInterfaceIdiom == .phone {
            // Hug the bottom more aggressively on iPhone while still staying visible.
            return max(2, bottomSafeArea - 32)
        } else {
            return bottomSafeArea > 0 ? max(8, bottomSafeArea - 12) : 8
        }
        #endif
    }

    private func updateReservedHeight(bottomSafeArea: CGFloat) {
        // Measurement only reserves list clearance; it never positions the bar.
        guard !isPresented, collapsedPillHeight > 0 else { return }
        let height = collapsedPillHeight + max(8, collapsedPillBottomInset(for: bottomSafeArea))
        if abs(reservedHeight - height) > 0.5 {
            reservedHeight = height
        }
    }

    private func select(_ entry: SettingsSearchEntry) {
        let selectedEntry = entry
        dismissSearch()
        DispatchQueue.main.asyncAfter(deadline: .now() + 0.05) {
            onSelect(selectedEntry)
        }
    }
}

private struct SettingsFloatingSearchBarHeightPreferenceKey: PreferenceKey {
    static var defaultValue: CGFloat = 0

    static func reduce(value: inout CGFloat, nextValue: () -> CGFloat) {
        value = max(value, nextValue())
    }
}

struct SettingsView: View {
    var initialDestination: SettingsDestination? = nil

    @Environment(\.dismiss) var dismiss
    @State private var navigationPath = NavigationPath()
    @State private var hasNavigatedToInitialDestination = false

    @State private var showDebugSettings = false
    @State private var searchReservedHeight: CGFloat = 88

    private var showsRootSearch: Bool {
        navigationPath.isEmpty
    }

    var body: some View {
        ZStack(alignment: .bottom) {
            NavigationStack(path: $navigationPath) {
                SettingsHomeList(showDebugSettings: $showDebugSettings)
                    .safeAreaInset(edge: .bottom) {
                        if showsRootSearch {
                            Color.clear.frame(height: searchReservedHeight)
                        }
                    }
                    .navigationTitle("Settings")
                    .navigationBarTitleDisplayMode(.inline)
                    .toolbar {
                        ToolbarItem(placement: .navigationBarTrailing) {
                            Button("Done") {
                                dismiss()
                            }
                        }
                    }
                    .navigationDestination(for: SettingsSection.self) { section in
                        sectionDetail(for: section)
                    }
                    .navigationDestination(for: SettingsDestination.self) { destination in
                        switch destination {
                        case .vpn:
                            #if !CHINA_BUILD
                            VPNSettingsView()
                            #else
                            EmptyView()
                            #endif
                        case .touchKeyboard:
                            settingsSearchDestinationView(for: .touchKeyboard)
                        }
                    }
                    .navigationDestination(for: SettingsSearchDestination.self) { destination in
                        settingsSearchDestinationView(for: destination)
                    }
                    .onAppear {
                        handleInitialDestination()
                    }
            }

            if showsRootSearch {
                SettingsFloatingSearchChrome(
                    reservedHeight: $searchReservedHeight,
                    onSelect: handleSearchSelection
                )
            }
        }
    }

    @ViewBuilder
    private func sectionDetail(for section: SettingsSection) -> some View {
        switch section {
        case .appearance:
            SettingsAppearanceSection()
        case .terminal:
            SettingsTerminalSection()
        case .connections:
            SettingsConnectionsSection()
        case .aiAssistant:
            SettingsAISection()
        case .general:
            SettingsGeneralSection()
        case .vpn:
            #if !CHINA_BUILD
            VPNSettingsView()
            #else
            EmptyView()
            #endif
        case .about:
            SettingsAboutSection()
        }
    }

    private func handleInitialDestination() {
        guard let initialDestination, !hasNavigatedToInitialDestination else { return }
        hasNavigatedToInitialDestination = true
        // Delay navigation by one run loop tick so the NavigationStack
        // finishes its initial layout.
        DispatchQueue.main.async {
            switch initialDestination {
            case .vpn:
                navigationPath.append(SettingsSection.vpn)
            case .touchKeyboard:
                navigationPath.append(SettingsSection.terminal)
                navigationPath.append(SettingsSearchDestination.touchKeyboard)
            }
        }
    }

    private func handleSearchSelection(_ entry: SettingsSearchEntry) {
        switch entry.action {
        case .section(let section):
            navigationPath.append(section)
        case .destination(let destination):
            navigationPath.append(destination)
        }
    }
}

#Preview {
    SettingsView()
}
