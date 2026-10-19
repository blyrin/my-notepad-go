//go:build !windows

package main

func notepadHooked() bool { return false }

func setNotepadHook(bool) error { return nil }

func shellMenuHooked() bool { return false }

func setShellMenu(bool) error { return nil }
