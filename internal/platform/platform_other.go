//go:build !windows && !darwin

package platform

import (
	"os"
	"os/exec"
)

// InfoBox 其他平台：输出到 stderr。
func InfoBox(title, msg string) { Notify(title, msg) }

// Notify 其他平台：输出到 stderr。
func Notify(title, msg string) {
	_, _ = os.Stderr.WriteString(title + ": " + msg + "\n")
}

// AskYesNo 其他平台：默认否。
func AskYesNo(title, msg string) bool { return false }

// OpenURL 其他平台：xdg-open。
func OpenURL(url string) error { return exec.Command("xdg-open", url).Start() }

// SetAutostart 其他平台：未实现。
func SetAutostart(on bool) error { return nil }

// AutostartEnabled 其他平台：false。
func AutostartEnabled() bool { return false }

// OpenWithTextEditor 其他平台：xdg-open。
func OpenWithTextEditor(path string) error { return exec.Command("xdg-open", path).Start() }
