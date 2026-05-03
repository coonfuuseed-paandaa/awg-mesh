package cmd

import (
	"archive/zip"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"github.com/spf13/cobra"
)

const (
	backupArchiveFormat  = "awg-mesh-backup"
	backupArchiveVersion = 1

	backupManifestName        = "manifest.json"
	backupLocalConfigPrefix   = "local-config"
	backupTopologyPrefix      = "topology"
	backupControlPlanePrefix  = "control-plane-state"
	backupDefaultFileMode     = 0o600
	backupDefaultDirMode      = 0o700
	backupRestoreTempUnixNano = 36
)

type backupOptions struct {
	archivePath          string
	topologyPath         string
	configDir            string
	controlPlaneStateDir string
	stdout               io.Writer
}

type restoreOptions struct {
	archivePath          string
	topologyPath         string
	configDir            string
	controlPlaneStateDir string
	confirm              bool
	stdout               io.Writer
}

type backupManifest struct {
	Format        string                 `json:"format"`
	Version       int                    `json:"version"`
	CreatedAtUnix int64                  `json:"created_at_unix"`
	ConfigDir     string                 `json:"config_dir"`
	TopologyPath  string                 `json:"topology_path"`
	Includes      backupManifestIncludes `json:"includes"`
	Entries       []backupManifestEntry  `json:"entries"`
}

type backupManifestIncludes struct {
	LocalConfig       bool `json:"local_config"`
	Topology          bool `json:"topology"`
	ControlPlaneState bool `json:"control_plane_state"`
}

type backupManifestEntry struct {
	Prefix     string `json:"prefix"`
	SourcePath string `json:"source_path"`
	FileCount  int    `json:"file_count"`
	ByteCount  int64  `json:"byte_count"`
}

type backupEntrySummary struct {
	prefix     string
	sourcePath string
	fileCount  int
	byteCount  int64
}

func newBackupCommand() *cobra.Command {
	options := backupOptions{}
	cmd := &cobra.Command{
		Use:   "backup <archive>",
		Short: "Create a local state backup archive",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			options.archivePath = args[0]
			options.topologyPath = topologyPath
			options.configDir = configDir
			options.stdout = cmd.OutOrStdout()
			return runBackupCommand(options)
		},
	}
	cmd.Flags().StringVar(&options.controlPlaneStateDir, "control-plane-state-dir", "", "Optional control-plane state directory to include")
	return cmd
}

func newRestoreCommand() *cobra.Command {
	options := restoreOptions{}
	cmd := &cobra.Command{
		Use:   "restore <archive>",
		Short: "Restore local state from a backup archive",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			options.archivePath = args[0]
			options.topologyPath = topologyPath
			options.configDir = configDir
			options.stdout = cmd.OutOrStdout()
			return runRestoreCommand(options)
		},
	}
	cmd.Flags().StringVar(&options.controlPlaneStateDir, "control-plane-state-dir", "", "Control-plane state restore target when archive contains it")
	cmd.Flags().BoolVar(&options.confirm, "confirm", false, "Confirm destructive restore into configured paths")
	return cmd
}

func runBackupCommand(options backupOptions) error {
	archivePath, err := normalizedArchivePath(options.archivePath)
	if err != nil {
		return err
	}
	configRoot, err := requireExistingDir(options.configDir, "config directory")
	if err != nil {
		return err
	}
	topologyFile, err := requireExistingFile(options.topologyPath, "topology file")
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(archivePath), backupDefaultDirMode); err != nil {
		return fmt.Errorf("create backup archive directory: %w", err)
	}

	file, err := os.OpenFile(archivePath, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, backupDefaultFileMode)
	if err != nil {
		return fmt.Errorf("create backup archive: %w", err)
	}
	closeFile := true
	defer func() {
		if closeFile {
			_ = file.Close()
		}
	}()

	writer := zip.NewWriter(file)
	builder := backupArchiveBuilder{writer: writer, archivePath: archivePath}
	configSummary, err := builder.addDirectory(backupLocalConfigPrefix, configRoot)
	if err != nil {
		_ = writer.Close()
		return err
	}
	topologySummary, err := builder.addFile(backupTopologyPrefix, topologyFile)
	if err != nil {
		_ = writer.Close()
		return err
	}

	entries := []backupManifestEntry{
		manifestEntry(configSummary),
		manifestEntry(topologySummary),
	}
	includes := backupManifestIncludes{
		LocalConfig: true,
		Topology:    true,
	}
	if strings.TrimSpace(options.controlPlaneStateDir) != "" {
		controlPlaneRoot, err := requireExistingDir(options.controlPlaneStateDir, "control-plane state directory")
		if err != nil {
			_ = writer.Close()
			return err
		}
		controlPlaneSummary, err := builder.addDirectory(backupControlPlanePrefix, controlPlaneRoot)
		if err != nil {
			_ = writer.Close()
			return err
		}
		includes.ControlPlaneState = true
		entries = append(entries, manifestEntry(controlPlaneSummary))
	}

	manifest := backupManifest{
		Format:        backupArchiveFormat,
		Version:       backupArchiveVersion,
		CreatedAtUnix: time.Now().Unix(),
		ConfigDir:     configRoot,
		TopologyPath:  topologyFile,
		Includes:      includes,
		Entries:       entries,
	}
	if err := writeBackupManifest(writer, manifest); err != nil {
		_ = writer.Close()
		return err
	}
	if err := writer.Close(); err != nil {
		return fmt.Errorf("finalize backup archive: %w", err)
	}
	if err := file.Close(); err != nil {
		closeFile = false
		return fmt.Errorf("close backup archive: %w", err)
	}
	closeFile = false

	_, err = fmt.Fprintf(commandOutput(options.stdout), "backup written: %s\n", archivePath)
	return err
}

func runRestoreCommand(options restoreOptions) error {
	archivePath, err := normalizedArchivePath(options.archivePath)
	if err != nil {
		return err
	}
	reader, err := zip.OpenReader(archivePath)
	if err != nil {
		return fmt.Errorf("open backup archive: %w", err)
	}
	defer func() { _ = reader.Close() }()

	manifest, err := readBackupManifest(&reader.Reader)
	if err != nil {
		return err
	}
	if err := validateBackupArchive(&reader.Reader, manifest); err != nil {
		return err
	}
	if manifest.Includes.ControlPlaneState && strings.TrimSpace(options.controlPlaneStateDir) == "" {
		return fmt.Errorf("backup contains control-plane state; pass --control-plane-state-dir to choose the restore target")
	}
	configTarget, err := validateRestoreDirectory(options.configDir)
	if err != nil {
		return err
	}
	topologyTarget, err := validateRestoreFile(options.topologyPath)
	if err != nil {
		return err
	}
	if pathWithinDirectory(archivePath, configTarget) {
		return fmt.Errorf("backup archive %s is inside restore target %s", archivePath, configTarget)
	}
	controlPlaneTarget := ""
	if manifest.Includes.ControlPlaneState {
		controlPlaneTarget, err = validateRestoreDirectory(options.controlPlaneStateDir)
		if err != nil {
			return err
		}
		if pathWithinDirectory(archivePath, controlPlaneTarget) {
			return fmt.Errorf("backup archive %s is inside restore target %s", archivePath, controlPlaneTarget)
		}
	}
	if !options.confirm {
		return fmt.Errorf("restore is destructive; re-run with --confirm after verifying archive metadata")
	}

	if err := restoreDirectoryPrefix(&reader.Reader, backupLocalConfigPrefix, configTarget); err != nil {
		return err
	}
	if err := restoreTopologyFile(&reader.Reader, topologyTarget); err != nil {
		return err
	}
	if manifest.Includes.ControlPlaneState {
		if err := restoreDirectoryPrefix(&reader.Reader, backupControlPlanePrefix, controlPlaneTarget); err != nil {
			return err
		}
	}

	_, err = fmt.Fprintf(commandOutput(options.stdout), "backup restored: %s\n", archivePath)
	return err
}

type backupArchiveBuilder struct {
	writer      *zip.Writer
	archivePath string
}

func (b backupArchiveBuilder) addDirectory(prefix, root string) (backupEntrySummary, error) {
	summary := backupEntrySummary{prefix: prefix, sourcePath: root}
	err := filepath.WalkDir(root, func(current string, entry os.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		info, err := entry.Info()
		if err != nil {
			return fmt.Errorf("stat backup source %s: %w", current, err)
		}
		if info.Mode()&os.ModeSymlink != 0 {
			return fmt.Errorf("refusing to back up symlink %s", current)
		}
		if !info.IsDir() && samePath(current, b.archivePath) {
			return nil
		}
		rel, err := filepath.Rel(root, current)
		if err != nil {
			return fmt.Errorf("compute backup relative path for %s: %w", current, err)
		}
		if rel == "." {
			return nil
		}
		name := archivePath(prefix, rel)
		if info.IsDir() {
			if err := addZipDirectory(b.writer, name, info); err != nil {
				return err
			}
			return nil
		}
		if err := addZipFile(b.writer, name, current, info); err != nil {
			return err
		}
		summary.fileCount++
		summary.byteCount += info.Size()
		return nil
	})
	if err != nil {
		return backupEntrySummary{}, fmt.Errorf("add %s to backup: %w", root, err)
	}
	return summary, nil
}

func (b backupArchiveBuilder) addFile(prefix, source string) (backupEntrySummary, error) {
	info, err := os.Lstat(source)
	if err != nil {
		return backupEntrySummary{}, fmt.Errorf("stat backup source %s: %w", source, err)
	}
	if info.IsDir() {
		return backupEntrySummary{}, fmt.Errorf("backup source %s is a directory", source)
	}
	if info.Mode()&os.ModeSymlink != 0 {
		return backupEntrySummary{}, fmt.Errorf("refusing to back up symlink %s", source)
	}
	name := archivePath(prefix, filepath.Base(source))
	if err := addZipFile(b.writer, name, source, info); err != nil {
		return backupEntrySummary{}, err
	}
	return backupEntrySummary{prefix: prefix, sourcePath: source, fileCount: 1, byteCount: info.Size()}, nil
}

func addZipDirectory(writer *zip.Writer, name string, info os.FileInfo) error {
	header, err := zip.FileInfoHeader(info)
	if err != nil {
		return fmt.Errorf("create zip header for %s: %w", name, err)
	}
	header.Name = strings.TrimRight(name, "/") + "/"
	header.Method = zip.Deflate
	header.SetMode(info.Mode())
	_, err = writer.CreateHeader(header)
	if err != nil {
		return fmt.Errorf("create zip directory %s: %w", name, err)
	}
	return nil
}

func addZipFile(writer *zip.Writer, name, source string, info os.FileInfo) error {
	header, err := zip.FileInfoHeader(info)
	if err != nil {
		return fmt.Errorf("create zip header for %s: %w", name, err)
	}
	header.Name = name
	header.Method = zip.Deflate
	header.SetMode(info.Mode())
	target, err := writer.CreateHeader(header)
	if err != nil {
		return fmt.Errorf("create zip file %s: %w", name, err)
	}
	file, err := os.Open(source)
	if err != nil {
		return fmt.Errorf("open backup source %s: %w", source, err)
	}
	defer func() { _ = file.Close() }()
	if _, err := io.Copy(target, file); err != nil {
		return fmt.Errorf("copy backup source %s: %w", source, err)
	}
	return nil
}

func writeBackupManifest(writer *zip.Writer, manifest backupManifest) error {
	data, err := json.MarshalIndent(manifest, "", "  ")
	if err != nil {
		return fmt.Errorf("encode backup manifest: %w", err)
	}
	header := &zip.FileHeader{Name: backupManifestName, Method: zip.Deflate}
	header.SetMode(backupDefaultFileMode)
	target, err := writer.CreateHeader(header)
	if err != nil {
		return fmt.Errorf("create backup manifest: %w", err)
	}
	if _, err := target.Write(data); err != nil {
		return fmt.Errorf("write backup manifest: %w", err)
	}
	return nil
}

func readBackupManifest(reader *zip.Reader) (backupManifest, error) {
	for _, file := range reader.File {
		if file.Name != backupManifestName {
			continue
		}
		body, err := readZipFile(file)
		if err != nil {
			return backupManifest{}, fmt.Errorf("read backup manifest: %w", err)
		}
		var manifest backupManifest
		if err := json.Unmarshal(body, &manifest); err != nil {
			return backupManifest{}, fmt.Errorf("decode backup manifest: %w", err)
		}
		return manifest, nil
	}
	return backupManifest{}, fmt.Errorf("backup manifest %q is missing", backupManifestName)
}

func validateBackupArchive(reader *zip.Reader, manifest backupManifest) error {
	if manifest.Format != backupArchiveFormat {
		return fmt.Errorf("unsupported backup format %q", manifest.Format)
	}
	if manifest.Version != backupArchiveVersion {
		return fmt.Errorf("unsupported backup version %d", manifest.Version)
	}
	if !manifest.Includes.LocalConfig {
		return fmt.Errorf("backup manifest does not include local config state")
	}
	if !manifest.Includes.Topology {
		return fmt.Errorf("backup manifest does not include topology state")
	}

	seenManifest := false
	for _, file := range reader.File {
		if file.Name == backupManifestName {
			seenManifest = true
			continue
		}
		prefix, err := validateArchiveMemberName(file.Name)
		if err != nil {
			return err
		}
		switch prefix {
		case backupLocalConfigPrefix, backupTopologyPrefix:
		case backupControlPlanePrefix:
			if !manifest.Includes.ControlPlaneState {
				return fmt.Errorf("archive contains %s entries but manifest does not declare control-plane state", backupControlPlanePrefix)
			}
		default:
			return fmt.Errorf("archive contains unsupported entry prefix %q", prefix)
		}
	}
	if !seenManifest {
		return fmt.Errorf("backup manifest %q is missing", backupManifestName)
	}
	return nil
}

func validateArchiveMemberName(name string) (string, error) {
	if strings.TrimSpace(name) == "" {
		return "", fmt.Errorf("archive contains empty entry name")
	}
	if strings.HasPrefix(name, "/") || strings.Contains(name, "\\") {
		return "", fmt.Errorf("archive entry %q is not a portable relative path", name)
	}
	entryName := strings.TrimSuffix(name, "/")
	clean := path.Clean(entryName)
	if clean == "." || clean != entryName || strings.HasPrefix(clean, "../") || clean == ".." {
		return "", fmt.Errorf("archive entry %q is not a safe relative path", name)
	}
	parts := strings.Split(clean, "/")
	if len(parts) < 2 {
		return "", fmt.Errorf("archive entry %q is missing a state prefix", name)
	}
	return parts[0], nil
}

func restoreDirectoryPrefix(reader *zip.Reader, prefix, targetDir string) error {
	targetRoot, err := validateRestoreDirectory(targetDir)
	if err != nil {
		return err
	}
	parent := filepath.Dir(targetRoot)
	if err := os.MkdirAll(parent, backupDefaultDirMode); err != nil {
		return fmt.Errorf("create restore parent directory: %w", err)
	}
	tempRoot := filepath.Join(parent, "."+filepath.Base(targetRoot)+".restore-"+strconvUnixNano(time.Now()))
	if err := os.MkdirAll(tempRoot, backupDefaultDirMode); err != nil {
		return fmt.Errorf("create restore temp directory: %w", err)
	}
	committed := false
	defer func() {
		if !committed {
			_ = os.RemoveAll(tempRoot)
		}
	}()

	found := false
	for _, file := range reader.File {
		rel, ok := strings.CutPrefix(file.Name, prefix+"/")
		if !ok || rel == "" {
			continue
		}
		found = true
		targetPath, err := safeRestorePath(tempRoot, rel)
		if err != nil {
			return err
		}
		if file.FileInfo().IsDir() {
			if err := os.MkdirAll(targetPath, directoryMode(file.Mode())); err != nil {
				return fmt.Errorf("create restore directory %s: %w", targetPath, err)
			}
			continue
		}
		if err := os.MkdirAll(filepath.Dir(targetPath), backupDefaultDirMode); err != nil {
			return fmt.Errorf("create restore file parent %s: %w", targetPath, err)
		}
		if err := extractZipFile(file, targetPath); err != nil {
			return err
		}
	}
	if !found {
		return fmt.Errorf("archive contains no %s entries", prefix)
	}
	if err := os.RemoveAll(targetRoot); err != nil {
		return fmt.Errorf("remove existing restore target %s: %w", targetRoot, err)
	}
	if err := os.Rename(tempRoot, targetRoot); err != nil {
		return fmt.Errorf("replace restore target %s: %w", targetRoot, err)
	}
	committed = true
	return nil
}

func restoreTopologyFile(reader *zip.Reader, topologyTarget string) error {
	targetPath, err := validateRestoreFile(topologyTarget)
	if err != nil {
		return err
	}
	var topologyEntry *zip.File
	for _, file := range reader.File {
		rel, ok := strings.CutPrefix(file.Name, backupTopologyPrefix+"/")
		if !ok || rel == "" || file.FileInfo().IsDir() {
			continue
		}
		if strings.Contains(rel, "/") {
			return fmt.Errorf("archive topology entry %q must be a single file", file.Name)
		}
		if topologyEntry != nil {
			return fmt.Errorf("archive contains more than one topology file")
		}
		topologyEntry = file
	}
	if topologyEntry == nil {
		return fmt.Errorf("archive contains no topology file")
	}
	if err := os.MkdirAll(filepath.Dir(targetPath), backupDefaultDirMode); err != nil {
		return fmt.Errorf("create topology restore directory: %w", err)
	}
	tempPath := targetPath + ".restore-tmp"
	if err := extractZipFile(topologyEntry, tempPath); err != nil {
		_ = os.Remove(tempPath)
		return err
	}
	if err := os.Remove(targetPath); err != nil && !os.IsNotExist(err) {
		_ = os.Remove(tempPath)
		return fmt.Errorf("remove existing topology file %s: %w", targetPath, err)
	}
	if err := os.Rename(tempPath, targetPath); err != nil {
		_ = os.Remove(tempPath)
		return fmt.Errorf("replace topology file %s: %w", targetPath, err)
	}
	return nil
}

func extractZipFile(file *zip.File, targetPath string) error {
	source, err := file.Open()
	if err != nil {
		return fmt.Errorf("open archive entry %s: %w", file.Name, err)
	}
	defer func() { _ = source.Close() }()
	target, err := os.OpenFile(targetPath, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, fileMode(file.Mode()))
	if err != nil {
		return fmt.Errorf("create restore file %s: %w", targetPath, err)
	}
	if _, err := io.Copy(target, source); err != nil {
		_ = target.Close()
		return fmt.Errorf("extract archive entry %s: %w", file.Name, err)
	}
	if err := target.Close(); err != nil {
		return fmt.Errorf("close restore file %s: %w", targetPath, err)
	}
	return nil
}

func readZipFile(file *zip.File) ([]byte, error) {
	source, err := file.Open()
	if err != nil {
		return nil, err
	}
	defer func() { _ = source.Close() }()
	return io.ReadAll(source)
}

func normalizedArchivePath(raw string) (string, error) {
	trimmed := strings.TrimSpace(raw)
	if trimmed == "" {
		return "", fmt.Errorf("archive path is required")
	}
	abs, err := filepath.Abs(trimmed)
	if err != nil {
		return "", fmt.Errorf("resolve archive path: %w", err)
	}
	return filepath.Clean(abs), nil
}

func requireExistingDir(raw, label string) (string, error) {
	abs, err := filepath.Abs(strings.TrimSpace(raw))
	if err != nil {
		return "", fmt.Errorf("resolve %s: %w", label, err)
	}
	info, err := os.Stat(abs)
	if err != nil {
		return "", fmt.Errorf("%s %s is required: %w", label, abs, err)
	}
	if !info.IsDir() {
		return "", fmt.Errorf("%s %s is not a directory", label, abs)
	}
	return filepath.Clean(abs), nil
}

func requireExistingFile(raw, label string) (string, error) {
	abs, err := filepath.Abs(strings.TrimSpace(raw))
	if err != nil {
		return "", fmt.Errorf("resolve %s: %w", label, err)
	}
	info, err := os.Stat(abs)
	if err != nil {
		return "", fmt.Errorf("%s %s is required: %w", label, abs, err)
	}
	if info.IsDir() {
		return "", fmt.Errorf("%s %s is a directory", label, abs)
	}
	return filepath.Clean(abs), nil
}

func validateRestoreDirectory(raw string) (string, error) {
	abs, err := filepath.Abs(strings.TrimSpace(raw))
	if err != nil {
		return "", fmt.Errorf("resolve restore directory: %w", err)
	}
	clean := filepath.Clean(abs)
	if err := rejectDangerousRestoreTarget(clean); err != nil {
		return "", err
	}
	return clean, nil
}

func validateRestoreFile(raw string) (string, error) {
	trimmed := strings.TrimSpace(raw)
	if trimmed == "" {
		return "", fmt.Errorf("restore file path is required")
	}
	abs, err := filepath.Abs(trimmed)
	if err != nil {
		return "", fmt.Errorf("resolve restore file: %w", err)
	}
	return filepath.Clean(abs), nil
}

func rejectDangerousRestoreTarget(target string) error {
	volume := filepath.VolumeName(target)
	root := volume + string(os.PathSeparator)
	if target == root || target == volume {
		return fmt.Errorf("refusing to restore over filesystem root %s", target)
	}
	if home, err := os.UserHomeDir(); err == nil && samePath(target, home) {
		return fmt.Errorf("refusing to restore over user home directory %s", target)
	}
	return nil
}

func safeRestorePath(root, archiveRel string) (string, error) {
	if _, err := validateArchiveMemberName("x/" + archiveRel); err != nil {
		return "", err
	}
	target := filepath.Clean(filepath.Join(root, filepath.FromSlash(archiveRel)))
	rel, err := filepath.Rel(root, target)
	if err != nil || rel == "." || strings.HasPrefix(rel, "..") || filepath.IsAbs(rel) {
		return "", fmt.Errorf("archive entry %q resolves outside restore target", archiveRel)
	}
	return target, nil
}

func archivePath(prefix, rel string) string {
	return prefix + "/" + filepath.ToSlash(filepath.Clean(rel))
}

func manifestEntry(summary backupEntrySummary) backupManifestEntry {
	return backupManifestEntry{
		Prefix:     summary.prefix,
		SourcePath: summary.sourcePath,
		FileCount:  summary.fileCount,
		ByteCount:  summary.byteCount,
	}
}

func fileMode(mode os.FileMode) os.FileMode {
	if perm := mode.Perm(); perm != 0 {
		return perm
	}
	return backupDefaultFileMode
}

func directoryMode(mode os.FileMode) os.FileMode {
	if perm := mode.Perm(); perm != 0 {
		return perm
	}
	return backupDefaultDirMode
}

func samePath(left, right string) bool {
	leftAbs, leftErr := filepath.Abs(left)
	rightAbs, rightErr := filepath.Abs(right)
	if leftErr != nil || rightErr != nil {
		return filepath.Clean(left) == filepath.Clean(right)
	}
	leftClean := filepath.Clean(leftAbs)
	rightClean := filepath.Clean(rightAbs)
	if runtime.GOOS == "windows" {
		return strings.EqualFold(leftClean, rightClean)
	}
	return leftClean == rightClean
}

func pathWithinDirectory(candidate, dir string) bool {
	rel, err := filepath.Rel(dir, candidate)
	if err != nil {
		return false
	}
	return rel != "." && !strings.HasPrefix(rel, "..") && !filepath.IsAbs(rel)
}

func strconvUnixNano(now time.Time) string {
	return strings.ToLower(fmt.Sprintf("%0*x", backupRestoreTempUnixNano, now.UnixNano()))
}
