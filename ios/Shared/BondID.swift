import Foundation

/// Identifiers the app and its tunnel extension must agree on. All of them
/// come from BOND_ID_PREFIX (Config/Bond.xcconfig, overridden in
/// Config/Local.xcconfig), which project.yml, both .entitlements files and
/// both Info.plists are built from.
enum BondID {
    static let prefix = Bundle.main.object(forInfoDictionaryKey: "BondIDPrefix") as? String ?? "com.cartertech"
    static let app = prefix + ".bond"
    static let tunnelBundle = app + ".tunnel"
    static let appGroup = "group." + app
}
