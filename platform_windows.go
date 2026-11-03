//go:build windows

package main

import (
	"errors"
	"fmt"
	"os"
	"strings"
	"syscall"
	"unsafe"

	"golang.org/x/sys/windows"
	"golang.org/x/sys/windows/registry"
)

const (
	notepadKey    = `Software\Microsoft\Windows NT\CurrentVersion\Image File Execution Options\notepad.exe`
	notepadValue  = "Debugger"
	appPathsKey   = `Software\Microsoft\Windows\CurrentVersion\App Paths\notepad.exe`
	shellMenuKey  = `Software\Classes\*\shell\MyNotepadGo`
	shellMenuText = "My Notepad Go 打开"

	hklmNotepadKey  = `SOFTWARE\Microsoft\Windows NT\CurrentVersion\Image File Execution Options\notepad.exe`
	hklmAppPathsKey = `SOFTWARE\Microsoft\Windows\CurrentVersion\App Paths\notepad.exe`

	debuggerArgs = " /z"
)

type regScope struct {
	hive   registry.Key
	access uint32
}

var (
	scopeUser = regScope{registry.CURRENT_USER, registry.ALL_ACCESS}
	scopeAll  = regScope{registry.LOCAL_MACHINE, registry.ALL_ACCESS | registry.WOW64_64KEY}
)

func (s regScope) reader() regScope {
	return regScope{s.hive, registry.QUERY_VALUE | s.access&registry.WOW64_64KEY}
}

type hookKeys struct {
	scope    regScope
	ifeo     string
	appPaths string
}

var hookScopes = []hookKeys{
	{scopeAll, hklmNotepadKey, hklmAppPathsKey},
	{scopeUser, notepadKey, appPathsKey},
}

func exePath() string {
	exe, err := os.Executable()
	if err != nil {
		return ""
	}
	return exe
}

func debuggerValue() string {
	return `"` + exePath() + `"` + debuggerArgs
}

func notepadHooked() bool {
	for _, h := range hookScopes {
		if !h.hooked() {
			return false
		}
	}
	return true
}

func (h hookKeys) hooked() bool {
	exe := exePath()
	if exe == "" {
		return false
	}
	s := h.scope.reader()
	dbg, err := s.readString(h.ifeo, notepadValue)
	if err != nil || strings.TrimSpace(dbg) != debuggerValue() {
		return false
	}
	target, err := s.readString(h.appPaths, "")
	return err == nil && target == exe
}

func (h hookKeys) conflict() error {
	s := h.scope.reader()
	if v, err := s.readString(h.ifeo, notepadValue); err == nil && strings.TrimSpace(v) != debuggerValue() && !debuggerStale(v) {
		return fmt.Errorf("注册表项 %s 的 %s 值已被其他程序占用", h.ifeo, notepadValue)
	}
	if v, err := s.readString(h.appPaths, ""); err == nil && v != "" && v != exePath() && !pathStale(v) {
		return fmt.Errorf("注册表项 %s 已被其他程序占用", h.appPaths)
	}
	return nil
}

func pathStale(p string) bool {
	if p == "" {
		return false
	}
	_, err := os.Stat(p)
	return err != nil && os.IsNotExist(err)
}

func debuggerStale(v string) bool {
	v = strings.TrimSpace(v)
	if quoted, ok := strings.CutPrefix(v, `"`); ok {
		if head, _, ok := strings.Cut(quoted, `"`); ok {
			return pathStale(head)
		}
		return pathStale(quoted)
	}
	v, _, _ = strings.Cut(v, " ")
	return pathStale(v)
}

func (h hookKeys) write(on bool) error {
	if on {
		if err := h.scope.setString(h.ifeo, notepadValue, debuggerValue()); err != nil {
			return err
		}
		return h.scope.setString(h.appPaths, "", exePath())
	}
	dbg, err := h.scope.readString(h.ifeo, notepadValue)
	if err != nil {
		if errors.Is(err, registry.ErrNotExist) {
			return h.clearAppPaths()
		}
		return err
	}
	if strings.TrimSpace(dbg) != debuggerValue() && !debuggerStale(dbg) {
		return fmt.Errorf("注册表项 %s 的 %s 值不属于本程序，未做修改", h.ifeo, notepadValue)
	}
	if err := h.scope.deleteValue(h.ifeo, notepadValue); err != nil {
		return err
	}
	return h.clearAppPaths()
}

func (h hookKeys) clearAppPaths() error {
	target, err := h.scope.readString(h.appPaths, "")
	if err != nil {
		return nil
	}
	if target == exePath() || pathStale(target) {
		return h.scope.deleteTree(h.appPaths)
	}
	return nil
}

func setNotepadHook(on bool) error {
	if on {
		for _, h := range hookScopes {
			if err := h.conflict(); err != nil {
				return fmt.Errorf("无法劫持系统记事本：%v", err)
			}
		}
	}
	err := writeHook(on)
	if err == nil {
		return nil
	}
	if errors.Is(err, windows.ERROR_ACCESS_DENIED) {
		return fmt.Errorf("需要管理员权限，请以管理员身份运行本程序")
	}
	return err
}

func writeHook(on bool) error {
	var done []hookKeys
	for _, h := range hookScopes {
		if err := h.write(on); err != nil {
			if on {
				for _, d := range done {
					_ = d.write(false)
				}
			}
			return err
		}
		done = append(done, h)
	}
	return nil
}

func shellMenuHooked() bool {
	v, ok := scopeUser.getString(shellMenuKey+`\command`, "")
	return ok && v == shellMenuCommand()
}

func shellMenuCommand() string {
	return "\"" + exePath() + "\" \"%1\""
}

func setShellMenu(on bool) error {
	if on {
		exe := exePath()
		if exe == "" {
			return fmt.Errorf("无法获取程序路径")
		}
		if err := scopeUser.setString(shellMenuKey, "", shellMenuText); err != nil {
			return err
		}
		if err := scopeUser.setString(shellMenuKey, "Icon", exe); err != nil {
			return err
		}
		return scopeUser.setString(shellMenuKey+`\command`, "", shellMenuCommand())
	}
	if !shellMenuHooked() {
		return nil
	}
	return scopeUser.deleteTree(shellMenuKey)
}

func (s regScope) readString(path, name string) (string, error) {
	key, err := registry.OpenKey(s.hive, path, s.access)
	if err != nil {
		return "", err
	}
	defer func(k registry.Key) { _ = k.Close() }(key)
	v, _, err := key.GetStringValue(name)
	return v, err
}

func (s regScope) getString(path, name string) (string, bool) {
	v, err := s.readString(path, name)
	if err != nil {
		return "", false
	}
	return v, true
}

func (s regScope) setString(path, name, value string) error {
	key, _, err := registry.CreateKey(s.hive, path, s.access)
	if err != nil {
		return err
	}
	defer func(k registry.Key) { _ = k.Close() }(key)
	return key.SetStringValue(name, value)
}

func (s regScope) deleteValue(path, name string) error {
	key, err := registry.OpenKey(s.hive, path, s.access)
	if err != nil {
		if errors.Is(err, registry.ErrNotExist) {
			return nil
		}
		return err
	}
	defer func(k registry.Key) { _ = k.Close() }(key)
	if err := key.DeleteValue(name); err != nil && !errors.Is(err, registry.ErrNotExist) {
		return err
	}
	return nil
}

func (s regScope) deleteTree(path string) error {
	key, err := registry.OpenKey(s.hive, path, s.access)
	if err != nil {
		if errors.Is(err, registry.ErrNotExist) {
			return nil
		}
		return err
	}
	subs, err := key.ReadSubKeyNames(-1)
	_ = key.Close()
	if err != nil {
		return err
	}
	for _, sub := range subs {
		if err := s.deleteTree(path + `\` + sub); err != nil {
			return err
		}
	}
	return deleteKeyEx(s.hive, path, s.access)
}

var (
	advapi32           = syscall.NewLazyDLL("advapi32.dll")
	procRegDeleteKeyEx = advapi32.NewProc("RegDeleteKeyExW")
)

func deleteKeyEx(hive registry.Key, path string, access uint32) error {
	p, err := syscall.UTF16PtrFromString(path)
	if err != nil {
		return err
	}
	r1, _, errno := procRegDeleteKeyEx.Call(uintptr(hive), uintptr(unsafe.Pointer(p)), uintptr(access), 0)
	if r1 != 0 {
		if errno == windows.ERROR_FILE_NOT_FOUND {
			return nil
		}
		return fmt.Errorf("无法删除注册表键 %s（%v）", path, errno)
	}
	return nil
}
