import SwiftUI

/// The root view until the status board lands: the app name and the API
/// revision. Shared by the macOS and iOS targets.
public struct PlaceholderView: View {
    public init() {}

    public var body: some View {
        VStack(spacing: 8) {
            Text(AppInfo.displayName())
                .font(.largeTitle.weight(.semibold))
            Text("API revision \(AppInfo.apiRevision)")
                .font(.callout)
                .foregroundStyle(.secondary)
        }
        .padding(40)
        .frame(minWidth: 320, minHeight: 200)
    }
}
