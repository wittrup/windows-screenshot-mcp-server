package screenshot

import (
	"fmt"
	"runtime"
	"strings"
	"syscall"
	"time"
	"unsafe"

	"github.com/screenshot-mcp-server/pkg/types"
	"golang.org/x/sys/windows"
)

var (
	// Windows API DLLs
	user32    = windows.NewLazyDLL("user32.dll")
	gdi32     = windows.NewLazyDLL("gdi32.dll")
	dwmapi    = windows.NewLazyDLL("dwmapi.dll")
	shcore    = windows.NewLazyDLL("shcore.dll")
	kernel32  = windows.NewLazyDLL("kernel32.dll")
	
	// User32 functions
	findWindowW           = user32.NewProc("FindWindowW")
	getWindowTextW        = user32.NewProc("GetWindowTextW")
	getWindowTextLengthW  = user32.NewProc("GetWindowTextLengthW")
	getWindowRect         = user32.NewProc("GetWindowRect")
	getClientRect         = user32.NewProc("GetClientRect")
	getWindowDC           = user32.NewProc("GetWindowDC")
	getDC                 = user32.NewProc("GetDC")
	releaseDC             = user32.NewProc("ReleaseDC")
	getDesktopWindow      = user32.NewProc("GetDesktopWindow")
	printWindow           = user32.NewProc("PrintWindow")
	isWindowVisible       = user32.NewProc("IsWindowVisible")
	isIconic              = user32.NewProc("IsIconic")
	showWindow            = user32.NewProc("ShowWindow")
	setProcessDPIAware    = user32.NewProc("SetProcessDPIAware")
	getWindowThreadProcessId = user32.NewProc("GetWindowThreadProcessId")
	enumWindows           = user32.NewProc("EnumWindows")
	getClassName          = user32.NewProc("GetClassNameW")

	// Window control functions
	setForegroundWindow   = user32.NewProc("SetForegroundWindow")
	moveWindow            = user32.NewProc("MoveWindow")
	setWindowPos          = user32.NewProc("SetWindowPos")

	// Mouse / keyboard input functions
	setCursorPos          = user32.NewProc("SetCursorPos")
	getCursorPos          = user32.NewProc("GetCursorPos")
	sendInput             = user32.NewProc("SendInput")
	clientToScreen        = user32.NewProc("ClientToScreen")
	screenToClient        = user32.NewProc("ScreenToClient")
	getSystemMetrics      = user32.NewProc("GetSystemMetrics")
	childWindowFromPoint  = user32.NewProc("ChildWindowFromPoint")
	mapWindowPoints       = user32.NewProc("MapWindowPoints")
	
	// GDI32 functions
	createCompatibleDC    = gdi32.NewProc("CreateCompatibleDC")
	createCompatibleBitmap = gdi32.NewProc("CreateCompatibleBitmap")
	selectObject          = gdi32.NewProc("SelectObject")
	bitBlt                = gdi32.NewProc("BitBlt")
	deleteDC              = gdi32.NewProc("DeleteDC")
	deleteObject          = gdi32.NewProc("DeleteObject")
	getDIBits             = gdi32.NewProc("GetDIBits")
	createDIBSection      = gdi32.NewProc("CreateDIBSection")
	getDeviceCaps         = gdi32.NewProc("GetDeviceCaps")
	
	// DWM functions
	dwmGetWindowAttribute = dwmapi.NewProc("DwmGetWindowAttribute")
	dwmIsCompositionEnabled = dwmapi.NewProc("DwmIsCompositionEnabled")
	
	// ShCore functions (for DPI awareness)
	setProcessDpiAwareness = shcore.NewProc("SetProcessDpiAwareness")
	getDpiForMonitor       = shcore.NewProc("GetDpiForMonitor")

	// Kernel32 functions
	closeHandle = kernel32.NewProc("CloseHandle")
)

// Windows API constants
const (
	SRCCOPY             = 0x00CC0020
	DIB_RGB_COLORS      = 0
	BI_RGB              = 0
	PW_CLIENTONLY       = 1
	PW_RENDERFULLCONTENT = 2
	SW_RESTORE          = 9
	SW_SHOW             = 5
	SW_MINIMIZE         = 6
	LOGPIXELSX          = 88
	LOGPIXELSY          = 90
	DWMWA_EXTENDED_FRAME_BOUNDS = 9
	PROCESS_DPI_AWARE   = 1
	MDT_EFFECTIVE_DPI   = 0
)

// RECT structure for Windows API
type RECT struct {
	Left, Top, Right, Bottom int32
}

// BITMAPINFOHEADER structure
type BITMAPINFOHEADER struct {
	Size          uint32
	Width         int32
	Height        int32
	Planes        uint16
	BitCount      uint16
	Compression   uint32
	SizeImage     uint32
	XPelsPerMeter int32
	YPelsPerMeter int32
	ClrUsed       uint32
	ClrImportant  uint32
}

// BITMAPINFO structure
type BITMAPINFO struct {
	Header BITMAPINFOHEADER
	Colors [1]uint32
}

// WindowsScreenshotEngine implements the ScreenshotEngine interface
type WindowsScreenshotEngine struct {
	dpiAware bool
}

// NewEngine creates a new Windows screenshot engine
func NewEngine() (*WindowsScreenshotEngine, error) {
	engine := &WindowsScreenshotEngine{}
	
	// Enable DPI awareness
	if err := engine.enableDPIAwareness(); err != nil {
		return nil, fmt.Errorf("failed to enable DPI awareness: %w", err)
	}
	
	return engine, nil
}

// enableDPIAwareness enables DPI awareness for the process
func (e *WindowsScreenshotEngine) enableDPIAwareness() error {
	// Try SetProcessDpiAwareness first (Windows 8.1+)
	if setProcessDpiAwareness.Find() == nil {
		ret, _, _ := setProcessDpiAwareness.Call(uintptr(PROCESS_DPI_AWARE))
		if ret == 0 {
			e.dpiAware = true
			return nil
		}
	}
	
	// Fallback to SetProcessDPIAware (Windows Vista+)
	if setProcessDPIAware.Find() == nil {
		ret, _, _ := setProcessDPIAware.Call()
		if ret != 0 {
			e.dpiAware = true
			return nil
		}
	}
	
	return fmt.Errorf("failed to enable DPI awareness")
}

// CaptureByHandle captures a screenshot of a window by its handle
func (e *WindowsScreenshotEngine) CaptureByHandle(handle uintptr, options *types.CaptureOptions) (*types.ScreenshotBuffer, error) {
	options = e.normalizeCaptureOptions(options)
	
	startTime := time.Now()
	
	// Get window information
	windowInfo, err := e.getWindowInfo(handle)
	if err != nil {
		return nil, fmt.Errorf("failed to get window info: %w", err)
	}
	
	// Check if window is minimized and handle accordingly
	isMinimized := e.isWindowMinimized(handle)
	wasRestored := false
	
	if isMinimized && options.RestoreWindow {
		if err := e.restoreWindow(handle); err != nil {
			return nil, fmt.Errorf("failed to restore window: %w", err)
		}
		wasRestored = true
		
		// Wait for window to become visible
		if options.WaitForVisible > 0 {
			time.Sleep(options.WaitForVisible)
		}
	}
	
	// Capture the screenshot
	var buffer *types.ScreenshotBuffer
	if e.shouldPreferFallbackCapture(windowInfo, options) {
		buffer, err = e.captureWithRetryFallbacks(handle, windowInfo, options)
	} else {
		if isMinimized && options.AllowMinimized && !options.RestoreWindow {
			// Use DWM/PrintWindow for minimized windows
			buffer, err = e.captureMinimizedWindow(handle, windowInfo, options)
		} else {
			// Use BitBlt for visible windows
			buffer, err = e.captureVisibleWindow(handle, windowInfo, options)
		}

		if err == nil && e.isLikelyBlankCapture(buffer) {
			buffer, err = e.captureWithRetryFallbacks(handle, windowInfo, options)
		}
	}
	
	if err != nil {
		return nil, fmt.Errorf("failed to capture window: %w", err)
	}
	
	// Restore original window state if we changed it
	if wasRestored && isMinimized {
		// Minimize the window again
		showWindow.Call(handle, uintptr(6)) // SW_MINIMIZE
	}
	
	// Fill in metadata
	buffer.Timestamp = time.Now()
	buffer.WindowInfo = *windowInfo
	
	// Processing time is calculated and used in metadata
	_ = time.Since(startTime)
	
	return buffer, nil
}

// CaptureByTitle captures a screenshot by window title
func (e *WindowsScreenshotEngine) CaptureByTitle(title string, options *types.CaptureOptions) (*types.ScreenshotBuffer, error) {
	handle, err := e.findWindowByTitle(title)
	if err != nil {
		return nil, fmt.Errorf("failed to find window with title '%s': %w", title, err)
	}
	
	return e.CaptureByHandle(handle, options)
}

// CaptureByPID captures a screenshot by process ID
func (e *WindowsScreenshotEngine) CaptureByPID(pid uint32, options *types.CaptureOptions) (*types.ScreenshotBuffer, error) {
	handle, err := e.findWindowByPID(pid)
	if err != nil {
		return nil, fmt.Errorf("failed to find window with PID %d: %w", pid, err)
	}
	
	return e.CaptureByHandle(handle, options)
}

// CaptureByClassName captures a screenshot by window class name
func (e *WindowsScreenshotEngine) CaptureByClassName(className string, options *types.CaptureOptions) (*types.ScreenshotBuffer, error) {
	handle, err := e.findWindowByClassName(className)
	if err != nil {
		return nil, fmt.Errorf("failed to find window with class '%s': %w", className, err)
	}
	
	return e.CaptureByHandle(handle, options)
}

// CaptureFullScreen captures the full screen
func (e *WindowsScreenshotEngine) CaptureFullScreen(monitor int, options *types.CaptureOptions) (*types.ScreenshotBuffer, error) {
	// Get desktop window handle
	desktopHandle, _, _ := getDesktopWindow.Call()
	if desktopHandle == 0 {
		return nil, fmt.Errorf("failed to get desktop window")
	}
	
	return e.CaptureByHandle(desktopHandle, options)
}

// captureVisibleWindow captures a visible window using BitBlt
func (e *WindowsScreenshotEngine) captureVisibleWindow(handle uintptr, windowInfo *types.WindowInfo, options *types.CaptureOptions) (*types.ScreenshotBuffer, error) {
	// Get window device context
	var hdc uintptr
	if options.IncludeFrame {
		hdc, _, _ = getWindowDC.Call(handle)
	} else {
		hdc, _, _ = getDC.Call(handle)
	}
	
	if hdc == 0 {
		return nil, fmt.Errorf("failed to get window DC")
	}
	defer releaseDC.Call(handle, hdc)
	
	// Determine capture dimensions and source origin.
	// Window DC coordinates are relative to the window/client origin rather than
	// absolute screen coordinates, so the default capture source is (0,0).
	var rect types.Rectangle
	sourceX := 0
	sourceY := 0
	if options.Region != nil {
		rect = *options.Region
		sourceX = rect.X
		sourceY = rect.Y
	} else if options.IncludeFrame {
		rect = windowInfo.Rect
	} else {
		rect = windowInfo.ClientRect
	}
	
	if rect.Width <= 0 || rect.Height <= 0 {
		return nil, fmt.Errorf("invalid capture dimensions: %dx%d", rect.Width, rect.Height)
	}
	
	// Create compatible DC and bitmap
	memDC, _, _ := createCompatibleDC.Call(hdc)
	if memDC == 0 {
		return nil, fmt.Errorf("failed to create compatible DC")
	}
	defer deleteDC.Call(memDC)
	
	// Create DIB section for direct pixel access
	var bmi BITMAPINFO
	bmi.Header.Size = uint32(unsafe.Sizeof(bmi.Header))
	bmi.Header.Width = int32(rect.Width)
	bmi.Header.Height = -int32(rect.Height) // Negative height for top-down DIB
	bmi.Header.Planes = 1
	bmi.Header.BitCount = 32 // 32-bit BGRA
	bmi.Header.Compression = BI_RGB
	
	var pBits uintptr
	bitmap, _, _ := createDIBSection.Call(memDC, uintptr(unsafe.Pointer(&bmi)), DIB_RGB_COLORS, uintptr(unsafe.Pointer(&pBits)), 0, 0)
	if bitmap == 0 {
		return nil, fmt.Errorf("failed to create DIB section")
	}
	defer deleteObject.Call(bitmap)
	
	// Select bitmap into memory DC
	oldBitmap, _, _ := selectObject.Call(memDC, bitmap)
	defer selectObject.Call(memDC, oldBitmap)
	
	// Copy pixels from window to memory DC
	ret, _, _ := bitBlt.Call(
		memDC, 0, 0, uintptr(rect.Width), uintptr(rect.Height),
		hdc, uintptr(sourceX), uintptr(sourceY), SRCCOPY,
	)
	
	if ret == 0 {
		return nil, fmt.Errorf("BitBlt failed")
	}
	
	// Get DPI information
	dpiX, _, _ := getDeviceCaps.Call(hdc, LOGPIXELSX)
	_, _, _ = getDeviceCaps.Call(hdc, LOGPIXELSY) // dpiY for future use
	
	// Copy pixel data
	pixelCount := rect.Width * rect.Height * 4 // 4 bytes per pixel (BGRA)
	pixelData := make([]byte, pixelCount)
	
	// Use unsafe pointer to copy memory directly
	if pBits != 0 {
		copy(pixelData, (*[1 << 30]byte)(unsafe.Pointer(pBits))[:pixelCount:pixelCount])
	}
	
	// Create screenshot buffer
	buffer := &types.ScreenshotBuffer{
		Data:       pixelData,
		Width:      rect.Width,
		Height:     rect.Height,
		Stride:     rect.Width * 4,
		Format:     "BGRA32",
		DPI:        int(dpiX),
		SourceRect: rect,
	}
	
	return buffer, nil
}

// captureMinimizedWindow captures a minimized window using PrintWindow or DWM
func (e *WindowsScreenshotEngine) captureMinimizedWindow(handle uintptr, windowInfo *types.WindowInfo, options *types.CaptureOptions) (*types.ScreenshotBuffer, error) {
	// Try PrintWindow first
	buffer, err := e.tryPrintWindow(handle, windowInfo, options)
	if err == nil {
		return buffer, nil
	}
	
	// Fallback: temporarily restore window
	if options.RetryCount > 0 {
		tempOptions := *options
		tempOptions.RestoreWindow = true
		tempOptions.RetryCount = 0
		
		return e.CaptureByHandle(handle, &tempOptions)
	}
	
	return nil, fmt.Errorf("failed to capture minimized window: %w", err)
}

// tryPrintWindow attempts to use PrintWindow API for off-screen rendering
func (e *WindowsScreenshotEngine) tryPrintWindow(handle uintptr, windowInfo *types.WindowInfo, options *types.CaptureOptions) (*types.ScreenshotBuffer, error) {
	// Get window dimensions
	rect := windowInfo.Rect
	if rect.Width <= 0 || rect.Height <= 0 {
		return nil, fmt.Errorf("invalid window dimensions")
	}
	
	// Create device context
	screenDC, _, _ := getDC.Call(0)
	if screenDC == 0 {
		return nil, fmt.Errorf("failed to get screen DC")
	}
	defer releaseDC.Call(0, screenDC)
	
	// Create compatible DC and bitmap
	memDC, _, _ := createCompatibleDC.Call(screenDC)
	if memDC == 0 {
		return nil, fmt.Errorf("failed to create compatible DC")
	}
	defer deleteDC.Call(memDC)
	
	// Create DIB section
	var bmi BITMAPINFO
	bmi.Header.Size = uint32(unsafe.Sizeof(bmi.Header))
	bmi.Header.Width = int32(rect.Width)
	bmi.Header.Height = -int32(rect.Height)
	bmi.Header.Planes = 1
	bmi.Header.BitCount = 32
	bmi.Header.Compression = BI_RGB
	
	var pBits uintptr
	bitmap, _, _ := createDIBSection.Call(memDC, uintptr(unsafe.Pointer(&bmi)), DIB_RGB_COLORS, uintptr(unsafe.Pointer(&pBits)), 0, 0)
	if bitmap == 0 {
		return nil, fmt.Errorf("failed to create DIB section")
	}
	defer deleteObject.Call(bitmap)
	
	// Select bitmap
	oldBitmap, _, _ := selectObject.Call(memDC, bitmap)
	defer selectObject.Call(memDC, oldBitmap)
	
	// Use PrintWindow to render to our DC
	// PW_RENDERFULLCONTENT (0x2) forces rendering of hardware-accelerated
	// content (OpenGL, DirectX) into the DC — critical for Qt 3D views.
	flags := uintptr(PW_RENDERFULLCONTENT)
	if !options.IncludeFrame {
		flags |= PW_CLIENTONLY
	}
	
	ret, _, _ := printWindow.Call(handle, memDC, flags)
	if ret == 0 {
		return nil, fmt.Errorf("PrintWindow failed")
	}
	
	// Copy pixel data
	pixelCount := rect.Width * rect.Height * 4
	pixelData := make([]byte, pixelCount)
	
	if pBits != 0 {
		copy(pixelData, (*[1 << 30]byte)(unsafe.Pointer(pBits))[:pixelCount:pixelCount])
	}
	
	// Create screenshot buffer
	buffer := &types.ScreenshotBuffer{
		Data:       pixelData,
		Width:      rect.Width,
		Height:     rect.Height,
		Stride:     rect.Width * 4,
		Format:     "BGRA32",
		DPI:        96, // Default DPI for PrintWindow
		SourceRect: rect,
	}
	
	return buffer, nil
}

// Helper functions

func (e *WindowsScreenshotEngine) findWindowByTitle(title string) (uintptr, error) {
	titlePtr, _ := syscall.UTF16PtrFromString(title)
	handle, _, _ := findWindowW.Call(0, uintptr(unsafe.Pointer(titlePtr)))
	if handle == 0 {
		return 0, fmt.Errorf("window not found")
	}
	return handle, nil
}

func (e *WindowsScreenshotEngine) findWindowByClassName(className string) (uintptr, error) {
	classPtr, _ := syscall.UTF16PtrFromString(className)
	handle, _, _ := findWindowW.Call(uintptr(unsafe.Pointer(classPtr)), 0)
	if handle == 0 {
		return 0, fmt.Errorf("window not found")
	}
	return handle, nil
}

func (e *WindowsScreenshotEngine) findWindowByPID(targetPID uint32) (uintptr, error) {
	var foundHandle uintptr
	
	// Callback function for EnumWindows
	callback := syscall.NewCallback(func(hwnd, lParam uintptr) uintptr {
		var pid uint32
		getWindowThreadProcessId.Call(hwnd, uintptr(unsafe.Pointer(&pid)))
		
		if pid == targetPID {
			// Check if window is visible and has a title
			visible, _, _ := isWindowVisible.Call(hwnd)
			if visible != 0 {
				titleLen, _, _ := getWindowTextLengthW.Call(hwnd)
				if titleLen > 0 {
					foundHandle = hwnd
					return 0 // Stop enumeration
				}
			}
		}
		return 1 // Continue enumeration
	})
	
	enumWindows.Call(callback, 0)
	
	if foundHandle == 0 {
		return 0, fmt.Errorf("no visible window found for PID %d", targetPID)
	}
	
	return foundHandle, nil
}

func (e *WindowsScreenshotEngine) getWindowInfo(handle uintptr) (*types.WindowInfo, error) {
	info := &types.WindowInfo{
		Handle: handle,
	}
	
	// Get window title
	titleLen, _, _ := getWindowTextLengthW.Call(handle)
	if titleLen > 0 {
		titleBuf := make([]uint16, titleLen+1)
		getWindowTextW.Call(handle, uintptr(unsafe.Pointer(&titleBuf[0])), uintptr(len(titleBuf)))
		info.Title = syscall.UTF16ToString(titleBuf)
	}
	
	// Get class name
	classBuf := make([]uint16, 256)
	getClassName.Call(handle, uintptr(unsafe.Pointer(&classBuf[0])), 256)
	info.ClassName = syscall.UTF16ToString(classBuf)
	
	// Get process and thread IDs
	var pid uint32
	threadID, _, _ := getWindowThreadProcessId.Call(handle, uintptr(unsafe.Pointer(&pid)))
	info.ProcessID = pid
	info.ThreadID = uint32(threadID)
	
	// Get window rectangle
	var rect RECT
	getWindowRect.Call(handle, uintptr(unsafe.Pointer(&rect)))
	info.Rect = types.Rectangle{
		X:      int(rect.Left),
		Y:      int(rect.Top),
		Width:  int(rect.Right - rect.Left),
		Height: int(rect.Bottom - rect.Top),
	}
	
	// Get client rectangle
	var clientRect RECT
	getClientRect.Call(handle, uintptr(unsafe.Pointer(&clientRect)))
	info.ClientRect = types.Rectangle{
		X:      0,
		Y:      0,
		Width:  int(clientRect.Right),
		Height: int(clientRect.Bottom),
	}
	
	// Check window state
	visible, _, _ := isWindowVisible.Call(handle)
	info.IsVisible = visible != 0
	
	minimized, _, _ := isIconic.Call(handle)
	if minimized != 0 {
		info.State = "minimized"
	} else if info.IsVisible {
		info.State = "visible"
	} else {
		info.State = "hidden"
	}
	
	return info, nil
}

func (e *WindowsScreenshotEngine) isWindowMinimized(handle uintptr) bool {
	ret, _, _ := isIconic.Call(handle)
	return ret != 0
}

func (e *WindowsScreenshotEngine) restoreWindow(handle uintptr) error {
	ret, _, _ := showWindow.Call(handle, SW_RESTORE)
	if ret == 0 {
		return fmt.Errorf("failed to restore window")
	}
	return nil
}

func (e *WindowsScreenshotEngine) normalizeCaptureOptions(options *types.CaptureOptions) *types.CaptureOptions {
	if options == nil {
		return types.DefaultCaptureOptions()
	}

	normalized := *options
	if normalized.WaitForVisible == 0 {
		normalized.WaitForVisible = 2 * time.Second
	}
	if normalized.RetryCount == 0 {
		normalized.RetryCount = 3
	}
	if normalized.PreferredMethod == "" {
		normalized.PreferredMethod = types.CaptureAuto
	}
	if normalized.FallbackMethods == nil {
		normalized.FallbackMethods = []types.CaptureMethod{
			types.CapturePrintWindow,
			types.CaptureWMPrint,
			types.CaptureDWMThumbnail,
			types.CaptureStealthRestore,
		}
	}
	if normalized.CustomProperties == nil {
		normalized.CustomProperties = make(map[string]string)
	}

	return &normalized
}

func (e *WindowsScreenshotEngine) shouldPreferFallbackCapture(windowInfo *types.WindowInfo, options *types.CaptureOptions) bool {
	if windowInfo == nil || options == nil {
		return false
	}

	className := strings.ToLower(windowInfo.ClassName)
	if strings.HasPrefix(className, "qt") {
		return true
	}
	if windowInfo.State != "visible" {
		return true
	}
	if options.UseDWMThumbnails || options.ForceRender {
		return true
	}
	if options.PreferredMethod != "" && options.PreferredMethod != types.CaptureAuto {
		return true
	}

	return false
}

func (e *WindowsScreenshotEngine) preferredRetryMethod(windowInfo *types.WindowInfo) types.CaptureMethod {
	if windowInfo == nil {
		return types.CapturePrintWindow
	}

	className := strings.ToLower(windowInfo.ClassName)
	switch {
	case strings.HasPrefix(className, "qt"):
		return types.CapturePrintWindow
	case windowInfo.State == "hidden" || windowInfo.State == "cloaked":
		return types.CaptureWMPrint
	default:
		return types.CapturePrintWindow
	}
}

func (e *WindowsScreenshotEngine) captureWithRetryFallbacks(handle uintptr, windowInfo *types.WindowInfo, options *types.CaptureOptions) (*types.ScreenshotBuffer, error) {
	fallbackOptions := *options
	if fallbackOptions.PreferredMethod == types.CaptureAuto {
		fallbackOptions.PreferredMethod = e.preferredRetryMethod(windowInfo)
	}

	return e.CaptureWithFallbacks(handle, &fallbackOptions)
}

func (e *WindowsScreenshotEngine) isLikelyBlankCapture(buffer *types.ScreenshotBuffer) bool {
	if buffer == nil || len(buffer.Data) == 0 {
		return true
	}
	if buffer.Width <= 2 || buffer.Height <= 2 {
		return true
	}
	if len(buffer.Data) < 4 {
		return true
	}
	if buffer.Format != "BGRA32" && buffer.Format != "RGBA32" {
		return false
	}

	pixelCount := len(buffer.Data) / 4
	step := 1
	if pixelCount > 4096 {
		step = pixelCount / 4096
	}

	firstB := buffer.Data[0]
	firstG := buffer.Data[1]
	firstR := buffer.Data[2]
	firstA := buffer.Data[3]

	sampled := 0
	blackish := 0
	transparent := 0
	different := 0

	for i := 0; i < pixelCount; i += step {
		idx := i * 4
		b := buffer.Data[idx]
		g := buffer.Data[idx+1]
		r := buffer.Data[idx+2]
		a := buffer.Data[idx+3]

		sampled++
		if int(r)+int(g)+int(b) <= 12 {
			blackish++
		}
		if a <= 4 {
			transparent++
		}
		if absInt(int(b)-int(firstB)) > 4 ||
			absInt(int(g)-int(firstG)) > 4 ||
			absInt(int(r)-int(firstR)) > 4 ||
			absInt(int(a)-int(firstA)) > 4 {
			different++
		}
	}

	if sampled == 0 {
		return true
	}

	mostlyBlack := float64(blackish)/float64(sampled) >= 0.98
	mostlyTransparent := float64(transparent)/float64(sampled) >= 0.98
	littleVariation := float64(different)/float64(sampled) <= 0.02

	return littleVariation && (mostlyBlack || mostlyTransparent)
}

func absInt(v int) int {
	if v < 0 {
		return -v
	}
	return v
}

// ListVisibleWindows enumerates all visible windows with titles
func (e *WindowsScreenshotEngine) ListVisibleWindows() ([]types.WindowInfo, error) {
	var windows []types.WindowInfo

	callback := syscall.NewCallback(func(hwnd, lParam uintptr) uintptr {
		visible, _, _ := isWindowVisible.Call(hwnd)
		if visible == 0 {
			return 1
		}

		titleLen, _, _ := getWindowTextLengthW.Call(hwnd)
		if titleLen == 0 {
			return 1
		}

		if info, err := e.getWindowInfo(hwnd); err == nil {
			windows = append(windows, *info)
		}
		return 1
	})

	enumWindows.Call(callback, 0)
	return windows, nil
}

// ControlWindow changes a window's state, position, or size.
// Supported actions: restore, maximize, minimize, focus, move, resize.
func (e *WindowsScreenshotEngine) ControlWindow(handle uintptr, action string, x, y, width, height int) (*types.WindowInfo, error) {
	switch action {
	case "restore":
		showWindow.Call(handle, SW_RESTORE)
	case "maximize":
		showWindow.Call(handle, SW_MAXIMIZE)
	case "minimize":
		showWindow.Call(handle, SW_MINIMIZE)
	case "focus":
		setForegroundWindow.Call(handle)
		showWindow.Call(handle, SW_RESTORE)
	case "move":
		// Keep current size, move to x,y
		// Use int32 cast to preserve sign for negative multi-monitor coordinates
		var rect RECT
		getWindowRect.Call(handle, uintptr(unsafe.Pointer(&rect)))
		curW := int32(rect.Right - rect.Left)
		curH := int32(rect.Bottom - rect.Top)
		ret, _, _ := moveWindow.Call(handle, uintptr(int32(x)), uintptr(int32(y)), uintptr(curW), uintptr(curH), 1)
		if ret == 0 {
			return nil, fmt.Errorf("MoveWindow failed")
		}
	case "resize":
		// Keep current position, change size
		var rect RECT
		getWindowRect.Call(handle, uintptr(unsafe.Pointer(&rect)))
		ret, _, _ := moveWindow.Call(handle, uintptr(rect.Left), uintptr(rect.Top), uintptr(int32(width)), uintptr(int32(height)), 1)
		if ret == 0 {
			return nil, fmt.Errorf("MoveWindow failed")
		}
	case "move_resize":
		ret, _, _ := moveWindow.Call(handle, uintptr(int32(x)), uintptr(int32(y)), uintptr(int32(width)), uintptr(int32(height)), 1)
		if ret == 0 {
			return nil, fmt.Errorf("MoveWindow failed")
		}
	default:
		return nil, fmt.Errorf("unsupported action: %s", action)
	}

	// Small delay for the window manager to process
	time.Sleep(200 * time.Millisecond)

	// Return updated window info
	return e.getWindowInfo(handle)
}

// FindWindowHandle resolves a window handle from method+target (same as capture)
func (e *WindowsScreenshotEngine) FindWindowHandle(method, target string) (uintptr, error) {
	switch method {
	case "title":
		return e.findWindowByTitle(target)
	case "class":
		return e.findWindowByClassName(target)
	default:
		return 0, fmt.Errorf("unsupported lookup method: %s", method)
	}
}

// FindWindowByPIDPublic is a public wrapper around findWindowByPID
func (e *WindowsScreenshotEngine) FindWindowByPIDPublic(pid uint32) (uintptr, error) {
	return e.findWindowByPID(pid)
}

// ClickMouse performs a mouse click.
// When windowHandle != 0, uses PostMessage to send synthetic WM_*BUTTON messages
// directly to the window — does NOT move the physical cursor (stealth/non-interfering).
// When windowHandle == 0, falls back to SetCursorPos + SendInput (screen-absolute).
func (e *WindowsScreenshotEngine) ClickMouse(x, y int, button, clickType string, windowHandle uintptr) error {
	if windowHandle != 0 {
		// Qt ignores PostMessage-based synthetic clicks — its input system
		// requires real hardware events.  Detect Qt windows and use
		// targeted physical click (converts screenshot coords → screen
		// coords, briefly moves cursor, clicks, restores cursor).
		if e.isQtWindow(windowHandle) {
			return e.clickMouseTargeted(x, y, button, clickType, windowHandle)
		}
		return e.clickMouseStealth(x, y, button, clickType, windowHandle)
	}
	return e.clickMousePhysical(x, y, button, clickType)
}

// isQtWindow checks if a window belongs to a Qt framework
func (e *WindowsScreenshotEngine) isQtWindow(hwnd uintptr) bool {
	classBuf := make([]uint16, 256)
	getClassName.Call(hwnd, uintptr(unsafe.Pointer(&classBuf[0])), 256)
	className := strings.ToLower(syscall.UTF16ToString(classBuf))
	return strings.HasPrefix(className, "qt")
}

// clickMouseTargeted converts screenshot coords to screen coords, saves/restores
// cursor position, and uses SendInput for a real click. Works with all frameworks
// including Qt.  Cursor is restored immediately after the click.
func (e *WindowsScreenshotEngine) clickMouseTargeted(x, y int, button, clickType string, hwnd uintptr) error {
	// Get frame offset (screenshot includes title bar + borders)
	var windowRect RECT
	getWindowRect.Call(hwnd, uintptr(unsafe.Pointer(&windowRect)))

	clientOrigin := POINT{X: 0, Y: 0}
	clientToScreen.Call(hwnd, uintptr(unsafe.Pointer(&clientOrigin)))

	frameOffsetX := int(clientOrigin.X - windowRect.Left)
	frameOffsetY := int(clientOrigin.Y - windowRect.Top)

	// Screenshot coord → screen coord
	screenX := int(windowRect.Left) + x
	screenY := int(windowRect.Top) + y
	// But adjust because the client area starts after the frame
	_ = frameOffsetX
	_ = frameOffsetY

	// Save current cursor position
	var savedPos POINT
	getCursorPos.Call(uintptr(unsafe.Pointer(&savedPos)))

	// Bring window to foreground
	setForegroundWindow.Call(hwnd)
	time.Sleep(100 * time.Millisecond)

	// Move, click, restore
	setCursorPos.Call(uintptr(screenX), uintptr(screenY))
	time.Sleep(30 * time.Millisecond)

	var downFlag, upFlag uint32
	switch button {
	case "right":
		downFlag, upFlag = MOUSEEVENTF_RIGHTDOWN, MOUSEEVENTF_RIGHTUP
	case "middle":
		downFlag, upFlag = MOUSEEVENTF_MIDDLEDOWN, MOUSEEVENTF_MIDDLEUP
	default:
		downFlag, upFlag = MOUSEEVENTF_LEFTDOWN, MOUSEEVENTF_LEFTUP
	}

	clicks := 1
	if clickType == "double" {
		clicks = 2
	}

	for i := 0; i < clicks; i++ {
		inputDown := INPUT{Type: INPUT_MOUSE}
		inputDown.Mi.DwFlags = downFlag
		sendInput.Call(1, uintptr(unsafe.Pointer(&inputDown)), unsafe.Sizeof(inputDown))
		time.Sleep(20 * time.Millisecond)

		inputUp := INPUT{Type: INPUT_MOUSE}
		inputUp.Mi.DwFlags = upFlag
		sendInput.Call(1, uintptr(unsafe.Pointer(&inputUp)), unsafe.Sizeof(inputUp))
		if i < clicks-1 {
			time.Sleep(50 * time.Millisecond)
		}
	}

	// Restore cursor to original position
	time.Sleep(30 * time.Millisecond)
	setCursorPos.Call(uintptr(savedPos.X), uintptr(savedPos.Y))

	return nil
}

// clickMouseStealth sends WM_*BUTTON messages via PostMessage — no cursor movement.
// The x,y are "screenshot coordinates" (include window frame/title bar), so we
// convert to client-area coordinates, then find the deepest child window at that
// point so Qt and other frameworks receive the message on the correct HWND.
func (e *WindowsScreenshotEngine) clickMouseStealth(x, y int, button, clickType string, hwnd uintptr) error {
	// 1. Convert screenshot (frame-inclusive) coords to client coords.
	//    Screenshot coords include title bar + borders. Client (0,0) starts
	//    below the title bar. We use the difference between window rect and
	//    client rect origin to compute the offset.
	var windowRect, clientRect RECT
	getWindowRect.Call(hwnd, uintptr(unsafe.Pointer(&windowRect)))
	getClientRect.Call(hwnd, uintptr(unsafe.Pointer(&clientRect)))

	// Map client (0,0) to screen coords to find the offset
	clientOrigin := POINT{X: 0, Y: 0}
	clientToScreen.Call(hwnd, uintptr(unsafe.Pointer(&clientOrigin)))

	frameOffsetX := int(clientOrigin.X - windowRect.Left)
	frameOffsetY := int(clientOrigin.Y - windowRect.Top)

	clientX := x - frameOffsetX
	clientY := y - frameOffsetY

	// Clamp to client area
	if clientX < 0 {
		clientX = 0
	}
	if clientY < 0 {
		clientY = 0
	}

	// 2. Find the deepest child window at this client point.
	//    Qt and other frameworks may use child HWNDs for rendering.
	targetHwnd := hwnd
	pt := POINT{X: int32(clientX), Y: int32(clientY)}
	child, _, _ := childWindowFromPoint.Call(hwnd, uintptr(pt.X), uintptr(pt.Y))
	if child != 0 && child != hwnd {
		// Map coordinates from parent client to child client
		mapWindowPoints.Call(hwnd, child, uintptr(unsafe.Pointer(&pt)), 1)
		targetHwnd = child
		clientX = int(pt.X)
		clientY = int(pt.Y)
	}

	// 3. Bring parent to foreground so the app processes input
	setForegroundWindow.Call(hwnd)
	time.Sleep(50 * time.Millisecond)

	// 4. Post the mouse messages
	lParam := uintptr((clientY << 16) | (clientX & 0xFFFF))

	var downMsg, upMsg, dblMsg uintptr
	var wParamDown uintptr
	switch button {
	case "right":
		downMsg = WM_RBUTTONDOWN
		upMsg = WM_RBUTTONUP
		dblMsg = WM_RBUTTONDBLCLK
		wParamDown = MK_RBUTTON
	case "middle":
		downMsg = WM_MBUTTONDOWN
		upMsg = WM_MBUTTONUP
		dblMsg = WM_MBUTTONDBLCLK
		wParamDown = MK_MBUTTON
	default:
		downMsg = WM_LBUTTONDOWN
		upMsg = WM_LBUTTONUP
		dblMsg = WM_LBUTTONDBLCLK
		wParamDown = MK_LBUTTON
	}

	if clickType == "double" {
		postMessage.Call(targetHwnd, downMsg, wParamDown, lParam)
		time.Sleep(20 * time.Millisecond)
		postMessage.Call(targetHwnd, upMsg, 0, lParam)
		time.Sleep(20 * time.Millisecond)
		postMessage.Call(targetHwnd, dblMsg, wParamDown, lParam)
		time.Sleep(20 * time.Millisecond)
		postMessage.Call(targetHwnd, upMsg, 0, lParam)
	} else {
		postMessage.Call(targetHwnd, downMsg, wParamDown, lParam)
		time.Sleep(20 * time.Millisecond)
		postMessage.Call(targetHwnd, upMsg, 0, lParam)
	}

	return nil
}

// clickMousePhysical uses SetCursorPos + SendInput — moves the real cursor.
func (e *WindowsScreenshotEngine) clickMousePhysical(x, y int, button, clickType string) error {
	ret, _, _ := setCursorPos.Call(uintptr(x), uintptr(y))
	if ret == 0 {
		return fmt.Errorf("SetCursorPos failed")
	}
	time.Sleep(50 * time.Millisecond)

	var downFlag, upFlag uint32
	switch button {
	case "right":
		downFlag, upFlag = MOUSEEVENTF_RIGHTDOWN, MOUSEEVENTF_RIGHTUP
	case "middle":
		downFlag, upFlag = MOUSEEVENTF_MIDDLEDOWN, MOUSEEVENTF_MIDDLEUP
	default:
		downFlag, upFlag = MOUSEEVENTF_LEFTDOWN, MOUSEEVENTF_LEFTUP
	}

	clicks := 1
	if clickType == "double" {
		clicks = 2
	}

	for i := 0; i < clicks; i++ {
		inputDown := INPUT{Type: INPUT_MOUSE}
		inputDown.Mi.DwFlags = downFlag
		sendInput.Call(1, uintptr(unsafe.Pointer(&inputDown)), unsafe.Sizeof(inputDown))
		time.Sleep(20 * time.Millisecond)

		inputUp := INPUT{Type: INPUT_MOUSE}
		inputUp.Mi.DwFlags = upFlag
		sendInput.Call(1, uintptr(unsafe.Pointer(&inputUp)), unsafe.Sizeof(inputUp))

		if i < clicks-1 {
			time.Sleep(50 * time.Millisecond)
		}
	}

	return nil
}

// Ensure we implement the interface
var _ types.ScreenshotEngine = (*WindowsScreenshotEngine)(nil)

// Add constants for the advanced features
const (
	SW_SHOWNOACTIVATE = 4
	SW_MAXIMIZE       = 3
	SW_SHOWNORMAL     = 1

	// SetWindowPos flags
	SWP_NOZORDER     = 0x0004
	SWP_NOACTIVATE   = 0x0010
	SWP_NOMOVE       = 0x0002
	SWP_NOSIZE       = 0x0001
	HWND_TOP         = 0

	// SendInput / mouse constants (physical fallback)
	INPUT_MOUSE           = 0
	MOUSEEVENTF_MOVE      = 0x0001
	MOUSEEVENTF_LEFTDOWN  = 0x0002
	MOUSEEVENTF_LEFTUP    = 0x0004
	MOUSEEVENTF_RIGHTDOWN = 0x0008
	MOUSEEVENTF_RIGHTUP   = 0x0010
	MOUSEEVENTF_MIDDLEDOWN = 0x0020
	MOUSEEVENTF_MIDDLEUP  = 0x0040
	MOUSEEVENTF_ABSOLUTE  = 0x8000
	SM_CXSCREEN           = 0
	SM_CYSCREEN           = 1

	// WM_*BUTTON message constants (stealth PostMessage click)
	WM_LBUTTONDOWN   = 0x0201
	WM_LBUTTONUP     = 0x0202
	WM_LBUTTONDBLCLK = 0x0203
	WM_RBUTTONDOWN   = 0x0204
	WM_RBUTTONUP     = 0x0205
	WM_RBUTTONDBLCLK = 0x0206
	WM_MBUTTONDOWN   = 0x0207
	WM_MBUTTONUP     = 0x0208
	WM_MBUTTONDBLCLK = 0x0209
	MK_LBUTTON       = 0x0001
	MK_RBUTTON       = 0x0002
	MK_MBUTTON       = 0x0010
)

// POINT structure for GetCursorPos
type POINT struct {
	X, Y int32
}

// MOUSEINPUT for SendInput
type MOUSEINPUT struct {
	Dx          int32
	Dy          int32
	MouseData   uint32
	DwFlags     uint32
	Time        uint32
	DwExtraInfo uintptr
}

// INPUT structure for SendInput (mouse variant)
type INPUT struct {
	Type uint32
	_    [4]byte // padding on 64-bit
	Mi   MOUSEINPUT
}

func init() {
	// Lock OS thread for Windows API calls
	runtime.LockOSThread()
}
