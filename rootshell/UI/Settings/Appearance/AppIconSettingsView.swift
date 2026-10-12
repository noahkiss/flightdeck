import SwiftUI
import UIKit

/// Lets the user pick between the primary and alternate app icons. Hidden on
/// platforms where `UIApplication.supportsAlternateIcons` is false (visionOS).
struct AppIconSettingsView: View {
    @ObservedObject private var iconManager = AppIconManager.shared
    private let radicalCollection = AppIconManager.AppIconVariant.radicalOfTheUnknownVariants

    private var selectedRadicalVariant: AppIconManager.AppIconVariant? {
        radicalCollection.first { $0 == iconManager.selectedVariant }
    }

    var body: some View {
        #if FLIGHTDECK // FlightDeck's picker: rootshell/Flightdeck/Icons/
        FlightdeckAppIconSettingsView()
        #else
        List {
            Section {
                ForEach(AppIconManager.AppIconVariant.standardVariants) { variant in
                    AppIconVariantButton(variant: variant)
                }
            } footer: {
                Text(String(
                    localized: "Changes the home-screen icon. Updates here also apply to the animated icon in Settings and the Live Activity widget.",
                    comment: "Footer explaining what the app icon picker affects"
                ))
            }

            Section {
                NavigationLink {
                    AppIconCollectionSettingsView(
                        title: String(
                            localized: "Radical of the Unknown",
                            comment: "Settings navigation title: Radical of the Unknown app icon collection"
                        ),
                        variants: radicalCollection
                    )
                } label: {
                    HStack(spacing: 14) {
                        AppIconCollectionPreview(variants: Array(radicalCollection.prefix(3)))

                        VStack(alignment: .leading, spacing: 3) {
                            Text(String(
                                localized: "Radical of the Unknown",
                                comment: "Settings row title: Radical of the Unknown app icon collection"
                            ))
                            .foregroundColor(.primary)

                            Text(String(
                                localized: "12 colorways",
                                comment: "Settings row subtitle: number of app icon colorways in a collection"
                            ))
                            .foregroundColor(.secondary)
                            .font(.subheadline)
                        }

                        Spacer()

                        if let selectedRadicalVariant {
                            Text(selectedRadicalVariant.displayName)
                                .foregroundColor(.secondary)
                                .font(.subheadline)
                                .lineLimit(1)
                        }
                    }
                }
                .themedRow()
            } header: {
                Text(String(
                    localized: "Collections",
                    comment: "Settings section title: app icon collections"
                ))
            }
        }
        .themedList()
        .navigationTitle(String(localized: "App Icon", comment: "Settings navigation title: app icon picker"))
        .navigationBarTitleDisplayMode(.inline)
        .toolbar { SettingsScreenPinMenu(groups: [.theme]) }
        #endif
    }
}

private struct AppIconCollectionSettingsView: View {
    let title: String
    let variants: [AppIconManager.AppIconVariant]

    var body: some View {
        List {
            Section {
                ForEach(variants) { variant in
                    AppIconVariantButton(variant: variant)
                }
            } footer: {
                Text(String(
                    localized: "Changes the home-screen icon. Updates here also apply to the animated icon in Settings and the Live Activity widget.",
                    comment: "Footer explaining what the app icon collection picker affects"
                ))
            }
        }
        .themedList()
        .navigationTitle(title)
        .navigationBarTitleDisplayMode(.inline)
        .toolbar { SettingsScreenPinMenu(groups: [.theme]) }
    }
}

private struct AppIconVariantButton: View {
    @ObservedObject private var iconManager = AppIconManager.shared
    let variant: AppIconManager.AppIconVariant

    var body: some View {
        Button {
            iconManager.selectedVariant = variant
        } label: {
            HStack(spacing: 14) {
                AppIconPreview(variant: variant)
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

private struct AppIconCollectionPreview: View {
    let variants: [AppIconManager.AppIconVariant]

    var body: some View {
        ZStack {
            ForEach(Array(variants.enumerated()), id: \.element.id) { index, variant in
                AppIconPreview(variant: variant)
                    .frame(width: 32, height: 32)
                    .offset(x: CGFloat(index) * 14)
                    .zIndex(Double(index))
            }
        }
        .frame(width: 60, height: 36, alignment: .leading)
    }
}

private struct AppIconPreview: View {
    let variant: AppIconManager.AppIconVariant

    var body: some View {
        Image(variant.previewAssetName)
            .resizable()
            .interpolation(.high)
            .aspectRatio(contentMode: .fit)
    }
}
