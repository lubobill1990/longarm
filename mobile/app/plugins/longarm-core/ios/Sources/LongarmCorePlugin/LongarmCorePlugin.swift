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

    override public func load() {
        // The Go runtime's iOS start-up (gomobile) makes the app bundle the
        // working directory. Capacitor's router resolves the empty path
        // against it - URL(fileURLWithPath: "") - sees an "app" extension
        // and serves the public directory itself: the first load fails
        // ("The file "public" couldn't be opened"). Back to "/" before it.
        let cwd = FileManager.default.currentDirectoryPath
        if cwd != "/" {
            NSLog("LongarmCore: working directory %@: back to /", cwd)
            FileManager.default.changeCurrentDirectoryPath("/")
        }
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
