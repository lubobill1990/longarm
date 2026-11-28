import AVFoundation
import Longarm
import UIKit

/// Longarm's remote desktop on iOS (docs/mobile.md): the Go core's stream
/// (CoreOpenDesktop) - H.264 access units, Annex B - rewritten as AVCC
/// sample buffers for an AVSampleBufferDisplayLayer (hardware decoding,
/// shown as it comes). The phone is a trackpad: drag moves the pointer,
/// tap clicks, two-finger tap right-clicks, two-finger drag scrolls, long
/// press then drag drags; pinch zooms. The keyboard button types (text as
/// Unicode; Backspace, Enter, arrows as keys); the bar has Esc, the Windows
/// key and Ctrl+Alt+Del.
final class DesktopViewController: UIViewController, CoreDesktopListenerProtocol {
    // PC/AT set-1 scan codes (DesktopKey)
    static let scEsc = 0x01, scBackspace = 0x0E, scTab = 0x0F, scEnter = 0x1C, scWin = 0xE05B
    static let scUp = 0xE048, scDown = 0xE050, scLeft = 0xE04B, scRight = 0xE04D, scDelete = 0xE053

    private let worker: String
    private var desktop: CoreDesktop?
    private let display = AVSampleBufferDisplayLayer()
    private var format: CMVideoFormatDescription?
    private var sps: Data?, pps: Data?
    private var size = CGSize.zero // the remote display
    private var zoom: CGFloat = 1
    private var pointer = CGPoint.zero // display pixels
    private var dragging = false
    private let cursor = UIImageView()
    private var hot = CGPoint.zero
    private let note = UILabel()
    private let keys = KeyView()
    private let qualityButton = UIButton(type: .system)
    // the quality asked for (internal/rdesk/quality): saved for next time
    private var preset = UserDefaults.standard.string(forKey: "desktopQuality") ?? "auto"

    init(worker: String) {
        self.worker = worker
        super.init(nibName: nil, bundle: nil)
    }

    required init?(coder: NSCoder) { fatalError("init(coder:) is not used") }

    override var prefersStatusBarHidden: Bool { true }
    override var prefersHomeIndicatorAutoHidden: Bool { true }

    override func viewDidLoad() {
        super.viewDidLoad()
        view.backgroundColor = .black
        display.videoGravity = .resize
        view.layer.addSublayer(display)
        view.addSubview(cursor)
        keys.desktop = self
        view.addSubview(keys)
        note.textColor = .white
        note.backgroundColor = UIColor.black.withAlphaComponent(0.65)
        note.textAlignment = .center
        note.numberOfLines = 0
        note.text = "Connecting to \(worker)…"
        view.addSubview(note)
        view.addSubview(bar())
        gestures()
        DispatchQueue.global(qos: .userInitiated).async { [weak self] in
            guard let self else { return }
            var err: NSError?
            let d = CoreOpenDesktop(self.worker, 0, 30, 0, self.preset, self, &err)
            DispatchQueue.main.async {
                if let err { self.show("Can't open the desktop of \(self.worker): \(err.localizedDescription)") }
                self.desktop = d
            }
        }
    }

    override func viewDidLayoutSubviews() {
        super.viewDidLayoutSubviews()
        note.frame = CGRect(x: 20, y: view.bounds.midY - 40, width: view.bounds.width - 40, height: 80)
        layout()
    }

    override func viewWillDisappear(_ animated: Bool) {
        super.viewWillDisappear(animated)
        desktop?.close()
    }

    private func bar() -> UIStackView {
        let items: [(String, () -> Void)] = [
            ("✕", { [weak self] in self?.dismiss(animated: true) }),
            ("⌨", { [weak self] in self?.toggleKeyboard() }),
            ("Esc", { [weak self] in self?.tap(Self.scEsc) }),
            ("Win", { [weak self] in self?.tap(Self.scWin) }),
            ("Ctrl+Alt+Del", { [weak self] in self?.desktop?.secureAttention() }),
        ]
        let stack = UIStackView()
        stack.axis = .horizontal
        stack.spacing = 8
        stack.backgroundColor = UIColor.black.withAlphaComponent(0.55)
        stack.layoutMargins = UIEdgeInsets(top: 6, left: 10, bottom: 6, right: 10)
        stack.isLayoutMarginsRelativeArrangement = true
        for (label, action) in items {
            let b = UIButton(type: .system)
            b.setTitle(label, for: .normal)
            b.setTitleColor(.white, for: .normal)
            b.addAction(UIAction { _ in action() }, for: .touchUpInside)
            stack.addArrangedSubview(b)
        }
        qualityButton.setTitle(Self.qualityLabel(preset, ""), for: .normal)
        qualityButton.setTitleColor(.white, for: .normal)
        qualityButton.addAction(UIAction { [weak self] _ in self?.chooseQuality() }, for: .touchUpInside)
        stack.addArrangedSubview(qualityButton)
        stack.translatesAutoresizingMaskIntoConstraints = false
        DispatchQueue.main.async { [weak self] in
            guard let self else { return }
            NSLayoutConstraint.activate([
                stack.topAnchor.constraint(equalTo: self.view.safeAreaLayoutGuide.topAnchor),
                stack.leadingAnchor.constraint(equalTo: self.view.safeAreaLayoutGuide.leadingAnchor),
            ])
        }
        return stack
    }

    private func show(_ text: String) {
        note.text = text
        note.isHidden = false
    }

    // ------------------------------------------------------------ the stream (a core thread)

    func started(_ width: Int, height: Int, codec: String?, displays: Int) {
        DispatchQueue.main.async {
            self.size = CGSize(width: width, height: height)
            self.pointer = CGPoint(x: width / 2, y: height / 2)
            self.format = nil
            self.display.flush()
            self.note.isHidden = true
            self.layout()
        }
    }

    func frame(_ data: Data?, key: Bool, seq: Int64) {
        guard let data else { return }
        DispatchQueue.main.async {
            self.enqueue(data)
            self.desktop?.ack(seq)
        }
    }

    func cursor(_ x: Int, y: Int, visible: Bool, shapeID: Int64, w: Int, h: Int, hotX: Int, hotY: Int, bgra: Data?) {
        let image = bgra.flatMap { Self.image(bgra: $0, w: w, h: h) }
        DispatchQueue.main.async {
            if !self.dragging { self.pointer = CGPoint(x: x, y: y) }
            if let image {
                self.cursor.image = image
                self.cursor.frame.size = image.size
                self.hot = CGPoint(x: hotX, y: hotY)
            }
            self.cursor.isHidden = !visible
            self.placeCursor()
        }
    }

    func quality(_ preset: String?, level: String?, fps: Int, kbps: Int) {
        let label = Self.qualityLabel(preset ?? "", level ?? "")
        DispatchQueue.main.async { self.qualityButton.setTitle(label, for: .normal) }
    }

    /// the bar's quality button: "Auto: low", "Balanced"
    static func qualityLabel(_ preset: String, _ level: String) -> String {
        if preset.isEmpty { return "Quality" }
        var name = preset.prefix(1).uppercased() + preset.dropFirst()
        if preset == "auto" && !level.isEmpty { name += ": " + level }
        return name
    }

    /// the quality presets (CoreQualityPresets): fewer pixels, frames and bits for a slow connection
    private func chooseQuality() {
        let sheet = UIAlertController(title: "Picture quality", message: nil, preferredStyle: .actionSheet)
        for line in CoreQualityPresets().split(separator: "\n") {
            let parts = line.split(separator: "\t", maxSplits: 1).map(String.init)
            guard let name = parts.first else { continue }
            let label = (parts.count > 1 ? parts[1] : name) + (name == preset ? " ✓" : "")
            sheet.addAction(UIAlertAction(title: label, style: .default) { [weak self] _ in
                guard let self else { return }
                self.preset = name
                UserDefaults.standard.set(name, forKey: "desktopQuality")
                self.qualityButton.setTitle(Self.qualityLabel(name, ""), for: .normal)
                let d = self.desktop
                DispatchQueue.global(qos: .userInitiated).async { try? d?.setQuality(name) }
            })
        }
        sheet.addAction(UIAlertAction(title: "Cancel", style: .cancel))
        sheet.popoverPresentationController?.sourceView = qualityButton // iPad
        present(sheet, animated: true)
    }

    func note(_ text: String?) {
        DispatchQueue.main.async {
            self.show(text ?? "")
            DispatchQueue.main.asyncAfter(deadline: .now() + 4) { self.note.isHidden = true }
        }
    }

    func ended(_ err: String?) {
        guard let err, !err.isEmpty else { return }
        DispatchQueue.main.async { self.show("Disconnected: \(err)") }
    }

    // ------------------------------------------------------------ H.264: Annex B to AVCC

    /// The NAL units of an Annex B access unit (start codes 00 00 01 / 00 00 00 01).
    static func nals(_ d: Data) -> [Data] {
        var out: [Data] = []
        let b = [UInt8](d)
        var i = 0, start = -1
        while i + 2 < b.count {
            if b[i] == 0, b[i + 1] == 0, b[i + 2] == 1 {
                if start >= 0 {
                    var end = i
                    if end > start, b[end - 1] == 0 { end -= 1 } // 4-byte start code
                    out.append(Data(b[start..<end]))
                }
                i += 3
                start = i
            } else {
                i += 1
            }
        }
        if start >= 0, start < b.count { out.append(Data(b[start...])) }
        return out
    }

    private func enqueue(_ au: Data) {
        var avcc = Data()
        for nal in Self.nals(au) where !nal.isEmpty {
            switch nal[nal.startIndex] & 0x1F {
            case 7: if sps != nal { sps = nal; format = nil }
            case 8: if pps != nal { pps = nal; format = nil }
            case 9: break // access unit delimiter
            default:
                var n = UInt32(nal.count).bigEndian
                avcc.append(Data(bytes: &n, count: 4))
                avcc.append(nal)
            }
        }
        if format == nil, let sps, let pps {
            format = Self.formatDescription(sps: sps, pps: pps)
        }
        guard let format, !avcc.isEmpty else { return }
        guard let sample = Self.sample(avcc, format: format) else { return }
        if display.status == .failed {
            display.flush()
            desktop?.keyframe()
        }
        display.enqueue(sample)
    }

    static func formatDescription(sps: Data, pps: Data) -> CMVideoFormatDescription? {
        var desc: CMVideoFormatDescription?
        sps.withUnsafeBytes { s in
            pps.withUnsafeBytes { p in
                let sets = [s.bindMemory(to: UInt8.self).baseAddress!, p.bindMemory(to: UInt8.self).baseAddress!]
                let sizes = [sps.count, pps.count]
                CMVideoFormatDescriptionCreateFromH264ParameterSets(
                    allocator: kCFAllocatorDefault, parameterSetCount: 2, parameterSetPointers: sets,
                    parameterSetSizes: sizes, nalUnitHeaderLength: 4, formatDescriptionOut: &desc)
            }
        }
        return desc
    }

    static func sample(_ avcc: Data, format: CMVideoFormatDescription) -> CMSampleBuffer? {
        var block: CMBlockBuffer?
        guard CMBlockBufferCreateWithMemoryBlock(
            allocator: kCFAllocatorDefault, memoryBlock: nil, blockLength: avcc.count, blockAllocator: kCFAllocatorDefault,
            customBlockSource: nil, offsetToData: 0, dataLength: avcc.count, flags: 0, blockBufferOut: &block) == noErr,
            let block else { return nil }
        avcc.withUnsafeBytes { raw in
            _ = CMBlockBufferReplaceDataBytes(with: raw.baseAddress!, blockBuffer: block, offsetIntoDestination: 0, dataLength: avcc.count)
        }
        var sample: CMSampleBuffer?
        var sizes = [avcc.count]
        guard CMSampleBufferCreateReady(
            allocator: kCFAllocatorDefault, dataBuffer: block, formatDescription: format, sampleCount: 1,
            sampleTimingEntryCount: 0, sampleTimingArray: nil, sampleSizeEntryCount: 1, sampleSizeArray: &sizes,
            sampleBufferOut: &sample) == noErr, let sample else { return nil }
        if let att = CMSampleBufferGetSampleAttachmentsArray(sample, createIfNecessary: true), CFArrayGetCount(att) > 0 {
            let dict = unsafeBitCast(CFArrayGetValueAtIndex(att, 0), to: CFMutableDictionary.self)
            CFDictionarySetValue(dict, Unmanaged.passUnretained(kCMSampleAttachmentKey_DisplayImmediately).toOpaque(),
                                 Unmanaged.passUnretained(kCFBooleanTrue).toOpaque())
        }
        return sample
    }

    static func image(bgra: Data, w: Int, h: Int) -> UIImage? {
        guard w > 0, h > 0, bgra.count >= w * h * 4 else { return nil }
        let cs = CGColorSpaceCreateDeviceRGB()
        let info = CGBitmapInfo(rawValue: CGImageAlphaInfo.premultipliedFirst.rawValue | CGBitmapInfo.byteOrder32Little.rawValue)
        guard let provider = CGDataProvider(data: bgra as CFData),
              let cg = CGImage(width: w, height: h, bitsPerComponent: 8, bitsPerPixel: 32, bytesPerRow: w * 4, space: cs,
                               bitmapInfo: info, provider: provider, decode: nil, shouldInterpolate: false, intent: .defaultIntent)
        else { return nil }
        return UIImage(cgImage: cg, scale: UIScreen.main.scale, orientation: .up)
    }

    // ------------------------------------------------------------ picture and pointer

    /// the remote display letterboxed in the view
    private var fit: CGFloat {
        guard size.width > 0, view.bounds.width > 0 else { return 1 }
        return min(view.bounds.width / size.width, view.bounds.height / size.height)
    }

    private var origin = CGPoint.zero

    private func layout() {
        guard size.width > 0 else { return }
        let s = fit * zoom
        let w = size.width * s, h = size.height * s
        let b = view.bounds
        // follow the pointer when zoomed; centered otherwise
        let ox = zoom > 1 ? clamp(b.width / 2 - pointer.x * s, b.width - w, 0) : (b.width - w) / 2
        let oy = zoom > 1 ? clamp(b.height / 2 - pointer.y * s, b.height - h, 0) : (b.height - h) / 2
        origin = CGPoint(x: ox, y: oy)
        CATransaction.begin()
        CATransaction.setDisableActions(true)
        display.frame = CGRect(x: ox, y: oy, width: w, height: h)
        CATransaction.commit()
        placeCursor()
    }

    private func placeCursor() {
        let s = fit * zoom
        let k = max(1, zoom * 0.75)
        cursor.transform = CGAffineTransform(scaleX: k, y: k)
        cursor.frame.origin = CGPoint(x: origin.x + pointer.x * s - hot.x * k, y: origin.y + pointer.y * s - hot.y * k)
    }

    private func clamp(_ v: CGFloat, _ lo: CGFloat, _ hi: CGFloat) -> CGFloat { max(lo, min(hi, v)) }

    private func moveBy(_ d: CGPoint) {
        let s = fit * zoom
        pointer.x = clamp(pointer.x + d.x / s * 1.4, 0, max(size.width - 1, 0))
        pointer.y = clamp(pointer.y + d.y / s * 1.4, 0, max(size.height - 1, 0))
        desktop?.mouse(Int(pointer.x), y: Int(pointer.y), button: 0, down: false)
        if zoom > 1 { layout() } else { placeCursor() }
    }

    private func click(_ button: Int) {
        desktop?.mouse(Int(pointer.x), y: Int(pointer.y), button: button, down: true)
        desktop?.mouse(Int(pointer.x), y: Int(pointer.y), button: button, down: false)
    }

    private func gestures() {
        let move = UIPanGestureRecognizer(target: self, action: #selector(onMove(_:)))
        move.maximumNumberOfTouches = 1
        let scroll = UIPanGestureRecognizer(target: self, action: #selector(onScroll(_:)))
        scroll.minimumNumberOfTouches = 2
        scroll.maximumNumberOfTouches = 2
        let tap = UITapGestureRecognizer(target: self, action: #selector(onTap))
        let tap2 = UITapGestureRecognizer(target: self, action: #selector(onTwoFingerTap))
        tap2.numberOfTouchesRequired = 2
        let pinch = UIPinchGestureRecognizer(target: self, action: #selector(onPinch(_:)))
        let press = UILongPressGestureRecognizer(target: self, action: #selector(onPress(_:)))
        press.minimumPressDuration = 0.45
        for g in [move, scroll, tap, tap2, pinch, press] as [UIGestureRecognizer] { view.addGestureRecognizer(g) }
    }

    @objc private func onMove(_ g: UIPanGestureRecognizer) {
        moveBy(g.translation(in: view))
        g.setTranslation(.zero, in: view)
    }

    private var wheel: CGFloat = 0

    @objc private func onScroll(_ g: UIPanGestureRecognizer) {
        wheel += g.translation(in: view).y
        g.setTranslation(.zero, in: view)
        while abs(wheel) > 24 {
            desktop?.wheel(0, dy: wheel > 0 ? 120 : -120)
            wheel -= wheel > 0 ? 24 : -24
        }
    }

    @objc private func onTap() { click(1) }
    @objc private func onTwoFingerTap() { click(3) }

    @objc private func onPinch(_ g: UIPinchGestureRecognizer) {
        zoom = clamp(zoom * g.scale, 1, 5)
        g.scale = 1
        layout()
    }

    private var pressAt = CGPoint.zero

    @objc private func onPress(_ g: UILongPressGestureRecognizer) {
        let p = g.location(in: view)
        switch g.state {
        case .began:
            dragging = true
            pressAt = p
            desktop?.mouse(Int(pointer.x), y: Int(pointer.y), button: 1, down: true)
            UIImpactFeedbackGenerator(style: .medium).impactOccurred()
        case .changed:
            moveBy(CGPoint(x: p.x - pressAt.x, y: p.y - pressAt.y))
            pressAt = p
        default:
            dragging = false
            desktop?.mouse(Int(pointer.x), y: Int(pointer.y), button: 1, down: false)
        }
    }

    // ------------------------------------------------------------ keyboard

    func tap(_ scan: Int) {
        desktop?.key(scan, down: true)
        desktop?.key(scan, down: false)
    }

    func type(_ text: String) {
        if text == "\n" { tap(Self.scEnter) } else { desktop?.text(text) }
    }

    private func toggleKeyboard() {
        if keys.isFirstResponder { keys.resignFirstResponder() } else { keys.becomeFirstResponder() }
    }

    override func pressesBegan(_ presses: Set<UIPress>, with event: UIPressesEvent?) {
        // a hardware keyboard's keys that type no text
        var handled = false
        for p in presses {
            guard let k = p.key else { continue }
            let scan: Int? = switch k.keyCode {
            case .keyboardUpArrow: Self.scUp
            case .keyboardDownArrow: Self.scDown
            case .keyboardLeftArrow: Self.scLeft
            case .keyboardRightArrow: Self.scRight
            case .keyboardEscape: Self.scEsc
            case .keyboardTab: Self.scTab
            case .keyboardDeleteForward: Self.scDelete
            default: nil
            }
            if let scan { tap(scan); handled = true }
        }
        if !handled { super.pressesBegan(presses, with: event) }
    }
}

/// The keyboard's target: text as Unicode, Backspace and Return as keys.
final class KeyView: UIView, UIKeyInput {
    weak var desktop: DesktopViewController?
    var autocorrectionType: UITextAutocorrectionType = .no
    var autocapitalizationType: UITextAutocapitalizationType = .none
    var spellCheckingType: UITextSpellCheckingType = .no
    var smartQuotesType: UITextSmartQuotesType = .no
    var smartDashesType: UITextSmartDashesType = .no
    var keyboardType: UIKeyboardType = .asciiCapable

    override var canBecomeFirstResponder: Bool { true }
    var hasText: Bool { true } // Backspace always reaches deleteBackward
    func insertText(_ text: String) { desktop?.type(text) }
    func deleteBackward() { desktop?.tap(DesktopViewController.scBackspace) }
}
