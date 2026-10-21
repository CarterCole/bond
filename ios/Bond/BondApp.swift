import SwiftUI

@main
struct BondApp: App {
    @State private var model = AppModel()

    var body: some Scene {
        WindowGroup {
            Group {
                if let device = model.device {
                    MainView(device: device)
                } else {
                    JoinView()
                }
            }
            .environment(model)
            .task { await model.tunnel.load() }
        }
    }
}
