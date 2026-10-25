package screenshot

import (
	"fmt"
	"unsafe"

	"golang.org/x/sys/windows"
)

var (
	windowFromPoint     = user32.NewProc("WindowFromPoint")
	getForegroundWindow = user32.NewProc("GetForegroundWindow")
)

// integrityLevel returns the mandatory integrity RID of a process token
// (0x2000 medium, 0x3000 high, 0x4000 system).
func integrityLevel(process windows.Handle) (uint32, error) {
	var token windows.Token
	if err := windows.OpenProcessToken(process, windows.TOKEN_QUERY, &token); err != nil {
		return 0, err
	}
	defer token.Close()

	var size uint32
	windows.GetTokenInformation(token, windows.TokenIntegrityLevel, nil, 0, &size)
	if size == 0 {
		return 0, fmt.Errorf("GetTokenInformation returned no size")
	}
	buf := make([]byte, size)
	if err := windows.GetTokenInformation(token, windows.TokenIntegrityLevel, &buf[0], size, &size); err != nil {
		return 0, err
	}
	label := (*windows.Tokenmandatorylabel)(unsafe.Pointer(&buf[0]))
	sid := label.Label.Sid
	return sid.SubAuthority(uint32(sid.SubAuthorityCount() - 1)), nil
}

// checkInputAllowed returns an error when hwnd belongs to a process with a
// higher integrity level than this server. Windows UIPI silently drops
// SendInput/PostMessage input to such windows (e.g. apps marked "Run as
// administrator"), so the caller would otherwise report a click that never landed.
func checkInputAllowed(hwnd uintptr) error {
	if hwnd == 0 {
		return nil
	}
	var pid uint32
	getWindowThreadProcessId.Call(hwnd, uintptr(unsafe.Pointer(&pid)))
	if pid == 0 {
		return nil
	}

	self, err := integrityLevel(windows.CurrentProcess())
	if err != nil {
		return nil
	}
	h, err := windows.OpenProcess(windows.PROCESS_QUERY_LIMITED_INFORMATION, false, pid)
	if err != nil {
		return nil
	}
	defer windows.CloseHandle(h)
	target, err := integrityLevel(h)
	if err != nil {
		return nil
	}

	if target > self {
		return fmt.Errorf("target window (PID %d) runs at integrity level 0x%X, higher than this server (0x%X); "+
			"Windows UIPI blocks injected input. Run the screenshot server elevated (as administrator) "+
			"or start the target application without elevation", pid, target, self)
	}
	return nil
}

func windowAtScreenPoint(x, y int) uintptr {
	pt := uintptr(uint32(int32(x))) | uintptr(uint32(int32(y)))<<32
	hwnd, _, _ := windowFromPoint.Call(pt)
	return hwnd
}

func foregroundWindow() uintptr {
	hwnd, _, _ := getForegroundWindow.Call()
	return hwnd
}
