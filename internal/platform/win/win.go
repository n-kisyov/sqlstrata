//go:build windows

// Package win contains the small amount of Win32 integration SQLStrata needs:
// system theme detection, DWM window styling, native file dialogs and access
// to system fonts. Everything goes through golang.org/x/sys/windows (no CGO).
package win

import (
	"image/color"
	"os"
	"path/filepath"
	"unsafe"

	"golang.org/x/sys/windows"
	"golang.org/x/sys/windows/registry"
)

var (
	dwmapi                    = windows.NewLazySystemDLL("dwmapi.dll")
	procDwmSetWindowAttribute = dwmapi.NewProc("DwmSetWindowAttribute")
	procDwmGetColorization    = dwmapi.NewProc("DwmGetColorizationColor")
)

// DWM window attributes (dwmapi.h, Windows 11).
const (
	dwmwaUseImmersiveDarkMode  = 20
	dwmwaWindowCornerPref      = 33
	dwmwaBorderColor           = 34
	dwmwaCaptionColor          = 35
	dwmwaTextColor             = 36
	dwmwaSystemBackdropType    = 38
	dwmwcpRound                = 2
	dwmsbtNone                 = 1
)

// SystemDarkMode reports whether the user chose dark mode for apps.
func SystemDarkMode() bool {
	k, err := registry.OpenKey(registry.CURRENT_USER, `Software\Microsoft\Windows\CurrentVersion\Themes\Personalize`, registry.QUERY_VALUE)
	if err != nil {
		return false
	}
	defer k.Close()
	v, _, err := k.GetIntegerValue("AppsUseLightTheme")
	return err == nil && v == 0
}

// AccentColor returns the system accent colour, if available.
func AccentColor() (color.NRGBA, bool) {
	k, err := registry.OpenKey(registry.CURRENT_USER, `Software\Microsoft\Windows\DWM`, registry.QUERY_VALUE)
	if err == nil {
		defer k.Close()
		// AccentColor is stored as ABGR.
		if v, _, err := k.GetIntegerValue("AccentColor"); err == nil {
			return color.NRGBA{R: uint8(v), G: uint8(v >> 8), B: uint8(v >> 16), A: 0xff}, true
		}
	}
	var argb uint32
	var opaque int32
	if procDwmGetColorization.Find() == nil {
		if r, _, _ := procDwmGetColorization.Call(uintptr(unsafe.Pointer(&argb)), uintptr(unsafe.Pointer(&opaque))); r == 0 {
			return color.NRGBA{R: uint8(argb >> 16), G: uint8(argb >> 8), B: uint8(argb), A: 0xff}, true
		}
	}
	return color.NRGBA{}, false
}

func colorref(c color.NRGBA) uint32 {
	return uint32(c.R) | uint32(c.G)<<8 | uint32(c.B)<<16
}

func setAttr(hwnd uintptr, attr uint32, val uint32) {
	if procDwmSetWindowAttribute.Find() != nil {
		return
	}
	procDwmSetWindowAttribute.Call(hwnd, uintptr(attr), uintptr(unsafe.Pointer(&val)), unsafe.Sizeof(val))
}

// StyleWindow makes the native title bar blend with the app theme: dark or
// light mode, rounded corners and a caption colour matching the app chrome.
// Attributes unsupported by the running Windows version are ignored.
func StyleWindow(hwnd uintptr, dark bool, caption, text, border color.NRGBA) {
	if hwnd == 0 {
		return
	}
	var d uint32
	if dark {
		d = 1
	}
	setAttr(hwnd, dwmwaUseImmersiveDarkMode, d)
	setAttr(hwnd, dwmwaWindowCornerPref, dwmwcpRound)
	setAttr(hwnd, dwmwaSystemBackdropType, dwmsbtNone)
	setAttr(hwnd, dwmwaCaptionColor, colorref(caption))
	setAttr(hwnd, dwmwaTextColor, colorref(text))
	setAttr(hwnd, dwmwaBorderColor, colorref(border))
}

// FontPath returns the path of a font in the Windows font directory if it
// exists. Fonts are loaded from the system rather than embedded.
func FontPath(names ...string) (string, bool) {
	dirs := []string{filepath.Join(os.Getenv("WINDIR"), "Fonts")}
	if la := os.Getenv("LOCALAPPDATA"); la != "" {
		dirs = append(dirs, filepath.Join(la, `Microsoft\Windows\Fonts`))
	}
	for _, n := range names {
		for _, d := range dirs {
			p := filepath.Join(d, n)
			if _, err := os.Stat(p); err == nil {
				return p, true
			}
		}
	}
	return "", false
}

