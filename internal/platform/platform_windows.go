//go:build windows

package platform

import (
	"os"
	"os/exec"
	"strings"
	"syscall"
	"unsafe"

	"golang.org/x/sys/windows"
	"golang.org/x/sys/windows/registry"
)

var user32 = windows.NewLazySystemDLL("user32.dll")

// InfoBox 弹信息提示框。
func InfoBox(title, msg string) {
	msgBox(title, msg, 0x40 /* MB_ICONINFORMATION */)
}

// Notify 弹信息提示框。
func Notify(title, msg string) { InfoBox(title, msg) }

// AskYesNo 弹"是/否"框，true=是。
func AskYesNo(title, msg string) bool {
	return msgBox(title, msg, 0x24 /* MB_YESNO|MB_ICONQUESTION */) == 6 /* IDYES */
}

func msgBox(title, msg string, flags uintptr) uintptr {
	proc := user32.NewProc("MessageBoxW")
	caption, _ := syscall.UTF16PtrFromString(title)
	text, _ := syscall.UTF16PtrFromString(msg)
	// MB_TOPMOST(0x40000) + MB_SETFOREGROUND(0x10000)：置顶并抢前台。
	// ⚠️ 不要加 MB_DEFAULT_DESKTOP_ONLY(0x20000)：那是「在缺省桌面窗口站上代建」的服务端语义，
	// 不是定位开关 —— 实测弹窗会落到屏幕右下角、半屏在屏外，且在非缺省桌面下（锁屏 / RDP / UAC）
	// 永不返回，调用它的那个 goroutine 会被永久阻塞（退出无反应 / 启动提示挡住托盘创建）。
	res, _, _ := proc.Call(0, uintptr(unsafe.Pointer(text)), uintptr(unsafe.Pointer(caption)), flags|0x40000|0x10000)
	return res
}

// ---------------------------------------------------------------------------
// 浏览器
// ---------------------------------------------------------------------------

// OpenURL 用系统默认方式打开 URL。
func OpenURL(url string) error {
	cmd := exec.Command("rundll32", "url.dll,FileProtocolHandler", url)
	cmd.SysProcAttr = &syscall.SysProcAttr{HideWindow: true}
	return cmd.Start()
}

// ---------------------------------------------------------------------------
// 开机自启（注册表 Run 键）
// ---------------------------------------------------------------------------

const autostartKey = `Software\Microsoft\Windows\CurrentVersion\Run`
const autostartValue = "wild-work"

// SetAutostart 设置/取消当前 exe 的开机自启。
func SetAutostart(on bool) error {
	k, err := registry.OpenKey(registry.CURRENT_USER, autostartKey, registry.SET_VALUE)
	if err != nil {
		return err
	}
	defer k.Close()
	if !on {
		if err := k.DeleteValue(autostartValue); err != nil && err != registry.ErrNotExist {
			return err
		}
		return nil
	}
	exe, err := os.Executable()
	if err != nil {
		return err
	}
	return k.SetStringValue(autostartValue, `"`+exe+`" --autostart`)
}

// AutostartEnabled 查询当前 exe 是否已设置开机自启。
func AutostartEnabled() bool {
	k, err := registry.OpenKey(registry.CURRENT_USER, autostartKey, registry.QUERY_VALUE)
	if err != nil {
		return false
	}
	defer k.Close()
	v, _, err := k.GetStringValue(autostartValue)
	if err != nil {
		return false
	}
	if exe, err := os.Executable(); err == nil {
		return strings.HasPrefix(v, `"`+exe+`"`)
	}
	return v != ""
}

// OpenWithTextEditor 用记事本打开文件。
func OpenWithTextEditor(path string) error {
	cmd := exec.Command("notepad.exe", path)
	return cmd.Start()
}
