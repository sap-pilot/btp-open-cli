package cmd

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"github.com/spf13/cobra"
)

const upgradeRepoAPI = "https://api.github.com/repos/sap-pilot/btp-open-cli/releases/latest"

type ghRelease struct {
	TagName string    `json:"tag_name"`
	Assets  []ghAsset `json:"assets"`
}

type ghAsset struct {
	Name               string `json:"name"`
	BrowserDownloadURL string `json:"browser_download_url"`
}

var upgradeCmd = &cobra.Command{
	Use:   "upgrade",
	Short: "Upgrade bo to the latest release from GitHub",
	RunE: func(cmd *cobra.Command, args []string) error {
		skipConfirm, _ := cmd.Flags().GetBool("yes")

		release, err := fetchLatestRelease()
		if err != nil {
			return fmt.Errorf("checking latest release: %w", err)
		}

		latestVersion := strings.TrimPrefix(release.TagName, "v")
		localVersion := strings.TrimPrefix(Version, "v")

		fmt.Fprintf(os.Stdout, "Local version:  %s\n", localVersion)
		fmt.Fprintf(os.Stdout, "Latest version: %s\n", latestVersion)

		if localVersion == latestVersion {
			fmt.Fprintln(os.Stdout, "Already up to date.")
			return nil
		}

		assetName := upgradeAssetName()
		var asset *ghAsset
		for i := range release.Assets {
			if release.Assets[i].Name == assetName {
				asset = &release.Assets[i]
				break
			}
		}
		if asset == nil {
			return fmt.Errorf("no release asset found for %s/%s (expected %q)", runtime.GOOS, runtime.GOARCH, assetName)
		}

		if !skipConfirm {
			fmt.Fprintf(os.Stderr, "Upgrade to version %s? [y/N] ", latestVersion)
			scanner := bufio.NewScanner(os.Stdin)
			if !scanner.Scan() || strings.ToLower(strings.TrimSpace(scanner.Text())) != "y" {
				fmt.Fprintln(os.Stdout, "Aborted.")
				return nil
			}
		}

		exePath, err := os.Executable()
		if err != nil {
			return fmt.Errorf("resolving executable path: %w", err)
		}
		exePath, err = filepath.EvalSymlinks(exePath)
		if err != nil {
			return fmt.Errorf("resolving symlinks: %w", err)
		}
		exeDir := filepath.Dir(exePath)

		fmt.Fprintf(os.Stdout, "Downloading %s...\n", asset.Name)

		if runtime.GOOS == "windows" {
			return upgradeWindows(asset, exePath, exeDir, localVersion)
		}
		return upgradeUnix(asset, exePath, exeDir)
	},
}

func fetchLatestRelease() (*ghRelease, error) {
	client := &http.Client{Timeout: 30 * time.Second}
	req, err := http.NewRequest("GET", upgradeRepoAPI, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Accept", "application/vnd.github+json")

	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("GitHub API returned HTTP %d: %s", resp.StatusCode, body)
	}

	var release ghRelease
	if err := json.Unmarshal(body, &release); err != nil {
		return nil, fmt.Errorf("parsing release response: %w", err)
	}
	return &release, nil
}

// upgradeAssetName returns the expected GitHub release asset filename for the
// current OS and architecture, e.g. "bo-linux-amd64" or "bo-windows-amd64.exe".
func upgradeAssetName() string {
	name := fmt.Sprintf("bo-%s-%s", runtime.GOOS, runtime.GOARCH)
	if runtime.GOOS == "windows" {
		name += ".exe"
	}
	return name
}

// upgradeUnix downloads the new binary to a temp file in the same directory,
// sets executable permissions, then atomically replaces the running binary.
func upgradeUnix(asset *ghAsset, exePath, exeDir string) error {
	tmpPath := filepath.Join(exeDir, ".bo-upgrade-tmp")
	if err := downloadBinary(asset.BrowserDownloadURL, tmpPath); err != nil {
		os.Remove(tmpPath)
		return fmt.Errorf("downloading: %w", err)
	}
	if err := os.Rename(tmpPath, exePath); err != nil {
		os.Remove(tmpPath)
		return fmt.Errorf("replacing executable: %w", err)
	}
	fmt.Fprintln(os.Stdout, "Upgrade complete.")
	return nil
}

// upgradeWindows renames the existing bo.exe to bo-{version}.exe (since Windows
// cannot overwrite a running executable), then downloads the new release as bo.exe.
func upgradeWindows(asset *ghAsset, exePath, exeDir, localVersion string) error {
	backupName := fmt.Sprintf("bo-%s.exe", localVersion)
	backupPath := filepath.Join(exeDir, backupName)

	if err := os.Rename(exePath, backupPath); err != nil {
		return fmt.Errorf("renaming current executable to %s: %w", backupName, err)
	}
	fmt.Fprintf(os.Stdout, "Renamed current executable to %s\n", backupName)

	newPath := filepath.Join(exeDir, "bo.exe")
	if err := downloadBinary(asset.BrowserDownloadURL, newPath); err != nil {
		// Best-effort rollback.
		os.Rename(backupPath, exePath)
		return fmt.Errorf("downloading: %w", err)
	}
	fmt.Fprintln(os.Stdout, "Upgrade complete.")
	return nil
}

func downloadBinary(url, destPath string) error {
	client := &http.Client{Timeout: 5 * time.Minute}
	resp, err := client.Get(url) //nolint:noctx
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("download returned HTTP %d", resp.StatusCode)
	}

	f, err := os.OpenFile(destPath, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0755)
	if err != nil {
		return err
	}
	_, copyErr := io.Copy(f, resp.Body)
	closeErr := f.Close()
	if copyErr != nil {
		return copyErr
	}
	return closeErr
}

func init() {
	rootCmd.AddCommand(upgradeCmd)
	upgradeCmd.Flags().BoolP("yes", "y", false, "Skip confirmation prompt")
}
