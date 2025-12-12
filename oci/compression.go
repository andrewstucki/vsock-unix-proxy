package oci

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"runtime"

	"github.com/klauspost/pgzip"
	"github.com/opencontainers/go-digest"
)

const (
	// DigestFileSuffix is the suffix for the digest file.
	DigestFileSuffix = ".digest"
	DefaultBlockSize = 100000
)

// CompressionResult contains the output file path, size, and digests of the compressed and uncompressed content.
type CompressionResult struct {
	OutputFilePath     string
	CompressedSize     int64
	UncompressedSize   int64
	GzDigest           digest.Digest
	UncompressedDigest digest.Digest
}

// CompressFileWithPath compresses the file at the given path and writes the compressed content to the output file.
func CompressFileWithPath(ctx context.Context, inputFilePath string, outputFile *os.File) (r CompressionResult, err error) {
	inputFile, err := os.Open(inputFilePath)
	if err != nil {
		return CompressionResult{}, err
	}
	defer func() {
		err = errors.Join(err, inputFile.Close())
	}()

	gzDigester := digest.Canonical.Digester()
	uncompressedDigester := digest.Canonical.Digester()

	multiWriter := io.MultiWriter(outputFile, gzDigester.Hash())
	teeReader := io.TeeReader(inputFile, uncompressedDigester.Hash())

	w, err := pgzip.NewWriterLevel(multiWriter, pgzip.DefaultCompression)
	if err != nil {
		return CompressionResult{}, err
	}
	err = w.SetConcurrency(DefaultBlockSize, runtime.NumCPU()*2)
	if err != nil {
		return CompressionResult{}, err
	}

	// Copy from the teeReader, which both reads inputFile and writes to uncompressedDigester
	_, err = io.Copy(w, teeReader)
	if err != nil {
		return CompressionResult{}, err
	}

	// flush all
	if err := w.Close(); err != nil {
		return CompressionResult{}, err
	}
	if err := outputFile.Sync(); err != nil {
		return CompressionResult{}, err
	}

	fi, err := outputFile.Stat()
	if err != nil {
		return CompressionResult{}, err
	}

	return CompressionResult{
		OutputFilePath:     outputFile.Name(),
		CompressedSize:     fi.Size(),
		UncompressedSize:   int64(w.UncompressedSize()),
		GzDigest:           gzDigester.Digest(),
		UncompressedDigest: uncompressedDigester.Digest(),
	}, nil
}

// writeDigestFile writes the digest to a file.
func writeDigestFile(filePath string, d digest.Digest) error {
	digestFilePath := digestFilePath(filePath)
	digestFile, err := os.Create(digestFilePath)
	if err != nil {
		return err
	}

	if _, err := digestFile.WriteString(d.String()); err != nil {
		return errors.Join(err, digestFile.Close())
	}

	return digestFile.Close()
}

// digestFilePath returns the path to the digest file.
func digestFilePath(filePath string) string {
	return filePath + DigestFileSuffix
}

// DecompressFileWithPath uncompresses the file at the given path and writes the uncompressed content to the output file.
// It uses gzip for uncompression and writes the content in chunks to the output file by skipping zero chunks.
func DecompressFileWithPath(ctx context.Context, inputFilePath, outputFilePath string, uncompressedSize int64) (d digest.Digest, err error) {
	// Open the source file for reading
	inputFile, err := os.Open(inputFilePath)
	if err != nil {
		return "", err
	}
	defer func() {
		err = errors.Join(err, inputFile.Close())
	}()

	// Create the output file for writing
	outputFile, err := os.Create(outputFilePath)
	if err != nil {
		return "", err
	}
	defer func() {
		err = errors.Join(err, outputFile.Close())
	}()

	err = outputFile.Truncate(uncompressedSize)
	if err != nil {
		return "", err
	}

	r, err := pgzip.NewReader(inputFile)
	if err != nil {
		return "", err
	}

	digester := digest.Canonical.Digester()
	h := digester.Hash()

	buf := make([]byte, 4<<20)
	sparseBlockSize := 64 << 10
	zeroChunk := make([]byte, sparseBlockSize)
	var offset int64

	for {
		select {
		case <-ctx.Done():
			return "", ctx.Err()
		default:
		}

		// Read a chunk
		n, err := r.Read(buf)
		if err == io.EOF {
			break // End of file
		}
		if err != nil {
			return "", err
		}

		for i := 0; i < n; {
			end := i + sparseBlockSize
			if end > n {
				end = n
			}
			chunk := buf[i:end]
			i = end

			h.Write(chunk) // Write chunk to the hash writer

			if !bytes.Equal(chunk, zeroChunk) {
				if _, err = outputFile.Seek(offset, io.SeekStart); err != nil {
					return "", fmt.Errorf("failed to seek in output file: %w", err)
				}
				if _, err = outputFile.Write(chunk); err != nil {
					return "", fmt.Errorf("failed to write to output file: %w", err)
				}
			}

			offset += int64(len(chunk))
		}
	}

	d = digester.Digest()

	// Write the digest to a file as a cache
	err = writeDigestFile(outputFilePath, d)
	if err != nil {
		return "", err
	}

	return d, nil
}

// ValidateFileWithDigest validates the file at the given path against the expected digest.
// If the digest file exists and is up-to-date, the digest is read from the file and compared against the expected digest.
func ValidateFileWithDigest(ctx context.Context, filePath string, expectedDigest digest.Digest) error {
	// Check the file existence
	fileInfo, err := os.Stat(filePath)
	if err != nil {
		return fmt.Errorf("error checking file: %w", err)
	}

	// Check digest file existence
	digestFilePath := digestFilePath(filePath)
	digestFileInfo, err := os.Stat(digestFilePath)
	isNotExistErr := os.IsNotExist(err)
	if err != nil && !isNotExistErr {
		return fmt.Errorf("error checking digest file: %w", err)
	}

	// If the digest file does not exist or the digest file is older than the file, compute and verify the digest manually
	if isNotExistErr || !digestFileInfo.ModTime().After(fileInfo.ModTime()) {
		return ComputeAndVerifyFileDigest(filePath, expectedDigest)
	}

	storedDigest, err := os.ReadFile(digestFilePath)
	if err != nil {
		return fmt.Errorf("error reading digest file: %w", err)
	}

	if expectedDigest.String() != string(storedDigest) {
		return fmt.Errorf("digest does not match: got %s, expected %s", string(storedDigest), expectedDigest)
	}

	return nil
}

// ComputeAndVerifyFileDigest computes the digest of the file at the given path and verifies it against the expected digest.
func ComputeAndVerifyFileDigest(filePath string, expectedDigest digest.Digest) error {
	rd, err := os.Open(filePath)
	if err != nil {
		return err
	}

	verifier := expectedDigest.Verifier()
	if _, err := io.Copy(verifier, rd); err != nil {
		return err
	}

	if !verifier.Verified() {
		return errors.New("digest verification failed")
	}

	// Write the digest to the digest file for future validation
	return writeDigestFile(filePath, expectedDigest)
}
