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

    // Spike diagnostics: with the Go runtime in the process, the WebView's
    // first load failed (NSCocoaErrorDomain 256): do file reads fail, and why?
    override public func load() {
        setvbuf(stdout, nil, _IONBF, 0) // Capacitor's log (print) reaches simctl's --stdout file
        NSLog("LongarmCore: app location %@", bridge?.config.appLocation.path ?? "?")
        guard let u = Bundle.main.url(forResource: "index", withExtension: "html", subdirectory: "public") else {
            NSLog("LongarmCore: no public/index.html")
            return
        }
        var fails = 0
        for _ in 0..<200 {
            do {
                _ = try Data(contentsOf: u)
            } catch {
                fails += 1
                if fails <= 3 { NSLog("LongarmCore: read failed: %@", String(describing: error)) }
            }
        }
        NSLog("LongarmCore: %d of 200 reads of index.html failed", fails)
    }

    @objc func start(_ call: CAPPluginCall) {
        DispatchQueue.global(qos: .userInitiated).async {
            do {
                let dir = try FileManager.default.url(
                    for: .applicationSupportDirectory, in: .userDomainMask, appropriateFor: nil, create: true)
                var err: NSError?
                let url = CoreStart(dir.path, &err)
                if let err = err {
                    NSLog("LongarmCore: start: %@", err.localizedDescription)
                    call.reject(err.localizedDescription)
                    return
                }
                NSLog("LongarmCore: started at %@", url.components(separatedBy: "?").first ?? "")
                call.resolve(["url": url])
            } catch {
                call.reject(error.localizedDescription)
            }
        }
    }
}
