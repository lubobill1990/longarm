//go:build windows

package audiodrv

import (
	"errors"
	"fmt"
	"path/filepath"
	"slices"
	"strings"
	"unsafe"

	"golang.org/x/sys/windows"
	"golang.org/x/sys/windows/registry"
)

// What devcon install / remove do: a root-enumerated device created with
// SetupAPI, its driver installed with UpdateDriverForPlugAndPlayDevices.

var (
	modSetupapi                       = windows.NewLazySystemDLL("setupapi.dll")
	modNewdev                         = windows.NewLazySystemDLL("newdev.dll")
	procSetupDiGetINFClassW           = modSetupapi.NewProc("SetupDiGetINFClassW")
	procSetupUninstallOEMInfW         = modSetupapi.NewProc("SetupUninstallOEMInfW")
	procUpdateDriverForPlugAndPlayDev = modNewdev.NewProc("UpdateDriverForPlugAndPlayDevicesW")
)

const (
	installFlagForce = 0x1 // INSTALLFLAG_FORCE
	suoiForceDelete  = 0x1 // SUOI_FORCEDELETE
)

func install(inf string) (bool, error) {
	inf, err := filepath.Abs(inf)
	if err != nil {
		return false, err
	}
	infW, _ := windows.UTF16PtrFromString(inf)
	ds, err := find()
	if err != nil {
		return false, err
	}
	closeAll(ds)
	if len(ds) == 0 {
		// The device: of the INF's class, with our hardware id.
		var cls windows.GUID
		name := make([]uint16, 256)
		if r, _, err := procSetupDiGetINFClassW.Call(uintptr(unsafe.Pointer(infW)), uintptr(unsafe.Pointer(&cls)),
			uintptr(unsafe.Pointer(&name[0])), uintptr(len(name)), 0); r == 0 {
			return false, fmt.Errorf("%s: %w", inf, err)
		}
		set, err := windows.SetupDiCreateDeviceInfoListEx(&cls, 0, "")
		if err != nil {
			return false, err
		}
		defer set.Close()
		data, err := set.CreateDeviceInfo(windows.UTF16ToString(name), &cls, "", 0, windows.DICD_GENERATE_ID)
		if err != nil {
			return false, fmt.Errorf("create the device: %w", err)
		}
		if err := set.SetDeviceRegistryProperty(data, windows.SPDRP_HARDWAREID, multiSZ(HardwareID)); err != nil {
			return false, fmt.Errorf("the device's hardware id: %w", err)
		}
		if err := set.CallClassInstaller(windows.DIF_REGISTERDEVICE, data); err != nil {
			return false, fmt.Errorf("register the device: %w", err)
		}
	}
	// The driver for every device with our id (the new one, or an update).
	hw, _ := windows.UTF16PtrFromString(HardwareID)
	var reboot int32
	if r, _, err := procUpdateDriverForPlugAndPlayDev.Call(0, uintptr(unsafe.Pointer(hw)), uintptr(unsafe.Pointer(infW)),
		installFlagForce, uintptr(unsafe.Pointer(&reboot))); r == 0 {
		return false, fmt.Errorf("install the driver: %w", err)
	}
	return reboot != 0, nil
}

func uninstall() error {
	ds, err := find()
	if err != nil {
		return err
	}
	var errs []error
	var infs []string
	for _, d := range ds {
		if oem := d.oemInf(); oem != "" && !slices.Contains(infs, oem) {
			infs = append(infs, oem)
		}
		if err := d.set.CallClassInstaller(windows.DIF_REMOVE, d.data); err != nil {
			errs = append(errs, fmt.Errorf("remove the device: %w", err))
		}
	}
	closeAll(ds)
	// The driver package out of the driver store (oemNN.inf).
	for _, oem := range infs {
		p, _ := windows.UTF16PtrFromString(oem)
		if r, _, err := procSetupUninstallOEMInfW.Call(uintptr(unsafe.Pointer(p)), suoiForceDelete, 0); r == 0 {
			errs = append(errs, fmt.Errorf("remove %s from the driver store: %w", oem, err))
		}
	}
	return errors.Join(errs...)
}

func devices() ([]string, error) {
	ds, err := find()
	if err != nil {
		return nil, err
	}
	defer closeAll(ds)
	var out []string
	for _, d := range ds {
		id, _ := windows.SetupDiGetDeviceInstanceId(d.set, d.data)
		s := id
		if oem := d.oemInf(); oem != "" {
			s += " (" + oem + ")"
		} else {
			s += " (no driver)"
		}
		out = append(out, s)
	}
	return out, nil
}

// device is one of ours, with the set it was enumerated from.
type device struct {
	set  windows.DevInfo
	data *windows.DevInfoData
}

// find enumerates the root devices (present or not) with our hardware id.
func find() ([]device, error) {
	set, err := windows.SetupDiGetClassDevsEx(nil, "ROOT", 0, windows.DIGCF_ALLCLASSES, 0, "")
	if err != nil {
		return nil, err
	}
	var out []device
	for i := 0; ; i++ {
		data, err := set.EnumDeviceInfo(i)
		if err != nil {
			break // ERROR_NO_MORE_ITEMS
		}
		v, err := set.DeviceRegistryProperty(data, windows.SPDRP_HARDWAREID)
		if err != nil {
			continue
		}
		ids, _ := v.([]string)
		if slices.ContainsFunc(ids, func(s string) bool { return strings.EqualFold(s, HardwareID) }) {
			out = append(out, device{set: set, data: data})
		}
	}
	if len(out) == 0 {
		set.Close()
	}
	return out, nil
}

func closeAll(ds []device) {
	if len(ds) > 0 {
		ds[0].set.Close() // one set for all
	}
}

// oemInf is the driver store's name of the device's driver package
// (oemNN.inf; "" without a driver).
func (d device) oemInf() string {
	h, err := d.set.OpenDevRegKey(d.data, windows.DICS_FLAG_GLOBAL, 0, windows.DIREG_DRV, windows.KEY_READ)
	if err != nil {
		return ""
	}
	k := registry.Key(h)
	defer k.Close()
	s, _, _ := k.GetStringValue("InfPath")
	return s
}

func multiSZ(ss ...string) []byte {
	var u []uint16
	for _, s := range ss {
		w, _ := windows.UTF16FromString(s)
		u = append(u, w...)
	}
	u = append(u, 0)
	return unsafe.Slice((*byte)(unsafe.Pointer(&u[0])), len(u)*2)
}
