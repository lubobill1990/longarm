package dev.longarm.app;

import android.os.Bundle;

import com.getcapacitor.BridgeActivity;

public class MainActivity extends BridgeActivity {
    @Override
    public void onCreate(Bundle savedInstanceState) {
        registerPlugin(LongarmCorePlugin.class); // the Go core (docs/mobile.md)
        super.onCreate(savedInstanceState);
    }
}
