//go:build windows

package vdisplay

import (
	"bytes"
	"crypto/x509"
	"embed"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
	"unsafe"

	"golang.org/x/sys/windows"
	"golang.org/x/sys/windows/registry"
)

//go:embed driver/MttVDD.inf driver/MttVDD.dll driver/mttvdd.cat driver/publisher.cer
var driverFS embed.FS

const (
	hardwareID = `Root\MttVDD`
	// settingsDir: where the driver reads vdd_settings.xml (fixed in it)
	settingsDir = `C:\VirtualDisplayDriver`
	// adapterName: the display adapter's description (EnumDisplayDevices)
	adapterName = "Virtual Display Driver"
)

// GUID_DEVCLASS_DISPLAY
var displayClass = windows.GUID{Data1: 0x4d36e968, Data2: 0xe325, Data3: 0x11ce, Data4: [8]byte{0xbf, 0xc1, 0x08, 0x00, 0x2b, 0xe1, 0x03, 0x18}}

var (
	modNewdev                     = windows.NewLazySystemDLL("newdev.dll")
	procUpdateDriverForPnPDevices = modNewdev.NewProc("UpdateDriverForPlugAndPlayDevicesW")
	modUser32                     = windows.NewLazySystemDLL("user32.dll")
	procEnumDisplayDevicesW       = modUser32.NewProc("EnumDisplayDevicesW")
	procChangeDisplaySettingsExW  = modUser32.NewProc("ChangeDisplaySettingsExW")
)

func supported() bool {
	// IddCx: Windows 10 1607+; the driver wants 2004 (build 19041)
	maj, _, build := windows.RtlGetNtVersionNumbers()
	return maj >= 10 && build >= 19041
}

func enable(m Mode) (string, error) {
	m = clamp(m)
	if !supported() {
		return "", errors.New("a virtual display needs Windows 10 2004 or newer")
	}
	if err := os.MkdirAll(settingsDir, 0o755); err != nil {
		return "", err
	}
	if err := os.WriteFile(filepath.Join(settingsDir, "vdd_settings.xml"), []byte(settingsXML(m)), 0o644); err != nil {
		return "", err
	}
	set, dev, err := findDevice()
	if err != nil {
		return "", err
	}
	if dev == nil {
		set.Close()
		if set, dev, err = createDevice(); err != nil {
			return "", err
		}
	}
	defer set.Close()
	if driverKey(set, dev) == "" {
		// no driver yet: install it (this starts the device, with the settings)
		if err := installDriver(); err != nil {
			return "", err
		}
	} else {
		// on, and started again: the driver reads the settings when it starts
		if err := changeState(set, dev, windows.DICS_ENABLE); err != nil {
			return "", err
		}
		if err := changeState(set, dev, windows.DICS_PROPCHANGE); err != nil {
			return "", err
		}
	}
	// Windows extends the desktop onto a new monitor: wait for it, then
	// make sure it runs at m.
	deadline := time.Now().Add(15 * time.Second)
	for {
		if name := attachedDisplay(); name != "" {
			if err := setMode(name, m); err != nil {
				return name, err
			}
			return name, nil
		}
		if time.Now().After(deadline) {
			return "", errors.New("the virtual display did not come up")
		}
		time.Sleep(250 * time.Millisecond)
	}
}

func disable() error {
	set, dev, err := findDevice()
	if err != nil {
		return err
	}
	defer set.Close()
	if dev == nil {
		return nil
	}
	return changeState(set, dev, windows.DICS_DISABLE)
}

func remove() error {
	set, dev, err := findDevice()
	if err != nil {
		return err
	}
	defer set.Close()
	if dev == nil {
		return nil
	}
	oem := oemInf(driverKey(set, dev))
	if err := set.CallClassInstaller(windows.DIF_REMOVE, dev); err != nil {
		return fmt.Errorf("remove the virtual display: %w", err)
	}
	if oem != "" {
		// the driver package out of the driver store
		exec.Command(filepath.Join(os.Getenv("SystemRoot"), "System32", "pnputil.exe"), "/delete-driver", oem, "/uninstall", "/force").Run()
	}
	os.RemoveAll(settingsDir)
	return nil
}

// findDevice: the virtual display's device (nil: none).
func findDevice() (windows.DevInfo, *windows.DevInfoData, error) {
	set, err := windows.SetupDiGetClassDevsEx(&displayClass, "", 0, 0, 0, "")
	if err != nil {
		return 0, nil, fmt.Errorf("SetupDiGetClassDevs: %w", err)
	}
	for i := 0; ; i++ {
		d, err := set.EnumDeviceInfo(i)
		if err != nil {
			break
		}
		if ids, _ := set.DeviceRegistryProperty(d, windows.SPDRP_HARDWAREID); hasID(ids, hardwareID) {
			return set, d, nil
		}
	}
	return set, nil, nil
}

func hasID(v any, id string) bool {
	ids, _ := v.([]string)
	for _, x := range ids {
		if strings.EqualFold(x, id) {
			return true
		}
	}
	return false
}

// createDevice makes the root-enumerated device the driver binds to (what
// `devcon install` does before installing).
func createDevice() (windows.DevInfo, *windows.DevInfoData, error) {
	set, err := windows.SetupDiCreateDeviceInfoListEx(&displayClass, 0, "")
	if err != nil {
		return 0, nil, err
	}
	d, err := set.CreateDeviceInfo("Display", &displayClass, "", 0, windows.DICD_GENERATE_ID)
	if err != nil {
		set.Close()
		return 0, nil, fmt.Errorf("SetupDiCreateDeviceInfo: %w", err)
	}
	ids, _ := windows.UTF16FromString(hardwareID)
	ids = append(ids, 0) // REG_MULTI_SZ
	if err := set.SetDeviceRegistryProperty(d, windows.SPDRP_HARDWAREID, unsafe.Slice((*byte)(unsafe.Pointer(&ids[0])), len(ids)*2)); err != nil {
		set.Close()
		return 0, nil, fmt.Errorf("hardware id: %w", err)
	}
	if err := set.CallClassInstaller(windows.DIF_REGISTERDEVICE, d); err != nil {
		set.Close()
		return 0, nil, fmt.Errorf("register the device: %w", err)
	}
	return set, d, nil
}

// installDriver puts the embedded driver package on the device. Its
// signature is a publisher's, not Microsoft's: Windows would ask whether
// to trust the publisher, so it is trusted for the install only.
func installDriver() error {
	dir := filepath.Join(os.Getenv("ProgramData"), "Longarm", "vdisplay")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	for _, f := range []string{"MttVDD.inf", "MttVDD.dll", "mttvdd.cat"} {
		b, _ := driverFS.ReadFile("driver/" + f)
		if old, err := os.ReadFile(filepath.Join(dir, f)); err == nil && bytes.Equal(old, b) {
			continue
		}
		if err := os.WriteFile(filepath.Join(dir, f), b, 0o644); err != nil {
			return err
		}
	}
	done, err := trustPublisher()
	if err != nil {
		return err
	}
	defer done()
	hwid, _ := windows.UTF16PtrFromString(hardwareID)
	inf, _ := windows.UTF16PtrFromString(filepath.Join(dir, "MttVDD.inf"))
	var reboot int32
	const installFlagForce = 1
	if r, _, err := procUpdateDriverForPnPDevices.Call(0, uintptr(unsafe.Pointer(hwid)), uintptr(unsafe.Pointer(inf)), installFlagForce, uintptr(unsafe.Pointer(&reboot))); r == 0 {
		return fmt.Errorf("install the virtual display driver: %w", err)
	}
	return nil
}

// trustPublisher adds the driver's publisher certificate to the machine's
// TrustedPublisher store (unless there already) and returns what removes
// it again.
func trustPublisher() (func(), error) {
	der, _ := driverFS.ReadFile("driver/publisher.cer")
	if _, err := x509.ParseCertificate(der); err != nil {
		return nil, err
	}
	name, _ := windows.UTF16PtrFromString("TrustedPublisher")
	const certStoreProvSystemW, certSystemStoreLocalMachine = 10, 0x20000
	store, err := windows.CertOpenStore(certStoreProvSystemW, 0, 0, certSystemStoreLocalMachine, uintptr(unsafe.Pointer(name)))
	if err != nil {
		return nil, fmt.Errorf("TrustedPublisher store: %w", err)
	}
	ctx, err := windows.CertCreateCertificateContext(windows.X509_ASN_ENCODING|windows.PKCS_7_ASN_ENCODING, &der[0], uint32(len(der)))
	if err != nil {
		windows.CertCloseStore(store, 0)
		return nil, err
	}
	defer windows.CertFreeCertificateContext(ctx)
	var added *windows.CertContext
	// CERT_STORE_ADD_NEW: fails (and nothing to undo) when it is there already
	const certStoreAddNew = 1
	if err := windows.CertAddCertificateContextToStore(store, ctx, certStoreAddNew, &added); err != nil {
		windows.CertCloseStore(store, 0)
		return func() {}, nil
	}
	return func() {
		windows.CertDeleteCertificateFromStore(added) // frees added
		windows.CertCloseStore(store, 0)
	}, nil
}

func changeState(set windows.DevInfo, d *windows.DevInfoData, state windows.DICS_STATE) error {
	p := windows.PropChangeParams{ClassInstallHeader: *windows.MakeClassInstallHeader(windows.DIF_PROPERTYCHANGE), StateChange: state, Scope: windows.DICS_FLAG_GLOBAL}
	if err := set.SetClassInstallParams(d, &p.ClassInstallHeader, uint32(unsafe.Sizeof(p))); err != nil {
		return err
	}
	if err := set.CallClassInstaller(windows.DIF_PROPERTYCHANGE, d); err != nil {
		return fmt.Errorf("virtual display state %d: %w", state, err)
	}
	return nil
}

// driverKey: the device's driver key ("{class guid}\0003"; "" = no driver).
func driverKey(set windows.DevInfo, d *windows.DevInfoData) string {
	v, _ := set.DeviceRegistryProperty(d, windows.SPDRP_DRIVER)
	s, _ := v.(string)
	return s
}

// oemInf: the driver store's name of the package bound by driverKey (oemNN.inf).
func oemInf(key string) string {
	if key == "" {
		return ""
	}
	k, err := registry.OpenKey(registry.LOCAL_MACHINE, `SYSTEM\CurrentControlSet\Control\Class\`+key, registry.QUERY_VALUE)
	if err != nil {
		return ""
	}
	defer k.Close()
	s, _, _ := k.GetStringValue("InfPath")
	return s
}

// displayDevice is DISPLAY_DEVICEW.
type displayDevice struct {
	Cb           uint32
	DeviceName   [32]uint16
	DeviceString [128]uint16
	StateFlags   uint32
	DeviceID     [128]uint16
	DeviceKey    [128]uint16
}

// attachedDisplay: the virtual display's name once it is part of the
// desktop ("" = not yet).
func attachedDisplay() string {
	const attachedToDesktop = 0x1
	for i := uint32(0); ; i++ {
		var dd displayDevice
		dd.Cb = uint32(unsafe.Sizeof(dd))
		if r, _, _ := procEnumDisplayDevicesW.Call(0, uintptr(i), uintptr(unsafe.Pointer(&dd)), 0); r == 0 {
			return ""
		}
		if windows.UTF16ToString(dd.DeviceString[:]) == adapterName && dd.StateFlags&attachedToDesktop != 0 {
			return windows.UTF16ToString(dd.DeviceName[:])
		}
	}
}

// devMode is DEVMODEW (the display part).
type devMode struct {
	DeviceName       [32]uint16
	SpecVersion      uint16
	DriverVersion    uint16
	Size             uint16
	DriverExtra      uint16
	Fields           uint32
	PositionX        int32
	PositionY        int32
	Orientation      uint32
	FixedOutput      uint32
	Color            int16
	Duplex           int16
	YResolution      int16
	TTOption         int16
	Collate          int16
	FormName         [32]uint16
	LogPixels        uint16
	BitsPerPel       uint32
	PelsWidth        uint32
	PelsHeight       uint32
	DisplayFlags     uint32
	DisplayFrequency uint32
	_                [8]uint32 // ICM, panning
}

func setMode(name string, m Mode) error {
	const dmPelsWidth, dmPelsHeight, cdsUpdateRegistry = 0x80000, 0x100000, 0x1
	dm := devMode{Fields: dmPelsWidth | dmPelsHeight, PelsWidth: uint32(m.W), PelsHeight: uint32(m.H)}
	dm.Size = uint16(unsafe.Sizeof(dm))
	n, _ := windows.UTF16PtrFromString(name)
	if r, _, _ := procChangeDisplaySettingsExW.Call(uintptr(unsafe.Pointer(n)), uintptr(unsafe.Pointer(&dm)), 0, cdsUpdateRegistry, 0); int32(r) != 0 {
		return fmt.Errorf("virtual display %dx%d: ChangeDisplaySettingsEx %d", m.W, m.H, int32(r))
	}
	return nil
}
