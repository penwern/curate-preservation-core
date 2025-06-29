// Package utils provides functions for detecting and extracting various archive formats.
// It supports ZIP, 7-Zip, and TAR formats, including GZIP-compressed TAR files.
// It also includes functions for validating file paths, compressing directories to ZIP, and extracting archives.
package utils

import (
	"archive/tar"
	"archive/zip"
	"bytes"
	"compress/gzip"
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"github.com/bodgit/sevenzip"
	"github.com/penwern/curate-preservation-core/pkg/logger"
)

const maxExtractFileSize = 5 << 30 // 5GB limit for extracted files

// sanitizeFileMode ensures mode is within safe bounds to prevent overflow
func sanitizeFileMode(mode int64) os.FileMode {
	if mode < 0 || mode > 0o777 {
		logger.Warn("Invalid file mode %d, using default 0755", mode)
		return 0o755 // default safe mode
	}
	return os.FileMode(mode)
}

// ----------------------------
// Helper Functions
// ----------------------------

// validatePath ensures that target is within destDir (prevents ZipSlip).
func validatePath(target, destDir string) error {
	cleanDest := filepath.Clean(destDir) + string(os.PathSeparator)
	if !strings.HasPrefix(filepath.Clean(target), cleanDest) {
		return fmt.Errorf("illegal file path: %s", target)
	}
	return nil
}

// safeJoin safely joins a destination directory with a file name, validating against path traversal.
func safeJoin(destDir, fileName string) (string, error) {
	cleanDest := filepath.Clean(destDir) + string(os.PathSeparator)
	filePath := filepath.Join(cleanDest, fileName)
	if err := validatePath(filePath, cleanDest); err != nil {
		return "", err
	}
	return filePath, nil
}

// ----------------------------
// Detection Functions
// ----------------------------

// IsZipFile checks if a file is a ZIP archive by reading its signature.
func IsZipFile(path string) bool {
	file, err := os.Open(path) // #nosec G304 -- path is controlled and validated by caller or context
	if err != nil {
		return false
	}
	defer func() {
		if err := file.Close(); err != nil {
			logger.Error("Failed to close file: %v", err)
		}
	}()

	var signature [4]byte
	if _, err = file.Read(signature[:]); err != nil {
		return false
	}
	// ZIP file signature: 0x50 0x4B 0x03 0x04
	return signature == [4]byte{0x50, 0x4B, 0x03, 0x04}
}

// Is7zFile checks if a file is a 7-Zip archive by comparing its header signature.
func Is7zFile(path string) bool {
	file, err := os.Open(path) // #nosec G304 -- path is controlled and validated by caller or context
	if err != nil {
		return false
	}
	defer func() {
		if err := file.Close(); err != nil {
			logger.Error("Failed to close file: %v", err)
		}
	}()

	var header [6]byte
	if _, err = file.Read(header[:]); err != nil {
		return false
	}
	expected := []byte{0x37, 0x7A, 0xBC, 0xAF, 0x27, 0x1C}
	return bytes.Equal(header[:], expected)
}

// IsTarFile attempts to detect a tar archive by checking for the "ustar" magic.
// (Tar files don’t always have a unique signature; this checks for POSIX tar.)
func IsTarFile(path string) bool {
	file, err := os.Open(path) // #nosec G304 -- path is controlled and validated by caller or context
	if err != nil {
		return false
	}
	defer func() {
		if err := file.Close(); err != nil {
			logger.Error("Failed to close file: %v", err)
		}
	}()

	// POSIX tar header has magic "ustar" at offset 257.
	if _, err := file.Seek(257, io.SeekStart); err != nil {
		return false
	}
	buf := make([]byte, 6)
	n, err := file.Read(buf)
	if err != nil || n < 6 {
		return false
	}
	return strings.HasPrefix(string(buf), "ustar")
}

// IsActualArchive checks if a file is an actual archive (not an Office document that uses ZIP format)
func IsActualArchive(path string) bool {
	ext := strings.ToLower(filepath.Ext(path))
	// Microsoft Office documents use ZIP format but shouldn't be extracted
	officeExtensions := []string{".docx", ".xlsx", ".pptx", ".docm", ".xlsm", ".pptm"}
	if slices.Contains(officeExtensions, ext) {
		return false
	}
	// OpenDocument formats also use ZIP
	openDocExtensions := []string{".odt", ".ods", ".odp", ".odg", ".odf"}
	if slices.Contains(openDocExtensions, ext) {
		return false
	}
	// JAR files are ZIP-based but shouldn't be extracted in this context
	if ext == ".jar" {
		return false
	}
	return true
}

// ----------------------------
// Extraction Functions
// ----------------------------

// ExtractZip extracts the ZIP archive at src into dest.
// It validates file paths (ZipSlip check), uses os.Mkdir for directories,
// and returns the computed package name (dest/packageName).
func ExtractZip(ctx context.Context, src, dest string) (string, error) {
	reader, err := zip.OpenReader(src)
	if err != nil {
		return "", fmt.Errorf("failed to open zip file %q: %w", src, err)
	}
	defer func() {
		if err := reader.Close(); err != nil {
			logger.Error("Failed to close zip reader: %v", err)
		}
	}()

	// Ensure destination exists.
	if err := CreateDir(dest); err != nil {
		return "", fmt.Errorf("failed to create destination directory %q: %w", dest, err)
	}
	cleanDest := filepath.Clean(dest) + string(os.PathSeparator)

	for _, file := range reader.File {
		select {
		case <-ctx.Done():
			return "", ctx.Err()
		default:
		}
		filePath, err := safeJoin(cleanDest, file.Name)
		if err != nil {
			return "", fmt.Errorf("invalid file path %q: %w", file.Name, err)
		}
		if file.FileInfo().IsDir() {
			if err := CreateDir(filePath); err != nil {
				return "", fmt.Errorf("failed to create directory %q: %w", filePath, err)
			}
			continue
		}

		if err := CreateDir(filepath.Dir(filePath)); err != nil {
			return "", fmt.Errorf("failed to create parent directories for %q: %w", filePath, err)
		}

		// #nosec G304 -- filePath is validated by safeJoin
		outFile, err := os.Create(filePath)
		if err != nil {
			return "", fmt.Errorf("failed to create file %q: %w", filePath, err)
		}
		defer func() {
			if err := outFile.Close(); err != nil {
				logger.Error("Failed to close output file %q: %v", filePath, err)
			}
		}()
		rc, err := file.Open()
		if err != nil {
			return "", fmt.Errorf("failed to open file %q in archive: %w", file.Name, err)
		}
		defer func() {
			if err := rc.Close(); err != nil {
				logger.Error("Failed to close file reader for %q: %v", file.Name, err)
			}
		}()
		if _, err := io.Copy(outFile, io.LimitReader(rc, maxExtractFileSize)); err != nil {
			return "", fmt.Errorf("failed to copy contents to %q: %w", filePath, err)
		}
	}

	packageName := filepath.Base(strings.TrimSuffix(src, filepath.Ext(src)))
	extractedPath := filepath.Join(cleanDest, packageName)
	return extractedPath, nil
}

// Extract7z extracts the 7z archive at src into dest using similar logic.
func Extract7z(ctx context.Context, src, dest string) (string, error) {
	r, err := sevenzip.OpenReader(src)
	if err != nil {
		return "", fmt.Errorf("opening archive: %w", err)
	}
	defer func() {
		if err := r.Close(); err != nil {
			logger.Error("Failed to close 7z reader: %v", err)
		}
	}()

	// Ensure destination exists. Parents must exist.
	if _, err := os.Stat(dest); os.IsNotExist(err) {
		if err := os.Mkdir(dest, 0o750); err != nil {
			return "", fmt.Errorf("creating destination directory: %w", err)
		}
	}
	cleanDest := filepath.Clean(dest) + string(os.PathSeparator)

	for _, file := range r.File {
		select {
		case <-ctx.Done():
			return "", ctx.Err()
		default:
		}
		outPath, err := safeJoin(cleanDest, file.Name)
		if err != nil {
			return "", err
		}
		if file.FileHeader.FileInfo().IsDir() {
			if err := os.Mkdir(outPath, file.Mode()); err != nil && !os.IsExist(err) {
				return "", fmt.Errorf("creating directory %q: %w", outPath, err)
			}
			continue
		}

		parentDir := filepath.Dir(outPath)
		if _, err := os.Stat(parentDir); os.IsNotExist(err) {
			if err := os.Mkdir(parentDir, 0o750); err != nil {
				return "", fmt.Errorf("creating parent directories for %q: %w", outPath, err)
			}
		}

		rc, err := file.Open()
		if err != nil {
			return "", fmt.Errorf("opening file %q from archive: %w", file.Name, err)
		}
		defer func() {
			if err := rc.Close(); err != nil {
				logger.Error("Failed to close file reader for %q: %v", file.Name, err)
			}
		}()
		// #nosec G304 -- outPath is validated by safeJoin
		outFile, err := os.OpenFile(outPath, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, sanitizeFileMode(int64(file.Mode())))
		if err != nil {
			return "", fmt.Errorf("creating file %q: %w", outPath, err)
		}
		defer func() {
			if err := outFile.Close(); err != nil {
				logger.Error("Failed to close output file %q: %v", outPath, err)
			}
		}()
		if _, err := io.Copy(outFile, io.LimitReader(rc, maxExtractFileSize)); err != nil {
			return "", fmt.Errorf("copying contents to %q: %w", outPath, err)
		}
	}

	packageName := filepath.Base(strings.TrimSuffix(src, filepath.Ext(src)))
	extractedPath := filepath.Join(cleanDest, packageName)
	return extractedPath, nil
}

// ExtractTar extracts a TAR or TAR.GZ archive at src into dest.
// It performs a ZipSlip-like check and returns the computed package name.
func ExtractTar(ctx context.Context, src, dest string) (string, error) {
	file, err := os.Open(src) // #nosec G304 -- src is controlled and validated by caller or context
	if err != nil {
		return "", err
	}
	defer func() {
		if err := file.Close(); err != nil {
			logger.Error("Failed to close file: %v", err)
		}
	}()

	var tarReader *tar.Reader
	if strings.HasSuffix(src, ".gz") || strings.HasSuffix(src, ".tgz") {
		gr, err := gzip.NewReader(file)
		if err != nil {
			return "", err
		}
		defer func() {
			if err := gr.Close(); err != nil {
				logger.Error("Failed to close gzip reader: %v", err)
			}
		}()
		tarReader = tar.NewReader(gr)
	} else {
		tarReader = tar.NewReader(file)
	}

	// Ensure destination exists. Parents must exist.
	if _, err := os.Stat(dest); os.IsNotExist(err) {
		if err := os.Mkdir(dest, 0o750); err != nil {
			return "", err
		}
	}
	cleanDest := filepath.Clean(dest) + string(os.PathSeparator)

	for {
		select {
		case <-ctx.Done():
			return "", ctx.Err()
		default:
		}
		header, err := tarReader.Next()
		if err == io.EOF {
			break // end of archive
		}
		if err != nil {
			return "", err
		}
		filePath, err := safeJoin(cleanDest, header.Name)
		if err != nil {
			return "", err
		}

		switch header.Typeflag {
		case tar.TypeDir:
			if err := os.Mkdir(filePath, sanitizeFileMode(header.Mode)); err != nil && !os.IsExist(err) {
				return "", err
			}
		case tar.TypeReg:
			parentDir := filepath.Dir(filePath)
			if _, err := os.Stat(parentDir); os.IsNotExist(err) {
				if err := os.Mkdir(parentDir, 0o750); err != nil {
					return "", err
				}
			}
			// #nosec G304 -- filePath is validated by safeJoin
			outFile, err := os.Create(filePath)
			if err != nil {
				return "", err
			}
			defer func() {
				if err := outFile.Close(); err != nil {
					logger.Error("Failed to close output file %q: %v", filePath, err)
				}
			}()
			if _, err := io.Copy(outFile, io.LimitReader(tarReader, maxExtractFileSize)); err != nil {
				return "", err
			}
		}
	}

	packageName := filepath.Base(strings.TrimSuffix(src, filepath.Ext(src)))
	extractedPath := filepath.Join(cleanDest, packageName)
	return extractedPath, nil
}

// ExtractArchive extracts an archive from src to dest.
// It supports 7z, tar, and zip formats.
// It returns the path to the extracted archive.
func ExtractArchive(ctx context.Context, src, dest string) (string, error) {
	var aipPath string
	var err error

	switch {
	case Is7zFile(src):
		aipPath, err = Extract7z(ctx, src, dest)
		if err != nil {
			return "", fmt.Errorf("error extracting 7zip: %w", err)
		}
	case IsTarFile(src):
		aipPath, err = ExtractTar(ctx, src, dest)
		if err != nil {
			return "", fmt.Errorf("error extracting tar: %w", err)
		}
	case IsZipFile(src):
		aipPath, err = ExtractZip(ctx, src, dest)
		if err != nil {
			return "", fmt.Errorf("error extracting zip: %w", err)
		}
	default:
		return "", fmt.Errorf("archive is not in a supported format: %s", src)
	}

	if aipPath == "" {
		return "", fmt.Errorf("error extracting archive: %s", src)
	}
	return aipPath, nil
}

// ----------------------------
// Compression Functions
// ----------------------------

// CompressToZip compresses the contents of the src directory into a ZIP archive at dest.
func CompressToZip(ctx context.Context, src, dest string) error {
	// #nosec G304 -- dest is controlled by caller
	zipFile, err := os.Create(dest)
	if err != nil {
		return fmt.Errorf("creating zip file: %w", err)
	}
	defer func() {
		if err := zipFile.Close(); err != nil {
			logger.Error("Failed to close zip file: %v", err)
		}
	}()

	zipWriter := zip.NewWriter(zipFile)
	defer func() {
		if err := zipWriter.Close(); err != nil {
			logger.Error("Failed to close zip writer: %v", err)
		}
	}()

	return filepath.Walk(src, func(path string, info os.FileInfo, err error) error {
		select {
		case <-ctx.Done():
			return ctx.Err()
		default:
		}

		if err != nil {
			return fmt.Errorf("walking path: %w", err)
		}
		// Compute relative path.
		relPath, err := filepath.Rel(src, path)
		if err != nil {
			return fmt.Errorf("computing relative path: %w", err)
		}
		// Skip the root directory.
		if relPath == "." {
			return nil
		}

		header, err := zip.FileInfoHeader(info)
		if err != nil {
			return fmt.Errorf("creating zip header: %w", err)
		}
		header.Name = relPath
		if info.IsDir() {
			header.Name += "/"
		} else {
			header.Method = zip.Deflate
		}

		writerEntry, err := zipWriter.CreateHeader(header)
		if err != nil {
			return fmt.Errorf("creating zip entry: %w", err)
		}
		if !info.IsDir() {
			// #nosec G304 -- path is controlled by Walk and user context
			file, err := os.Open(path)
			if err != nil {
				return fmt.Errorf("opening file: %w", err)
			}
			defer func() {
				if err := file.Close(); err != nil {
					logger.Error("Failed to close file: %v", err)
				}
			}()

			select {
			case <-ctx.Done():
				return ctx.Err()
			default:
			}

			if _, err := io.Copy(writerEntry, file); err != nil {
				return fmt.Errorf("copying file contents: %w", err)
			}
		}
		return nil
	})
}
