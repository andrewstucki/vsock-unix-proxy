package oci

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strconv"
	"sync"
	"sync/atomic"
	"time"

	contentpkg "oras.land/oras-go/v2/content"
	"oras.land/oras-go/v2/content/memory"
	"oras.land/oras-go/v2/errdef"

	"github.com/opencontainers/go-digest"
	ocispec "github.com/opencontainers/image-spec/specs-go/v1"
	v1 "github.com/opencontainers/image-spec/specs-go/v1"
)

const (
	// AnnotationDigest is the annotation key for the digest of the uncompressed content.
	AnnotationDigest = "com.vm.content.digest"

	// AnnotationUncompressedSize is the annotation key for the size of the uncompressed content.
	AnnotationUncompressedSize = "com.vm.content.uncompressed-size"

	// AnnotationUncompressedDigest is the annotation key for the digest of the uncompressed content.
	AnnotationUncompressedDigest = "com.vm.content.uncompressed-digest"
)

var (
	// ErrStoreClosed is returned when the store is already closed.
	ErrStoreClosed = errors.New("store already closed")

	// ErrDuplicateName is returned when a name is duplicated.
	ErrDuplicateName = errors.New("duplicate name")
)

// Store holds information about bundled content and provides functionalities to manage OCI images.
type Store struct {
	workingDir     string
	ignoreExisting bool

	closed          int32    // if the store is closed - 0: false, 1: true.
	digestToPath    sync.Map // map[digest.Digest]string
	mediaTypeToPath sync.Map // map[string]string
	nameToStatus    sync.Map // map[string]*nameStatus
	tmpFiles        sync.Map // map[string]bool

	memoryStore *memory.Store
}

// New initializes and returns a new Store with the given working directory and the ignoreExisting flag.
func New(workingDir string, ignoreExisting bool) (*Store, error) {
	workingDirAbs, err := filepath.Abs(workingDir)
	if err != nil {
		return nil, fmt.Errorf("failed to resolve absolute path for %s: %w", workingDir, err)
	}

	return &Store{
		workingDir:     workingDirAbs,
		ignoreExisting: ignoreExisting,

		memoryStore: memory.New(),
	}, nil
}

// Close closes the Store, removing any temporary files and marking the store as closed.
func (s *Store) Close(ctx context.Context) (err error) {
	if s.isClosedSet() {
		return ErrStoreClosed
	}
	s.setClosed()

	var errs []error
	var files []string
	s.tmpFiles.Range(func(name, _ any) bool {
		path, ok := name.(string)
		if !ok {
			return true
		}
		files = append(files, path)
		if err := os.Remove(path); err != nil {
			errs = append(errs, err)
		}
		return true
	})

	return errors.Join(errs...)
}

// Fetch retrieves content by its Descriptor, either from the store or the fallback memory store.
func (s *Store) Fetch(ctx context.Context, target ocispec.Descriptor) (fp io.ReadCloser, err error) {
	if s.isClosedSet() {
		return nil, ErrStoreClosed
	}

	// check if the content exists in the store
	val, exists := s.digestToPath.Load(target.Digest)
	if exists {
		path, ok := val.(string)
		if !ok {
			return nil, fmt.Errorf("failed to cast value to string: %v", val)
		}

		fp, err = os.Open(path)
		if err != nil {
			if os.IsNotExist(err) {
				return nil, fmt.Errorf("%s: %s: %w", target.Digest, target.MediaType, errdef.ErrNotFound)
			}
			return nil, err
		}

		return fp, nil
	}

	// if the content does not exist in the store,
	// then fall back to the fallback storage.
	return s.memoryStore.Fetch(ctx, target)
}

// Push saves content to the store, ensuring it adheres to expected media types and is not duplicated.
func (s *Store) Push(ctx context.Context, expected ocispec.Descriptor, content io.Reader) (err error) {
	if s.isClosedSet() {
		return ErrStoreClosed
	}

	name := expected.Annotations[ocispec.AnnotationTitle]
	if name == "" {
		return s.memoryStore.Push(ctx, expected, content)
	}

	// check the status of the name
	status := s.status(name)
	status.Lock()
	defer status.Unlock()
	if status.exists {
		return fmt.Errorf("%s: %w", name, ErrDuplicateName)
	}

	if !IsMediaTypeSupported(expected.MediaType) {
		return fmt.Errorf("unsupported media type: %s", expected.MediaType)
	}

	outputFilePath := filepath.Join(s.workingDir, name)
	if err = s.processContentByType(ctx, expected, content, outputFilePath); err != nil {
		return err
	}

	// update the name status as existed
	status.exists = true

	return nil
}

// Exists checks whether content exists in the store or on disk, validating it if necessary.
func (s *Store) Exists(ctx context.Context, target ocispec.Descriptor) (ok bool, err error) {
	if s.isClosedSet() {
		return false, ErrStoreClosed
	}

	// check if the content exists in the store
	_, exists := s.mediaTypeToPath.Load(target.MediaType)
	if exists {
		return true, nil
	}

	// check if the content exists on the disk and validate if it does
	name := target.Annotations[ocispec.AnnotationTitle]
	filePath := filepath.Join(s.workingDir, name)

	// if the content exists on the disk and is not ignored, validate it
	if _, err := os.Stat(filePath); err == nil && !s.ignoreExisting && name != "" {
		d := target.Digest
		if uncompressedDigest := target.Annotations[AnnotationUncompressedDigest]; uncompressedDigest != "" {
			d = digest.Digest(uncompressedDigest)
		}

		// Validate local file with output path with digest
		err = ValidateFileWithDigest(ctx, filePath, d)
		if err == nil {
			s.mediaTypeToPath.Store(target.MediaType, filePath)
			return true, nil
		}
	}

	// if the content does not exist in the store,
	// then fall back to the fallback storage.
	return s.memoryStore.Exists(ctx, target)
}

// Resolve attempts to resolve a reference to a Descriptor.
func (s *Store) Resolve(ctx context.Context, reference string) (d ocispec.Descriptor, err error) {
	if s.isClosedSet() {
		return ocispec.Descriptor{}, ErrStoreClosed
	}

	if reference == "" {
		return ocispec.Descriptor{}, errdef.ErrMissingReference
	}

	return s.memoryStore.Resolve(ctx, reference)
}

// Tag assigns a reference to a Descriptor if the content exists.
func (s *Store) Tag(ctx context.Context, desc ocispec.Descriptor, reference string) (err error) {
	if s.isClosedSet() {
		return ErrStoreClosed
	}

	if reference == "" {
		return errdef.ErrMissingReference
	}

	exists, err := s.Exists(ctx, desc)
	if err != nil {
		return err
	}
	if !exists {
		return fmt.Errorf("%s: %s: %w", desc.Digest, desc.MediaType, errdef.ErrNotFound)
	}

	return s.memoryStore.Tag(ctx, desc, reference)
}

// Predecessors returns the nodes directly pointing to the current node.
// Predecessors returns nil without error if the node does not exists in the
// store.
func (s *Store) Predecessors(ctx context.Context, node ocispec.Descriptor) ([]ocispec.Descriptor, error) {
	return nil, nil
}

// Add saves the content to the store, ensuring it adheres to expected media types and is not duplicated.
func (s *Store) Add(ctx context.Context, mediaType string, path string) (ocispec.Descriptor, error) {
	if s.isClosedSet() {
		return ocispec.Descriptor{}, ErrStoreClosed
	}

	if !IsMediaTypeSupported(mediaType) {
		return ocispec.Descriptor{}, fmt.Errorf("unsupported media type: %s", mediaType)
	}

	// check the status of the name
	mt := MediaType(mediaType)
	name := mt.Title()
	status := s.status(name)
	status.Lock()
	defer status.Unlock()

	if status.exists {
		return ocispec.Descriptor{}, fmt.Errorf("%s: %w", name, ErrDuplicateName)
	}

	if path == "" {
		path = name
	}
	path = s.absPath(path)

	_, err := os.Stat(path)
	if err != nil {
		return ocispec.Descriptor{}, fmt.Errorf("failed to stat %s: %w", path, err)
	}

	desc, err := s.descriptorFromStorageFile(ctx, mt, path)
	if err != nil {
		return ocispec.Descriptor{}, fmt.Errorf("failed to create descriptor for %s: %w", name, err)
	}

	s.mediaTypeToPath.Store(mediaType, path)
	// update the name status as existed
	status.exists = true

	return desc, nil
}

// Set saves the Store's configuration.
func (s *Store) Set(ctx context.Context, cfg Config) (ocispec.Descriptor, error) {
	if s.isClosedSet() {
		return ocispec.Descriptor{}, ErrStoreClosed
	}

	// check the status of the name
	name := MediaTypeConfigV1.Title()
	status := s.status(name)
	status.Lock()
	defer status.Unlock()

	if status.exists {
		return ocispec.Descriptor{}, fmt.Errorf("%s: %w", name, ErrDuplicateName)
	}

	desc, err := s.descriptorFromConfig(ctx, cfg)
	if err != nil {
		return ocispec.Descriptor{}, fmt.Errorf("failed to create descriptor for %s: %w", MediaTypeConfigV1.Title(), err)
	}

	// update the name status as existed
	status.exists = true

	return desc, nil
}

// GetManifestConfigDescriptor retrieves the descriptor for the Store's Manifest configuration.
func (s *Store) GetManifestConfigDescriptor(ctx context.Context) (ocispec.Descriptor, error) {
	if s.isClosedSet() {
		return ocispec.Descriptor{}, ErrStoreClosed
	}

	configDesc := ocispec.DescriptorEmptyJSON
	configDesc.Platform = &ocispec.Platform{
		Architecture: "arm64",
		OS:           "darwin",
	}

	if ok, err := s.memoryStore.Exists(ctx, configDesc); !ok || err != nil {
		_ = s.memoryStore.Push(ctx, configDesc, bytes.NewReader(ocispec.DescriptorEmptyJSON.Data))
	}

	return configDesc, nil
}

// GetFilePathForMediaType returns the file path for the given media type.
func (s *Store) GetFilePathForMediaType(ctx context.Context, mediaType MediaType) (path string, err error) {
	if s.isClosedSet() {
		return path, ErrStoreClosed
	}

	val, ok := s.mediaTypeToPath.Load(string(mediaType))
	if !ok {
		return path, fmt.Errorf("media type %s not found", mediaType)
	}

	path, ok = val.(string)
	if !ok {
		return path, fmt.Errorf("failed to cast value to string: %v", val)
	}

	return path, nil
}

// GetConfig retrieves the Store's configuration.
func (s *Store) GetConfig(ctx context.Context) (cfg Config, err error) {
	if s.isClosedSet() {
		return cfg, ErrStoreClosed
	}

	path, err := s.GetFilePathForMediaType(ctx, MediaTypeConfigV1)
	if err != nil {
		return Config{}, fmt.Errorf("failed to get file path for media type %s: %w", MediaTypeConfigV1, err)
	}

	fp, err := os.Open(path)
	if err != nil {
		return Config{}, fmt.Errorf("failed to open file %s: %w", path, err)
	}
	defer func() {
		err = errors.Join(err, fp.Close())
	}()

	config := &Config{}
	if err := json.NewDecoder(fp).Decode(config); err != nil {
		return Config{}, fmt.Errorf("failed to decode bundle: %w", err)
	}

	return *config, nil
}

// nameStatus contains a flag indicating if a name exists,
// and a RWMutex protecting it.
type nameStatus struct {
	sync.RWMutex
	exists bool
}

// absPath returns the absolute path of the path.
func (s *Store) absPath(path string) string {
	if filepath.IsAbs(path) {
		return path
	}
	return filepath.Join(s.workingDir, path)
}

// tempFile creates a temp file with the file name format "macosvz_file_randomString",
// and returns the pointer to the temp file.
func (s *Store) tempFile() (*os.File, error) {
	tmp, err := os.CreateTemp(os.TempDir(), "macosvz_file_*")
	if err != nil {
		return nil, err
	}

	s.tmpFiles.Store(tmp.Name(), true)
	return tmp, nil
}

// status returns the nameStatus for the given name.
func (s *Store) status(name string) *nameStatus {
	v, _ := s.nameToStatus.LoadOrStore(name, &nameStatus{sync.RWMutex{}, false})
	status, _ := v.(*nameStatus)
	return status
}

// isClosedSet returns true if the `closed` flag is set, otherwise returns false.
func (s *Store) isClosedSet() bool {
	return atomic.LoadInt32(&s.closed) == 1
}

// setClosed sets the `closed` flag.
func (s *Store) setClosed() {
	atomic.StoreInt32(&s.closed, 1)
}

// processContentByType processes content based on whether it is compressed or regular.
func (s *Store) processContentByType(ctx context.Context, expected ocispec.Descriptor, content io.Reader, outputFilePath string) (err error) {
	if err = os.MkdirAll(s.workingDir, os.ModePerm); err != nil {
		return fmt.Errorf("failed to ensure the working directory exists: %w", err)
	}

	uncompressedSize, sizeExists := expected.Annotations[AnnotationUncompressedSize]
	uncompressedDigest, digestExists := expected.Annotations[AnnotationUncompressedDigest]
	if sizeExists && digestExists {
		return s.processCompressedContent(ctx, expected, content, outputFilePath, uncompressedSize, uncompressedDigest)
	}

	return s.processRegularContent(ctx, expected, content, outputFilePath)
}

// processCompressedContent handles content that is compressed, attempting to decompress it.
func (s *Store) processCompressedContent(ctx context.Context, expected ocispec.Descriptor, content io.Reader, outputFilePath, uncompressedSize, uncompressedDigest string) (err error) {
	size, err := strconv.ParseInt(uncompressedSize, 10, 64)
	if err != nil {
		return fmt.Errorf("invalid uncompressed size: %w", err)
	}

	fp, err := s.tempFile()
	if err != nil {
		return err
	}
	defer func() {
		err = errors.Join(err, fp.Close())
	}()

	path := fp.Name()

	// save the content to the temp file
	if err = s.saveFile(ctx, fp, expected, content); err != nil {
		return fmt.Errorf("failed to save content to temp file: %w", err)
	}

	// Since file was saved successfully, store the digest and path
	s.digestToPath.Store(expected.Digest, path)

	d, err := DecompressFileWithPath(ctx, path, outputFilePath, size)
	if err != nil {
		return fmt.Errorf("failed to decompress file: %w", err)
	}

	if d != digest.Digest(uncompressedDigest) {
		return fmt.Errorf("digest mismatch: expected %s, got %s", uncompressedDigest, d)
	}
	s.mediaTypeToPath.Store(expected.MediaType, outputFilePath)

	return nil
}

// processRegularContent handles content that is not compressed, saving it directly.
func (s *Store) processRegularContent(ctx context.Context, expected ocispec.Descriptor, content io.Reader, outputFilePath string) (err error) {
	fp, err := os.Create(outputFilePath)
	if err != nil {
		return fmt.Errorf("failed to create file: %w", err)
	}
	defer func() {
		err = errors.Join(err, fp.Close())
	}()

	if err = s.saveFile(ctx, fp, expected, content); err != nil {
		return fmt.Errorf("failed to save content: %w", err)
	}

	// Since file was saved successfully, store the digest and path
	s.digestToPath.Store(expected.Digest, outputFilePath)

	if err = ComputeAndVerifyFileDigest(outputFilePath, expected.Digest); err != nil {
		return fmt.Errorf("failed to verify file digest: %w", err)
	}

	s.mediaTypeToPath.Store(expected.MediaType, outputFilePath)

	return nil
}

// saveFile saves content matching an ocispec.Descriptor to a given file, performing verification.
func (s *Store) saveFile(ctx context.Context, fp *os.File, expected ocispec.Descriptor, content io.Reader) (err error) {
	path := fp.Name()

	// verify while copying
	vr := contentpkg.NewVerifyReader(content, expected)

	// copy content to the file
	if _, err = io.Copy(fp, vr); err != nil {
		return fmt.Errorf("failed to copy content to %s: %w", path, err)
	}

	// verify the content
	if err = vr.Verify(); err != nil {
		return fmt.Errorf("failed to verify content in %s: %w", path, err)
	}

	// sync file
	if err = fp.Sync(); err != nil {
		return fmt.Errorf("failed to sync %s: %w", path, err)
	}

	return nil
}

type descriptorFileInfo struct {
	v1.Descriptor
}

func (d descriptorFileInfo) Name() string {
	if title, ok := mediaTypeToTitle[MediaType(d.Descriptor.MediaType)]; ok {
		return title
	}
	return "manifest.json"
}

func (d descriptorFileInfo) Size() int64        { return d.Descriptor.Size }
func (d descriptorFileInfo) Mode() fs.FileMode  { return 0o644 }
func (d descriptorFileInfo) ModTime() time.Time { return time.Now() }
func (d descriptorFileInfo) IsDir() bool        { return false }
func (d descriptorFileInfo) Sys() any           { return nil }

// Archive archives the entire store content to a tarball
func (s *Store) Archive(ctx context.Context, tarball string, manifest v1.Manifest) error {
	if s.isClosedSet() {
		return ErrStoreClosed
	}

	// Resolve absolute output tarball path
	dstTarAbs, err := filepath.Abs(tarball)
	if err != nil {
		return fmt.Errorf("failed to resolve absolute path for %s: %w", tarball, err)
	}

	// Create destination file
	outFile, err := os.Create(dstTarAbs)
	if err != nil {
		return fmt.Errorf("failed to create tarball %s: %w", dstTarAbs, err)
	}
	defer func() {
		err = errors.Join(err, outFile.Close())
	}()

	gz := gzip.NewWriter(outFile)
	defer func() {
		err = errors.Join(err, gz.Close())
	}()

	tw := tar.NewWriter(gz)
	defer func() {
		err = errors.Join(err, tw.Close())
	}()

	for _, descriptor := range append([]v1.Descriptor{manifest.Config}, manifest.Layers...) {
		fp, err := s.Fetch(ctx, descriptor)
		if err != nil {
			return fmt.Errorf("failed to get content: %w", err)
		}
		defer func() {
			err = errors.Join(err, fp.Close())
		}()

		info := descriptorFileInfo{descriptor}

		header, err := tar.FileInfoHeader(info, "")
		if err != nil {
			return fmt.Errorf("failed creating tar header: %w", err)
		}

		if err := tw.WriteHeader(header); err != nil {
			return fmt.Errorf("failed writing tar header for %s: %w", info.Name(), err)
		}

		_, err = io.Copy(tw, fp)
		if err != nil {
			return fmt.Errorf("failed writing file %s to tar: %w", info.Name(), err)
		}
	}

	if err != nil {
		return fmt.Errorf("failed to archive directory: %w", err)
	}

	if err := outFile.Sync(); err != nil {
		return fmt.Errorf("failed to sync tarball %s: %w", dstTarAbs, err)
	}

	return nil
}
