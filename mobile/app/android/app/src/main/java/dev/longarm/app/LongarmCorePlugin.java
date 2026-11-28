package dev.longarm.app;

import android.content.Intent;
import android.net.Uri;

import com.getcapacitor.JSObject;
import com.getcapacitor.Plugin;
import com.getcapacitor.PluginCall;
import com.getcapacitor.PluginMethod;
import com.getcapacitor.annotation.CapacitorPlugin;

/**
 * Starts the Longarm Go core (mobile/core, longarm.aar) with its config in
 * the app's files dir and resolves with its page's URL (127.0.0.1, token
 * included; desktop=1: this app has the native desktop viewer), which the
 * loader page opens. The page's longarm-native://desktop?worker=W links
 * open the viewer (DesktopActivity). iOS: plugins/longarm-core.
 */
@CapacitorPlugin(name = "LongarmCore")
public class LongarmCorePlugin extends Plugin {
    @PluginMethod
    public void start(PluginCall call) {
        final String files = getContext().getFilesDir().getAbsolutePath();
        new Thread(() -> {
            try {
                JSObject r = new JSObject();
                r.put("url", core.Core.start(files) + "&desktop=1");
                call.resolve(r);
            } catch (Exception e) {
                call.reject(e.getMessage() == null ? e.toString() : e.getMessage());
            }
        }).start();
    }

    @Override
    public Boolean shouldOverrideLoad(Uri url) {
        if (!"longarm-native".equals(url.getScheme())) return null;
        if ("desktop".equals(url.getHost())) {
            Intent i = new Intent(getContext(), DesktopActivity.class);
            i.putExtra(DesktopActivity.EXTRA_WORKER, url.getQueryParameter("worker"));
            getActivity().startActivity(i);
        }
        return true;
    }

    @Override
    protected void handleOnResume() {
        // the system may have ended the connections while the app was away
        new Thread(core.Core::reconnect).start();
    }
}
