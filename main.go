//go:build windows

package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"
	"unsafe"
)

const (
	releasesURL         = "https://api.github.com/repos/anomalyco/opencode-beta/releases"
	installerDownloadURL = "https://opencode.ai/download/beta/windows-x64-nsis"
	windowsAssetName    = "opencode-desktop-windows-x64.exe"
	installDir          = `C:\Users\giuli\AppData\Local\Programs\OpenCode Beta`
	renamedExe          = "OpenCode Beta.exe"
	cliExeName          = "opencode-cli.exe"
	fallbackCLIPath     = `C:\Users\giuli\Downloads\opencode-desktop-windows-x64\opencode-cli.exe`
	stateFileName       = "latest_release_id.txt"
)

const (
	mbOK            = 0x00000000
	mbIconError     = 0x00000010
	mbSetForeground = 0x00010000
)

var (
	user32          = syscall.NewLazyDLL("user32.dll")
	procMessageBoxW = user32.NewProc("MessageBoxW")
	errFound         = errors.New("found")
)

type githubRelease struct {
	ID      int64         `json:"id"`
	TagName string        `json:"tag_name"`
	Assets  []githubAsset `json:"assets"`
}

type githubAsset struct {
	Name               string `json:"name"`
	BrowserDownloadURL string `json:"browser_download_url"`
}

type releaseForWindows struct {
	ID      int64
	TagName string
	Asset   githubAsset
}

func main() {
	logf("launcher started")

	latest, err := fetchLatestWindowsRelease()
	if err == nil {
		statePath, pathErr := getStateFilePath()
		if pathErr == nil {
			logf("state file path: %s", statePath)
			storedID, _ := readStateID(statePath)
			latestID := strconv.FormatInt(latest.ID, 10)
			logf("latest release id: %s", latestID)
			shouldPersistLatestID := false

			if storedID == "" {
				logf("first launch detected: forcing update")
				if updateErr := updateInstall(latest); updateErr != nil {
					logf("forced first-launch update failed: %v", updateErr)
					showError("OpenCode Beta Updater", "First-launch update failed:\n"+updateErr.Error()+"\n\nLaunching current app.")
				} else {
					logf("forced first-launch update completed")
					shouldPersistLatestID = true
				}
			} else if storedID != latestID {
				logf("update found (stored=%s, latest=%s): auto-updating", storedID, latestID)
				if updateErr := updateInstall(latest); updateErr != nil {
					logf("update failed: %v", updateErr)
					showError("OpenCode Beta Updater", "Update failed:\n"+updateErr.Error()+"\n\nLaunching current app.")
				} else {
					logf("update completed")
					shouldPersistLatestID = true
				}
			} else {
				logf("no update found (stored=%s)", storedID)
				shouldPersistLatestID = true
			}

			if shouldPersistLatestID {
				if err := writeStateID(statePath, latestID); err != nil {
					logf("could not write state file: %v", err)
				} else {
					logf("state file updated")
				}
			} else {
				logf("state file not updated because installation did not complete")
			}
		} else {
			logf("could not resolve state path: %v", pathErr)
		}
	} else {
		logf("could not fetch releases: %v", err)
	}

	if err := ensureCLIInInstallDir(); err != nil {
		logf("warning: could not ensure %s in install dir: %v", cliExeName, err)
	} else {
		logf("verified %s in install dir", cliExeName)
	}

	logf("launching OpenCode")
	if err := runOpenCode(); err != nil {
		logf("launch failed: %v", err)
		showError("OpenCode Beta Updater", "Could not launch OpenCode Beta:\n"+err.Error())
		os.Exit(1)
	}

	logf("launcher finished")
}

func fetchLatestWindowsRelease() (releaseForWindows, error) {
	req, err := http.NewRequest(http.MethodGet, releasesURL, nil)
	if err != nil {
		return releaseForWindows{}, err
	}
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("X-GitHub-Api-Version", "2022-11-28")
	req.Header.Set("User-Agent", "opencode-app-launcher")

	client := &http.Client{Timeout: 30 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return releaseForWindows{}, err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return releaseForWindows{}, fmt.Errorf("github api returned %s", resp.Status)
	}

	var releases []githubRelease
	if err := json.NewDecoder(resp.Body).Decode(&releases); err != nil {
		return releaseForWindows{}, err
	}

	for _, rel := range releases {
		for _, asset := range rel.Assets {
			if strings.EqualFold(asset.Name, windowsAssetName) {
				return releaseForWindows{
					ID:      rel.ID,
					TagName: rel.TagName,
					Asset:   asset,
				}, nil
			}
		}
	}

	return releaseForWindows{}, fmt.Errorf("asset %q not found in releases", windowsAssetName)
}

func updateInstall(rel releaseForWindows) error {
	logf("starting update for tag %s (id %d)", rel.TagName, rel.ID)

	tempDir, err := os.MkdirTemp("", "opencode-beta-update-*")
	if err != nil {
		return err
	}
	defer os.RemoveAll(tempDir)

	installerPath := filepath.Join(tempDir, windowsAssetName)
	logf("downloading installer: %s", installerDownloadURL)
	if err := downloadFile(installerDownloadURL, installerPath); err != nil {
		return fmt.Errorf("download installer: %w", err)
	}
	logf("installer downloaded: %s", installerPath)

	if err := runInstallerSilently(installerPath); err != nil {
		return err
	}
	logf("silent installer completed")

	if err := ensureRenamedExecutable(installDir); err != nil {
		return err
	}
	logf("ensured executable name: %s", renamedExe)

	if err := ensureCLIInInstallDir(); err != nil {
		return err
	}
	logf("ensured CLI location: %s", filepath.Join(installDir, cliExeName))

	return nil
}

func runInstallerSilently(installerPath string) error {
	logf("running installer silently to %s", installDir)

	cmd := exec.Command(installerPath, "/S", "/D="+installDir)
	out, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("silent installer failed: %w\n%s", err, strings.TrimSpace(string(out)))
	}

	if !exists(installDir) {
		return fmt.Errorf("installer finished but install dir was not created: %s", installDir)
	}

	return nil
}

func ensureCLIInInstallDir() error {
	target := filepath.Join(installDir, cliExeName)
	if exists(target) {
		return nil
	}

	srcInInstall, err := findFileByName(installDir, cliExeName)
	if err == nil {
		info, statErr := os.Stat(srcInInstall)
		if statErr != nil {
			return statErr
		}
		if err := copyFile(srcInInstall, target, info.Mode()); err != nil {
			return fmt.Errorf("copy cli from install payload: %w", err)
		}
		return nil
	}

	if exists(fallbackCLIPath) {
		info, statErr := os.Stat(fallbackCLIPath)
		if statErr != nil {
			return statErr
		}
		if err := copyFile(fallbackCLIPath, target, info.Mode()); err != nil {
			return fmt.Errorf("copy fallback cli: %w", err)
		}
		return nil
	}

	return fmt.Errorf("%s not found after install and no fallback cli at %s", cliExeName, fallbackCLIPath)
}

func downloadFile(url, outputPath string) error {
	req, err := http.NewRequest(http.MethodGet, url, nil)
	if err != nil {
		return err
	}
	req.Header.Set("User-Agent", "opencode-app-launcher")

	client := &http.Client{Timeout: 15 * time.Minute}
	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return fmt.Errorf("download failed with %s", resp.Status)
	}

	out, err := os.Create(outputPath)
	if err != nil {
		return err
	}
	defer out.Close()

	_, err = io.Copy(out, resp.Body)
	return err
}

func ensureRenamedExecutable(root string) error {
	target := filepath.Join(root, renamedExe)
	if exists(target) {
		return nil
	}

	for _, name := range []string{"opencode-desktop.exe", "opencode.exe", "OpenCode.exe"} {
		src, err := findFileByName(root, name)
		if err == nil {
			if err := os.Rename(src, target); err != nil {
				return fmt.Errorf("rename executable: %w", err)
			}
			return nil
		}
	}

	return fmt.Errorf("could not find executable to rename to %q", renamedExe)
}

func runOpenCode() error {
	for _, exe := range []string{
		filepath.Join(installDir, renamedExe),
		filepath.Join(installDir, "opencode-desktop.exe"),
		filepath.Join(installDir, "opencode.exe"),
	} {
		if exists(exe) {
			logf("starting executable: %s", exe)
			cmd := exec.Command(exe)
			cmd.Dir = installDir
			if err := cmd.Start(); err != nil {
				return err
			}
			logf("OpenCode started successfully (pid=%d). Exiting launcher.", cmd.Process.Pid)
			return nil
		}
	}

	if _, err := exec.LookPath("opencode"); err == nil {
		logf("starting fallback command: opencode")
		cmd := exec.Command("opencode")
		if err := cmd.Start(); err != nil {
			return err
		}
		logf("OpenCode started successfully (pid=%d). Exiting launcher.", cmd.Process.Pid)
		return nil
	}

	return fmt.Errorf("no executable found in %s", installDir)
}

func getStateFilePath() (string, error) {
	return filepath.Join(installDir, stateFileName), nil
}

func readStateID(path string) (string, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(string(b)), nil
}

func writeStateID(path, id string) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	return os.WriteFile(path, []byte(id), 0o644)
}

func messageBox(title, text string, flags uintptr) (int, error) {
	titlePtr, err := syscall.UTF16PtrFromString(title)
	if err != nil {
		return 0, err
	}
	textPtr, err := syscall.UTF16PtrFromString(text)
	if err != nil {
		return 0, err
	}

	ret, _, callErr := procMessageBoxW.Call(
		0,
		uintptr(unsafe.Pointer(textPtr)),
		uintptr(unsafe.Pointer(titlePtr)),
		flags,
	)
	if ret == 0 && callErr != syscall.Errno(0) {
		return 0, callErr
	}

	return int(ret), nil
}

func showError(title, message string) {
	_, _ = messageBox(title, message, mbOK|mbIconError|mbSetForeground)
}

func findFileByName(root, fileName string) (string, error) {
	var found string
	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			return nil
		}
		if strings.EqualFold(d.Name(), fileName) {
			found = path
			return errFound
		}
		return nil
	})

	if err != nil && !errors.Is(err, errFound) {
		return "", err
	}
	if found == "" {
		return "", os.ErrNotExist
	}
	return found, nil
}

func copyFile(src, dst string, mode fs.FileMode) error {
	if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
		return err
	}

	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()

	out, err := os.OpenFile(dst, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, mode)
	if err != nil {
		return err
	}
	defer out.Close()

	if _, err := io.Copy(out, in); err != nil {
		return err
	}

	return out.Close()
}

func exists(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}

func logf(format string, args ...any) {
	fmt.Printf("[launcher] "+format+"\n", args...)
}
