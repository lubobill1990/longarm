import Capacitor
import Foundation
import Longarm
import UIKit
import WebKit

/// Starts the Longarm Go core (mobile/core) with its config in the app's
/// Application Support directory and resolves with its page's URL
/// (127.0.0.1, token included; desktop=1: this app has the native desktop
/// viewer), which the loader page opens. The page's
/// longarm-native://desktop?worker=W links open DesktopViewController.
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
        // iOS ends an app's connections while it is away: dial again on return
        NotificationCenter.default.addObserver(forName: UIApplication.willEnterForegroundNotification, object: nil, queue: nil) { _ in
            DispatchQueue.global(qos: .utility).async { CoreReconnect() }
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
                call.resolve(["url": url + "&desktop=1"])
            } catch {
                call.reject(error.localizedDescription)
            }
        }
    }

    override public func shouldOverrideLoad(_ navigationAction: WKNavigationAction) -> NSNumber? {
        guard let url = navigationAction.request.url, url.scheme == "longarm-native" else { return nil }
        if url.host == "desktop",
           let worker = URLComponents(url: url, resolvingAgainstBaseURL: false)?.queryItems?.first(where: { $0.name == "worker" })?.value {
            DispatchQueue.main.async {
                let vc = DesktopViewController(worker: worker)
                vc.modalPresentationStyle = .fullScreen
                self.bridge?.viewController?.present(vc, animated: true)
            }
        }
        return true
    }
}
