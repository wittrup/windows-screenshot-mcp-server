package screenshot

import (
	"fmt"
	"strings"
	"time"
	"unicode/utf16"
	"unsafe"
)

// SendInput keyboard constants
const (
	INPUT_KEYBOARD        = 1
	KEYEVENTF_EXTENDEDKEY = 0x0001
	KEYEVENTF_KEYUP       = 0x0002
	KEYEVENTF_UNICODE     = 0x0004
)

// KEYBDINPUT for SendInput
type KEYBDINPUT struct {
	WVk         uint16
	WScan       uint16
	DwFlags     uint32
	Time        uint32
	_           [4]byte // padding on 64-bit
	DwExtraInfo uintptr
}

// KEYINPUT is the keyboard variant of INPUT, padded to the size of the
// mouse variant so SendInput accepts it on 64-bit.
type KEYINPUT struct {
	Type uint32
	_    [4]byte
	Ki   KEYBDINPUT
	_    [8]byte
}

// virtualKeys maps key names accepted by SendKeys to virtual-key codes.
var virtualKeys = map[string]uint16{
	"backspace": 0x08, "tab": 0x09, "enter": 0x0D, "return": 0x0D,
	"shift": 0x10, "ctrl": 0x11, "control": 0x11, "alt": 0x12,
	"pause": 0x13, "capslock": 0x14, "esc": 0x1B, "escape": 0x1B,
	"space": 0x20, "pageup": 0x21, "pagedown": 0x22, "end": 0x23,
	"home": 0x24, "left": 0x25, "up": 0x26, "right": 0x27, "down": 0x28,
	"insert": 0x2D, "delete": 0x2E, "del": 0x2E, "win": 0x5B,
	"apps": 0x5D, "menu": 0x5D,
	"f1": 0x70, "f2": 0x71, "f3": 0x72, "f4": 0x73, "f5": 0x74, "f6": 0x75,
	"f7": 0x76, "f8": 0x77, "f9": 0x78, "f10": 0x79, "f11": 0x7A, "f12": 0x7B,
}

// extendedKeys need KEYEVENTF_EXTENDEDKEY to be distinguished from numpad keys
var extendedKeys = map[uint16]bool{
	0x21: true, 0x22: true, 0x23: true, 0x24: true, 0x25: true, 0x26: true,
	0x27: true, 0x28: true, 0x2D: true, 0x2E: true, 0x5B: true, 0x5D: true,
}

func sendKeyInput(vk, scan uint16, flags uint32) error {
	in := KEYINPUT{Type: INPUT_KEYBOARD}
	in.Ki.WVk = vk
	in.Ki.WScan = scan
	in.Ki.DwFlags = flags
	ret, _, err := sendInput.Call(1, uintptr(unsafe.Pointer(&in)), unsafe.Sizeof(in))
	if ret == 0 {
		return fmt.Errorf("SendInput failed: %v", err)
	}
	return nil
}

func (e *WindowsScreenshotEngine) focusForInput(windowHandle uintptr) error {
	if windowHandle != 0 {
		if err := checkInputAllowed(windowHandle); err != nil {
			return err
		}
		setForegroundWindow.Call(windowHandle)
		time.Sleep(100 * time.Millisecond)
	}
	return checkInputAllowed(foregroundWindow())
}

// TypeText types a string as unicode keystrokes into the focused control.
// When windowHandle != 0 the window is brought to the foreground first.
func (e *WindowsScreenshotEngine) TypeText(text string, windowHandle uintptr) error {
	if err := e.focusForInput(windowHandle); err != nil {
		return err
	}
	for _, r := range text {
		if r == '\n' {
			if err := e.pressCombo([]uint16{0x0D}); err != nil {
				return err
			}
			continue
		}
		units := utf16.Encode([]rune{r})
		for _, u := range units {
			if err := sendKeyInput(0, u, KEYEVENTF_UNICODE); err != nil {
				return err
			}
			if err := sendKeyInput(0, u, KEYEVENTF_UNICODE|KEYEVENTF_KEYUP); err != nil {
				return err
			}
		}
		time.Sleep(5 * time.Millisecond)
	}
	return nil
}

// SendKeys presses a space-separated sequence of key combos, e.g.
// "ctrl+a delete", "alt+f", "tab tab enter". Single characters a-z/0-9 are allowed.
func (e *WindowsScreenshotEngine) SendKeys(keys string, windowHandle uintptr) error {
	combos := strings.Fields(keys)
	if len(combos) == 0 {
		return fmt.Errorf("no keys given")
	}
	var parsed [][]uint16
	for _, combo := range combos {
		var vks []uint16
		for _, name := range strings.Split(strings.ToLower(combo), "+") {
			vk, ok := virtualKeys[name]
			if !ok {
				if len(name) == 1 && ((name[0] >= 'a' && name[0] <= 'z') || (name[0] >= '0' && name[0] <= '9')) {
					vk = uint16(strings.ToUpper(name)[0])
				} else {
					return fmt.Errorf("unknown key: %q", name)
				}
			}
			vks = append(vks, vk)
		}
		parsed = append(parsed, vks)
	}

	if err := e.focusForInput(windowHandle); err != nil {
		return err
	}
	for _, vks := range parsed {
		if err := e.pressCombo(vks); err != nil {
			return err
		}
		time.Sleep(30 * time.Millisecond)
	}
	return nil
}

// pressCombo presses keys in order and releases them in reverse order.
func (e *WindowsScreenshotEngine) pressCombo(vks []uint16) error {
	for _, vk := range vks {
		var flags uint32
		if extendedKeys[vk] {
			flags |= KEYEVENTF_EXTENDEDKEY
		}
		if err := sendKeyInput(vk, 0, flags); err != nil {
			return err
		}
		time.Sleep(10 * time.Millisecond)
	}
	for i := len(vks) - 1; i >= 0; i-- {
		flags := uint32(KEYEVENTF_KEYUP)
		if extendedKeys[vks[i]] {
			flags |= KEYEVENTF_EXTENDEDKEY
		}
		if err := sendKeyInput(vks[i], 0, flags); err != nil {
			return err
		}
	}
	return nil
}
