//go:build windows

package win

import (
	"runtime"
	"unicode/utf16"
	"unsafe"

	"golang.org/x/sys/windows"
)

var (
	comdlg32             = windows.NewLazySystemDLL("comdlg32.dll")
	procGetOpenFileNameW = comdlg32.NewProc("GetOpenFileNameW")
)

// openFileNameW mirrors OPENFILENAMEW.
type openFileNameW struct {
	lStructSize       uint32
	hwndOwner         uintptr
	hInstance         uintptr
	lpstrFilter       *uint16
	lpstrCustomFilter *uint16
	nMaxCustFilter    uint32
	nFilterIndex      uint32
	lpstrFile         *uint16
	nMaxFile          uint32
	lpstrFileTitle    *uint16
	nMaxFileTitle     uint32
	lpstrInitialDir   *uint16
	lpstrTitle        *uint16
	flags             uint32
	nFileOffset       uint16
	nFileExtension    uint16
	lpstrDefExt       *uint16
	lCustData         uintptr
	lpfnHook          uintptr
	lpTemplateName    *uint16
	pvReserved        uintptr
	dwReserved        uint32
	flagsEx           uint32
}

const (
	ofnFileMustExist = 0x00001000
	ofnPathMustExist = 0x00000800
	ofnExplorer      = 0x00080000
	ofnNoChangeDir   = 0x00000008
)

// Filter is a file dialog filter, e.g. {"SQLite databases", "*.db;*.sqlite"}.
type Filter struct{ Name, Pattern string }

// OpenFile shows the native Open dialog. It blocks, so call it from a
// goroutine other than the UI loop. ok is false if the user cancelled.
func OpenFile(owner uintptr, title string, filters []Filter) (path string, ok bool) {
	runtime.LockOSThread()
	defer runtime.UnlockOSThread()

	filter := encodeFilter(filters)

	buf := make([]uint16, 32*1024)
	ofn := openFileNameW{
		hwndOwner:   owner,
		lpstrFilter: &filter[0],
		lpstrFile:   &buf[0],
		nMaxFile:    uint32(len(buf)),
		flags:       ofnFileMustExist | ofnPathMustExist | ofnExplorer | ofnNoChangeDir,
	}
	ofn.lStructSize = uint32(unsafe.Sizeof(ofn))
	if title != "" {
		ofn.lpstrTitle, _ = windows.UTF16PtrFromString(title)
	}
	r, _, _ := procGetOpenFileNameW.Call(uintptr(unsafe.Pointer(&ofn)))
	if r == 0 {
		return "", false
	}
	return windows.UTF16ToString(buf), true
}

// encodeFilter builds the double-NUL-terminated UTF-16 filter block required
// by OPENFILENAMEW (windows.StringToUTF16 rejects strings containing NUL).
func encodeFilter(filters []Filter) []uint16 {
	var out []uint16
	for _, flt := range filters {
		out = append(out, utf16.Encode([]rune(flt.Name))...)
		out = append(out, 0)
		out = append(out, utf16.Encode([]rune(flt.Pattern))...)
		out = append(out, 0)
	}
	return append(out, 0, 0)
}
