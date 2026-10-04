//go:build windows

package main

import (
    _ "embed"
    "fmt"
    "os"
    "path/filepath"
    "sync"

    "golang.org/x/sys/windows"
)

//go:embed wintun.dll
var wintunDLL []byte

var (
    wintunLoadOnce sync.Once
    wintunLoadErr  error
)

func ensureWintunLoaded() error {
    wintunLoadOnce.Do(func() {
        if exePath, err := os.Executable(); err == nil {
            exeDir := filepath.Dir(exePath)
            candidate := filepath.Join(exeDir, "wintun.dll")
            if _, err := os.Stat(candidate); err == nil {
                if err := loadWintunDLL(candidate); err == nil {
                    return
                }
            }
        }

        tempDir := filepath.Join(os.TempDir(), "n2n-go-client")
        if err := os.MkdirAll(tempDir, 0755); err != nil {
            wintunLoadErr = fmt.Errorf("create temp dir: %w", err)
            return
        }
        dllPath := filepath.Join(tempDir, "wintun.dll")

        needWrite := true
        if existing, err := os.ReadFile(dllPath); err == nil {
            if len(existing) == len(wintunDLL) {
                needWrite = false
            }
        }
        if needWrite {
            if err := os.WriteFile(dllPath, wintunDLL, 0644); err != nil {
                wintunLoadErr = fmt.Errorf("write wintun.dll: %w", err)
                return
            }
        }

        if err := loadWintunDLL(dllPath); err != nil {
            wintunLoadErr = err
        }
    })
    return wintunLoadErr
}

func loadWintunDLL(path string) error {
    if _, err := windows.LoadLibraryEx(
        path,
        0,
        windows.LOAD_LIBRARY_SEARCH_DLL_LOAD_DIR|
            windows.LOAD_LIBRARY_SEARCH_DEFAULT_DIRS,
    ); err == nil {
        return nil
    }
    if _, err := windows.LoadLibrary(path); err != nil {
        return fmt.Errorf("LoadLibrary(%s): %w", path, err)
    }
    return nil
}
