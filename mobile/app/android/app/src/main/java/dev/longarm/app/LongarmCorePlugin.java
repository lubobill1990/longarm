package dev.longarm.app;

import com.getcapacitor.JSObject;
import com.getcapacitor.Plugin;
import com.getcapacitor.PluginCall;
import com.getcapacitor.PluginMethod;
import com.getcapacitor.annotation.CapacitorPlugin;

/**
 * Starts the Longarm Go core (mobile/core, longarm.aar) with its config in
 * the app's files dir and resolves with its page's URL (127.0.0.1, token
 * included), which the loader page opens. iOS: plugins/longarm-core.
 */
@CapacitorPlugin(name = "LongarmCore")
public class LongarmCorePlugin extends Plugin {
    @PluginMethod
    public void start(PluginCall call) {
        final String files = getContext().getFilesDir().getAbsolutePath();
        new Thread(() -> {
            try {
                JSObject r = new JSObject();
                r.put("url", core.Core.start(files));
                call.resolve(r);
            } catch (Exception e) {
                call.reject(e.getMessage() == null ? e.toString() : e.getMessage());
            }
        }).start();
    }
}
