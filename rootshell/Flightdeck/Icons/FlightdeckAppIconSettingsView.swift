#if FLIGHTDECK
import SwiftUI
import UIKit

/// FlightDeck's app icon picker: one row per section of
/// `AppIconManager.AppIconVariant.sections`, each opening a list of icons.
/// `AppIconSettingsView` shows this view when FLIGHTDECK is set.
struct FlightdeckAppIconSettingsView: View {
    @ObservedObject private var iconManager = AppIconManager.shared

    var body: some View {
        List {
            Section {
                ForEach(AppIconManager.AppIconVariant.sections) { section in
                    NavigationLink {
                        FlightdeckAppIconSectionView(section: section)
                    } label: {
                        HStack(spacing: 14) {
                            FlightdeckAppIconStack(variants: Array(section.variants.prefix(3)))

                            VStack(alignment: .leading, spacing: 3) {
                                Text(section.title)
                                    .foregroundColor(.primary)
                                Text("\(section.variants.count) icons")
                                    .foregroundColor(.secondary)
                                    .font(.subheadline)
                            }

                            Spacer()

                            if section.variants.contains(iconManager.selectedVariant) {
                                Text(iconManager.selectedVariant.displayName)
                                    .foregroundColor(.secondary)
                                    .font(.subheadline)
                                    .lineLimit(1)
                            }
                        }
                    }
                    .themedRow()
                }
            } footer: {
                Text("Changes the home-screen icon, the icon in Settings and the Live Activity icon.")
            }
        }
        .themedList()
        .navigationTitle("App Icon")
        .navigationBarTitleDisplayMode(.inline)
        .toolbar { SettingsScreenPinMenu(groups: [.theme]) }
    }
}

private struct FlightdeckAppIconSectionView: View {
    let section: AppIconManager.AppIconVariant.Section

    var body: some View {
        List {
            ForEach(section.variants) { variant in
                FlightdeckAppIconButton(variant: variant)
            }
        }
        .themedList()
        .navigationTitle(section.title)
        .navigationBarTitleDisplayMode(.inline)
        .toolbar { SettingsScreenPinMenu(groups: [.theme]) }
    }
}

private struct FlightdeckAppIconButton: View {
    @ObservedObject private var iconManager = AppIconManager.shared
    let variant: AppIconManager.AppIconVariant

    var body: some View {
        Button {
            iconManager.selectedVariant = variant
        } label: {
            HStack(spacing: 14) {
                FlightdeckAppIconImage(variant: variant)
                    .frame(width: 60, height: 60)

                Text(variant.displayName)
                    .foregroundColor(.primary)

                Spacer()

                if iconManager.selectedVariant == variant {
                    Image(systemName: "checkmark")
                        .foregroundColor(.accentColor)
                        .font(.body.weight(.semibold))
                }
            }
            .contentShape(Rectangle())
        }
        .buttonStyle(.plain)
        .themedRow()
    }
}

private struct FlightdeckAppIconStack: View {
    let variants: [AppIconManager.AppIconVariant]

    var body: some View {
        ZStack {
            ForEach(Array(variants.enumerated()), id: \.element.id) { index, variant in
                FlightdeckAppIconImage(variant: variant)
                    .frame(width: 32, height: 32)
                    .offset(x: CGFloat(index) * 14)
                    .zIndex(Double(index))
            }
        }
        .frame(width: 60, height: 36, alignment: .leading)
    }
}

private struct FlightdeckAppIconImage: View {
    let variant: AppIconManager.AppIconVariant

    var body: some View {
        Image(variant.previewAssetName)
            .resizable()
            .interpolation(.high)
            .aspectRatio(contentMode: .fit)
    }
}
#endif
