package dev.longarm.app;

import android.app.Activity;
import android.content.Context;
import android.graphics.Bitmap;
import android.graphics.Color;
import android.graphics.Matrix;
import android.graphics.SurfaceTexture;
import android.media.MediaCodec;
import android.media.MediaFormat;
import android.os.Bundle;
import android.os.Handler;
import android.os.Looper;
import android.text.InputType;
import android.view.GestureDetector;
import android.view.Gravity;
import android.view.KeyEvent;
import android.view.MotionEvent;
import android.view.ScaleGestureDetector;
import android.view.Surface;
import android.view.TextureView;
import android.view.View;
import android.view.ViewGroup;
import android.view.WindowManager;
import android.view.inputmethod.BaseInputConnection;
import android.view.inputmethod.EditorInfo;
import android.view.inputmethod.InputConnection;
import android.view.inputmethod.InputMethodManager;
import android.widget.Button;
import android.widget.FrameLayout;
import android.widget.ImageView;
import android.widget.LinearLayout;
import android.widget.TextView;

import java.nio.ByteBuffer;
import java.nio.ByteOrder;

/**
 * Longarm's remote desktop on Android (docs/mobile.md): the Go core's stream
 * (core.Core.openDesktop) decoded by MediaCodec into a TextureView. The
 * phone is a trackpad: drag moves the pointer, tap clicks, two-finger tap
 * right-clicks, two-finger drag scrolls, long press then drag drags; pinch
 * zooms. The soft keyboard types (text as Unicode, Backspace and Enter as
 * keys); the bar has Esc, the Windows key and Ctrl+Alt+Del.
 */
public class DesktopActivity extends Activity implements core.DesktopListener {
    public static final String EXTRA_WORKER = "worker";

    // PC/AT set-1 scan codes (DesktopKey)
    static final int SC_ESC = 0x01, SC_BACKSPACE = 0x0E, SC_TAB = 0x0F, SC_ENTER = 0x1C, SC_WIN = 0xE05B,
            SC_UP = 0xE048, SC_DOWN = 0xE050, SC_LEFT = 0xE04B, SC_RIGHT = 0xE04D, SC_DELETE = 0xE053;

    private final Handler ui = new Handler(Looper.getMainLooper());
    private core.Desktop desktop;
    private TextureView view;
    private Surface surface;
    private MediaCodec codec;
    private Thread drain;
    private int width, height; // the remote display
    private float zoom = 1f, panX = 0f, panY = 0f;
    private float cx, cy; // the remote pointer, display pixels
    private boolean dragging;
    private ImageView cursor;
    private int hotX, hotY;
    private TextView note;
    private KeyView keys;
    private String worker;
    private Button qualityButton;
    private String preset; // the quality asked for (internal/rdesk/quality): saved for next time

    @Override
    protected void onCreate(Bundle state) {
        super.onCreate(state);
        getWindow().addFlags(WindowManager.LayoutParams.FLAG_KEEP_SCREEN_ON | WindowManager.LayoutParams.FLAG_FULLSCREEN);
        worker = getIntent().getStringExtra(EXTRA_WORKER);
        preset = getSharedPreferences("desktop", MODE_PRIVATE).getString("quality", "auto");
        FrameLayout root = new FrameLayout(this);
        root.setBackgroundColor(Color.BLACK);
        view = new TextureView(this);
        root.addView(view, new FrameLayout.LayoutParams(ViewGroup.LayoutParams.MATCH_PARENT, ViewGroup.LayoutParams.MATCH_PARENT));
        cursor = new ImageView(this);
        root.addView(cursor, new FrameLayout.LayoutParams(ViewGroup.LayoutParams.WRAP_CONTENT, ViewGroup.LayoutParams.WRAP_CONTENT));
        keys = new KeyView(this);
        root.addView(keys, new FrameLayout.LayoutParams(1, 1));
        android.widget.HorizontalScrollView scroll = new android.widget.HorizontalScrollView(this); // a narrow screen: the bar scrolls
        scroll.addView(bar());
        root.addView(scroll, new FrameLayout.LayoutParams(ViewGroup.LayoutParams.WRAP_CONTENT, ViewGroup.LayoutParams.WRAP_CONTENT, Gravity.TOP));
        note = new TextView(this);
        note.setTextColor(Color.WHITE);
        note.setBackgroundColor(0xAA000000);
        note.setPadding(24, 12, 24, 12);
        note.setText("Connecting to " + worker + "…");
        root.addView(note, new FrameLayout.LayoutParams(ViewGroup.LayoutParams.WRAP_CONTENT, ViewGroup.LayoutParams.WRAP_CONTENT, Gravity.CENTER));
        setContentView(root);

        view.setSurfaceTextureListener(new TextureView.SurfaceTextureListener() {
            @Override public void onSurfaceTextureAvailable(SurfaceTexture st, int w, int h) {
                surface = new Surface(st);
                open();
            }
            @Override public void onSurfaceTextureSizeChanged(SurfaceTexture st, int w, int h) { layout(); }
            @Override public boolean onSurfaceTextureDestroyed(SurfaceTexture st) { return true; }
            @Override public void onSurfaceTextureUpdated(SurfaceTexture st) { }
        });
        gestures(root);
    }

    private LinearLayout bar() {
        LinearLayout bar = new LinearLayout(this);
        bar.setBackgroundColor(0x99000000);
        bar.setPadding(8, 8, 8, 8);
        bar.addView(button("✕", v -> finish()));
        bar.addView(button("⌨", v -> toggleKeyboard()));
        bar.addView(button("Esc", v -> tap(SC_ESC)));
        bar.addView(button("Win", v -> tap(SC_WIN)));
        bar.addView(button("Ctrl+Alt+Del", v -> { if (desktop != null) desktop.secureAttention(); }));
        qualityButton = button(qualityLabel(preset, ""), v -> chooseQuality());
        bar.addView(qualityButton);
        return bar;
    }

    private Button button(String label, View.OnClickListener on) {
        Button b = new Button(this);
        b.setText(label);
        b.setAllCaps(false);
        b.setSingleLine(true);
        b.setTextSize(13);
        b.setMinWidth(0);
        b.setMinimumWidth(0);
        b.setPadding(24, 0, 24, 0);
        b.setOnClickListener(on);
        return b;
    }

    private void open() {
        new Thread(() -> {
            try {
                desktop = core.Core.openDesktop(worker, 0, 30, 0, preset, this);
            } catch (Exception e) {
                String msg = e.getMessage() == null ? e.toString() : e.getMessage();
                ui.post(() -> note.setText("Can't open the desktop of " + worker + ": " + msg));
            }
        }).start();
    }

    // ---------------------------------------------------------------- the stream (a core thread)

    @Override public void started(long w, long h, String codecName, long displays) {
        width = (int) w;
        height = (int) h;
        cx = width / 2f;
        cy = height / 2f;
        stopCodec();
        try {
            MediaFormat f = MediaFormat.createVideoFormat(MediaFormat.MIMETYPE_VIDEO_AVC, width, height);
            codec = MediaCodec.createDecoderByType(MediaFormat.MIMETYPE_VIDEO_AVC);
            codec.configure(f, surface, null, 0);
            codec.start();
            MediaCodec c = codec;
            drain = new Thread(() -> drain(c));
            drain.start();
        } catch (Exception e) {
            ui.post(() -> note.setText("No H.264 decoder: " + e));
            return;
        }
        ui.post(() -> { note.setVisibility(View.GONE); layout(); });
    }

    @Override public void frame(byte[] data, boolean key, long seq) {
        MediaCodec c = codec;
        if (c == null) return;
        try {
            int i = c.dequeueInputBuffer(50_000);
            if (i < 0) { // the decoder is behind: drop this one, start again from a key frame
                desktop.keyframe();
                desktop.ack(seq);
                return;
            }
            ByteBuffer in = c.getInputBuffer(i);
            in.clear();
            in.put(data);
            c.queueInputBuffer(i, 0, data.length, System.nanoTime() / 1000, 0);
        } catch (Exception e) {
            desktop.keyframe();
        }
        desktop.ack(seq);
    }

    private void drain(MediaCodec c) {
        MediaCodec.BufferInfo info = new MediaCodec.BufferInfo();
        while (codec == c) {
            try {
                int o = c.dequeueOutputBuffer(info, 100_000);
                if (o >= 0) c.releaseOutputBuffer(o, true);
            } catch (Exception e) {
                return;
            }
        }
    }

    private void stopCodec() {
        MediaCodec c = codec;
        codec = null;
        if (c != null) {
            try { c.stop(); } catch (Exception ignored) { }
            c.release();
        }
    }

    @Override public void cursor(long x, long y, boolean visible, long shapeID, long w, long h, long hx, long hy, byte[] bgra) {
        if (!dragging) { cx = x; cy = y; }
        Bitmap bmp = null;
        if (bgra != null && w > 0 && h > 0 && bgra.length >= w * h * 4) {
            int[] px = new int[(int) (w * h)];
            ByteBuffer.wrap(bgra).order(ByteOrder.LITTLE_ENDIAN).asIntBuffer().get(px); // BGRA bytes = ARGB ints
            bmp = Bitmap.createBitmap(px, (int) w, (int) h, Bitmap.Config.ARGB_8888);
            hotX = (int) hx;
            hotY = (int) hy;
        }
        Bitmap shape = bmp;
        ui.post(() -> {
            if (shape != null) cursor.setImageBitmap(shape);
            cursor.setVisibility(visible ? View.VISIBLE : View.INVISIBLE);
            placeCursor();
        });
    }

    @Override public void quality(String asked, String level, long fps, long kbps) {
        String label = qualityLabel(asked, level);
        ui.post(() -> qualityButton.setText(label));
    }

    /** the bar's quality button: "Auto: low", "Balanced" */
    private static String qualityLabel(String asked, String level) {
        if (asked == null || asked.isEmpty()) return "Quality";
        String name = cap(asked);
        if ("auto".equals(asked) && level != null && !level.isEmpty()) name += ": " + level;
        return name;
    }

    private static String cap(String s) { return s.isEmpty() ? s : Character.toUpperCase(s.charAt(0)) + s.substring(1); }

    /** the quality presets (core.Core.qualityPresets): fewer pixels, frames and bits for a slow connection */
    private void chooseQuality() {
        String[] lines = core.Core.qualityPresets().trim().split("\n");
        String[] names = new String[lines.length], labels = new String[lines.length];
        int checked = 0;
        for (int i = 0; i < lines.length; i++) {
            String[] p = lines[i].split("\t", 2);
            names[i] = p[0];
            labels[i] = p.length > 1 ? p[1] : p[0];
            if (p[0].equals(preset)) checked = i;
        }
        new android.app.AlertDialog.Builder(this)
                .setTitle("Picture quality")
                .setSingleChoiceItems(labels, checked, (dlg, which) -> {
                    preset = names[which];
                    getSharedPreferences("desktop", MODE_PRIVATE).edit().putString("quality", preset).apply();
                    qualityButton.setText(qualityLabel(preset, ""));
                    core.Desktop d = desktop;
                    if (d != null) new Thread(() -> { try { d.setQuality(preset); } catch (Exception ignored) { } }).start();
                    dlg.dismiss();
                })
                .show();
    }

    @Override public void note(String text) {
        ui.post(() -> { note.setText(text); note.setVisibility(View.VISIBLE); ui.postDelayed(() -> note.setVisibility(View.GONE), 4000); });
    }

    @Override public void ended(String err) {
        if (err == null || err.isEmpty()) return;
        ui.post(() -> { note.setText("Disconnected: " + err); note.setVisibility(View.VISIBLE); });
    }

    // ---------------------------------------------------------------- picture and pointer

    /** fit: the remote display letterboxed in the view, then zoom and pan */
    private float fit() {
        if (width == 0 || view.getWidth() == 0) return 1f;
        return Math.min(view.getWidth() / (float) width, view.getHeight() / (float) height);
    }

    private void layout() {
        if (width == 0 || view.getWidth() == 0) return;
        float s = fit() * zoom;
        float w = width * s, h = height * s;
        // follow the pointer when zoomed; centered otherwise
        float ox = zoom > 1 ? clamp(view.getWidth() / 2f - cx * s, view.getWidth() - w, 0) : (view.getWidth() - w) / 2;
        float oy = zoom > 1 ? clamp(view.getHeight() / 2f - cy * s, view.getHeight() - h, 0) : (view.getHeight() - h) / 2;
        panX = ox;
        panY = oy;
        Matrix m = new Matrix();
        m.setScale(w / view.getWidth(), h / view.getHeight());
        m.postTranslate(ox, oy);
        view.setTransform(m);
        view.invalidate();
        placeCursor();
    }

    private void placeCursor() {
        float s = fit() * zoom;
        cursor.setScaleX(Math.max(1f, zoom * 0.75f));
        cursor.setScaleY(Math.max(1f, zoom * 0.75f));
        cursor.setPivotX(0);
        cursor.setPivotY(0);
        cursor.setTranslationX(panX + cx * s - hotX);
        cursor.setTranslationY(panY + cy * s - hotY);
    }

    private static float clamp(float v, float lo, float hi) { return Math.max(lo, Math.min(hi, v)); }

    private void moveBy(float dx, float dy) {
        float s = fit() * zoom;
        cx = clamp(cx + dx / s * 1.4f, 0, width - 1);
        cy = clamp(cy + dy / s * 1.4f, 0, height - 1);
        if (desktop != null) desktop.mouse((long) cx, (long) cy, 0, false);
        if (zoom > 1) layout(); else placeCursor();
    }

    private void click(int button) {
        if (desktop == null) return;
        desktop.mouse((long) cx, (long) cy, button, true);
        desktop.mouse((long) cx, (long) cy, button, false);
    }

    private void gestures(View root) {
        ScaleGestureDetector scale = new ScaleGestureDetector(this, new ScaleGestureDetector.SimpleOnScaleGestureListener() {
            @Override public boolean onScale(ScaleGestureDetector d) {
                zoom = clamp(zoom * d.getScaleFactor(), 1f, 5f);
                layout();
                return true;
            }
        });
        GestureDetector g = new GestureDetector(this, new GestureDetector.SimpleOnGestureListener() {
            @Override public boolean onSingleTapConfirmed(MotionEvent e) { click(1); return true; }
            @Override public boolean onDoubleTap(MotionEvent e) { click(1); click(1); return true; }
            @Override public void onLongPress(MotionEvent e) {
                if (desktop == null) return;
                dragging = true;
                desktop.mouse((long) cx, (long) cy, 1, true);
                view.performHapticFeedback(android.view.HapticFeedbackConstants.LONG_PRESS);
            }
        });
        root.setOnTouchListener(new View.OnTouchListener() {
            float lx, ly; // the last position: one finger moves, two scroll
            int fingers;
            long twoDown;
            boolean twoMoved;
            float wheel;

            @Override public boolean onTouch(View v, MotionEvent e) {
                scale.onTouchEvent(e);
                g.onTouchEvent(e);
                switch (e.getActionMasked()) {
                    case MotionEvent.ACTION_DOWN:
                        fingers = 1;
                        lx = e.getX();
                        ly = e.getY();
                        break;
                    case MotionEvent.ACTION_POINTER_DOWN:
                        fingers = e.getPointerCount();
                        if (fingers == 2) { twoDown = e.getEventTime(); twoMoved = false; ly = avgY(e); wheel = 0; }
                        break;
                    case MotionEvent.ACTION_MOVE:
                        if (scale.isInProgress() && Math.abs(scale.getScaleFactor() - 1) > 0.01) { twoMoved = true; break; }
                        if (e.getPointerCount() >= 2) {
                            float y = avgY(e);
                            wheel += (y - ly);
                            ly = y;
                            if (Math.abs(wheel) > 30) {
                                twoMoved = true;
                                if (desktop != null) desktop.wheel(0, wheel > 0 ? 120 : -120);
                                wheel = 0;
                            }
                        } else if (fingers == 1) {
                            moveBy(e.getX() - lx, e.getY() - ly);
                            lx = e.getX();
                            ly = e.getY();
                        }
                        break;
                    case MotionEvent.ACTION_POINTER_UP:
                        if (fingers == 2 && !twoMoved && e.getEventTime() - twoDown < 300) click(3); // two-finger tap
                        fingers = 0; // until all are up: no more moves
                        break;
                    case MotionEvent.ACTION_UP:
                    case MotionEvent.ACTION_CANCEL:
                        if (dragging && desktop != null) desktop.mouse((long) cx, (long) cy, 1, false);
                        dragging = false;
                        break;
                }
                return true;
            }

            float avgY(MotionEvent e) {
                float s = 0;
                for (int i = 0; i < e.getPointerCount(); i++) s += e.getY(i);
                return s / e.getPointerCount();
            }
        });
    }

    // ---------------------------------------------------------------- keyboard

    private void tap(int scan) {
        if (desktop == null) return;
        desktop.key(scan, true);
        desktop.key(scan, false);
    }

    private void toggleKeyboard() {
        InputMethodManager imm = (InputMethodManager) getSystemService(Context.INPUT_METHOD_SERVICE);
        keys.requestFocus();
        imm.toggleSoftInput(InputMethodManager.SHOW_FORCED, 0);
    }

    /** the soft keyboard's target: text goes as Unicode, Backspace / Enter / arrows as keys */
    class KeyView extends View {
        KeyView(Context c) {
            super(c);
            setFocusable(true);
            setFocusableInTouchMode(true);
        }

        @Override public boolean onCheckIsTextEditor() { return true; }

        @Override public InputConnection onCreateInputConnection(EditorInfo out) {
            out.inputType = InputType.TYPE_CLASS_TEXT | InputType.TYPE_TEXT_FLAG_NO_SUGGESTIONS;
            out.imeOptions = EditorInfo.IME_FLAG_NO_EXTRACT_UI | EditorInfo.IME_FLAG_NO_FULLSCREEN;
            return new BaseInputConnection(this, false) {
                // The IME's composing text (a word being typed, pinyin) is
                // typed on the remote as it changes: Backspace over what
                // changed, then the rest.
                String composing = "";

                void compose(String text) {
                    int same = 0;
                    while (same < text.length() && same < composing.length() && text.charAt(same) == composing.charAt(same)) same++;
                    for (int i = composing.codePointCount(same, composing.length()); i > 0; i--) tap(SC_BACKSPACE);
                    String add = text.substring(same);
                    if (!add.isEmpty() && desktop != null) {
                        if ("\n".equals(add)) tap(SC_ENTER); else desktop.text(add);
                    }
                    composing = text;
                }

                @Override public boolean setComposingText(CharSequence text, int pos) {
                    compose(text.toString());
                    return true;
                }
                @Override public boolean commitText(CharSequence text, int pos) {
                    compose(text.toString());
                    composing = "";
                    return true;
                }
                @Override public boolean finishComposingText() {
                    composing = "";
                    return true;
                }
                @Override public boolean deleteSurroundingText(int before, int after) {
                    if (!composing.isEmpty()) {
                        compose(composing.substring(0, Math.max(0, composing.length() - Math.max(1, before))));
                        return true;
                    }
                    for (int i = 0; i < Math.max(1, before); i++) tap(SC_BACKSPACE);
                    return true;
                }
                @Override public boolean sendKeyEvent(KeyEvent e) {
                    if (e.getAction() == KeyEvent.ACTION_DOWN) key(e);
                    return true;
                }
                @Override public boolean performEditorAction(int action) {
                    tap(SC_ENTER);
                    return true;
                }
            };
        }

        @Override public boolean onKeyDown(int code, KeyEvent e) {
            return key(e) || super.onKeyDown(code, e);
        }

        /** a key event (a hardware keyboard, some IMEs): keys that type no text, then text */
        boolean key(KeyEvent e) {
            int scan;
            switch (e.getKeyCode()) {
                case KeyEvent.KEYCODE_DEL: scan = SC_BACKSPACE; break;
                case KeyEvent.KEYCODE_FORWARD_DEL: scan = SC_DELETE; break;
                case KeyEvent.KEYCODE_ENTER: scan = SC_ENTER; break;
                case KeyEvent.KEYCODE_TAB: scan = SC_TAB; break;
                case KeyEvent.KEYCODE_ESCAPE: scan = SC_ESC; break;
                case KeyEvent.KEYCODE_DPAD_UP: scan = SC_UP; break;
                case KeyEvent.KEYCODE_DPAD_DOWN: scan = SC_DOWN; break;
                case KeyEvent.KEYCODE_DPAD_LEFT: scan = SC_LEFT; break;
                case KeyEvent.KEYCODE_DPAD_RIGHT: scan = SC_RIGHT; break;
                default:
                    int u = e.getUnicodeChar(e.getMetaState());
                    if (u <= 0 || desktop == null) return false;
                    desktop.text(new String(Character.toChars(u)));
                    return true;
            }
            tap(scan);
            return true;
        }
    }

    @Override protected void onDestroy() {
        if (desktop != null) desktop.close();
        stopCodec();
        super.onDestroy();
    }
}
