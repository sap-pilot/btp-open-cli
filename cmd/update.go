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

const (
	updateRepoAPI  = "https://api.github.com/repos/sap-pilot/btp-open-cli/releases/latest"
	updateRepoBase = "https://github.com/sap-pilot/btp-open-cli/releases/download"
)

type ghRelease struct {
	TagName string    `json:"tag_name"`
	Assets  []ghAsset `json:"assets"`
}

type ghAsset struct {
	Name               string `json:"name"`
	BrowserDownloadURL string `json:"browser_download_url"`
}

var updateCmd = &cobra.Command{
	Use:   "update [release]",
	Short: "Update bo to a specific or the latest release from GitHub",
	Long: `Update the bo binary in place from a GitHub release.

If [release] is omitted, the latest published release is fetched from the
GitHub API and compared to the running version; if already up to date the
command exits without downloading anything.

If [release] is specified (e.g. "v0.9"), the binary is downloaded directly
from https://github.com/sap-pilot/btp-open-cli/releases/download/{release}/
without calling the GitHub API or checking the current version.`,
	Args: cobra.MaximumNArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		skipConfirm, _ := cmd.Flags().GetBool("yes")

		exePath, err := os.Executable()
		if err != nil {
			return fmt.Errorf("resolving executable path: %w", err)
		}
		exePath, err = filepath.EvalSymlinks(exePath)
		if err != nil {
			return fmt.Errorf("resolving symlinks: %w", err)
		}
		exeDir := filepath.Dir(exePath)
		localVersion := strings.TrimPrefix(Version, "v")
		assetName := updateAssetName()

		if len(args) == 1 {
			// Specific release requested — skip version check and API call.
			release := args[0]
			downloadURL := fmt.Sprintf("%s/%s/%s", updateRepoBase, release, assetName)

			if !skipConfirm {
				fmt.Fprintf(os.Stderr, "Update to release %s? [y/N] ", release)
				scanner := bufio.NewScanner(os.Stdin)
				if !scanner.Scan() || strings.ToLower(strings.TrimSpace(scanner.Text())) != "y" {
					fmt.Fprintln(os.Stdout, "Aborted.")
					return nil
				}
			}

			fmt.Fprintf(os.Stdout, "Downloading %s from release %s...\n", assetName, release)
			if runtime.GOOS == "windows" {
				return updateWindows(downloadURL, exePath, exeDir, localVersion)
			}
			return updateUnix(downloadURL, exePath, exeDir)
		}

		// No release specified — fetch latest from GitHub API.
		latest, err := fetchLatestRelease()
		if err != nil {
			return fmt.Errorf("checking latest release: %w", err)
		}

		latestVersion := strings.TrimPrefix(latest.TagName, "v")
		fmt.Fprintf(os.Stdout, "Local version:  %s\n", localVersion)
		fmt.Fprintf(os.Stdout, "Latest version: %s\n", latestVersion)

		if localVersion == latestVersion {
			fmt.Fprintln(os.Stdout, "Already up to date.")
			return nil
		}

		var asset *ghAsset
		for i := range latest.Assets {
			if latest.Assets[i].Name == assetName {
				asset = &latest.Assets[i]
				break
			}
		}
		if asset == nil {
			return fmt.Errorf("no release asset found for %s/%s (expected %q)", runtime.GOOS, runtime.GOARCH, assetName)
		}

		if !skipConfirm {
			fmt.Fprintf(os.Stderr, "Update to version %s? [y/N] ", latestVersion)
			scanner := bufio.NewScanner(os.Stdin)
			if !scanner.Scan() || strings.ToLower(strings.TrimSpace(scanner.Text())) != "y" {
				fmt.Fprintln(os.Stdout, "Aborted.")
				return nil
			}
		}

		fmt.Fprintf(os.Stdout, "Downloading %s...\n", asset.Name)
		if runtime.GOOS == "windows" {
			return updateWindows(asset.BrowserDownloadURL, exePath, exeDir, localVersion)
		}
		return updateUnix(asset.BrowserDownloadURL, exePath, exeDir)
	},
}

func fetchLatestRelease() (*ghRelease, error) {
	client := &http.Client{Timeout: 30 * time.Second}
	req, err := http.NewRequest("GET", updateRepoAPI, nil)
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

// updateAssetName returns the expected GitHub release asset filename for the
// current OS and architecture, e.g. "bo-linux-amd64" or "bo-windows-amd64.exe".
func updateAssetName() string {
	name := fmt.Sprintf("bo-%s-%s", runtime.GOOS, runtime.GOARCH)
	if runtime.GOOS == "windows" {
		name += ".exe"
	}
	return name
}

// updateUnix downloads the new binary to a temp file in the same directory,
// sets executable permissions, then atomically replaces the running binary.
func updateUnix(downloadURL, exePath, exeDir string) error {
	tmpPath := filepath.Join(exeDir, ".bo-update-tmp")
	if err := downloadBinary(downloadURL, tmpPath); err != nil {
		os.Remove(tmpPath)
		return fmt.Errorf("downloading: %w", err)
	}
	if err := os.Rename(tmpPath, exePath); err != nil {
		os.Remove(tmpPath)
		return fmt.Errorf("replacing executable: %w", err)
	}
	fmt.Fprintln(os.Stdout, "Update complete.")
	return nil
}

// updateWindows renames the existing bo.exe to bo-{version}.exe (since Windows
// cannot overwrite a running executable), then downloads the new release as bo.exe.
func updateWindows(downloadURL, exePath, exeDir, localVersion string) error {
	backupName := fmt.Sprintf("bo-%s.exe", localVersion)
	backupPath := filepath.Join(exeDir, backupName)

	if err := os.Rename(exePath, backupPath); err != nil {
		return fmt.Errorf("renaming current executable to %s: %w", backupName, err)
	}
	fmt.Fprintf(os.Stdout, "Renamed current executable to %s\n", backupName)

	newPath := filepath.Join(exeDir, "bo.exe")
	if err := downloadBinary(downloadURL, newPath); err != nil {
		// Best-effort rollback.
		os.Rename(backupPath, exePath)
		return fmt.Errorf("downloading: %w", err)
	}
	fmt.Fprintln(os.Stdout, "Update complete.")
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
	rootCmd.AddCommand(updateCmd)
	updateCmd.Flags().BoolP("yes", "y", false, "Skip confirmation prompt")
}
