import Capacitor
import Foundation
import Longarm

/// Starts the Longarm Go core (mobile/core) with its config in the app's
/// Application Support directory and resolves with its page's URL
/// (127.0.0.1, token included), which the loader page opens.
@objc(LongarmCorePlugin)
public class LongarmCorePlugin: CAPPlugin, CAPBridgedPlugin {
    public let identifier = "LongarmCorePlugin"
    public let jsName = "LongarmCore"
    public let pluginMethods: [CAPPluginMethod] = [
        CAPPluginMethod(name: "start", returnType: CAPPluginReturnPromise)
    ]

    @objc func start(_ call: CAPPluginCall) {
        DispatchQueue.global(qos: .userInitiated).async {
            do {
                let dir = try FileManager.default.url(
                    for: .applicationSupportDirectory, in: .userDomainMask, appropriateFor: nil, create: true)
                var err: NSError?
                let url = CoreStart(dir.path, &err)
                if let err = err {
                    call.reject(err.localizedDescription)
                    return
                }
                call.resolve(["url": url])
            } catch {
                call.reject(error.localizedDescription)
            }
        }
    }
}
