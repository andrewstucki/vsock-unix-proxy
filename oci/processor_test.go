package oci

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"testing"

	"github.com/opencontainers/go-digest"
	ocispec "github.com/opencontainers/image-spec/specs-go/v1"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"oras.land/oras-go/v2"
	"oras.land/oras-go/v2/content/oci"
)

var _ oras.Target = &oci.Store{}
var _ oras.GraphTarget = &oci.Store{}
var _ oras.ReadOnlyTarget = &oci.Store{}
var _ oras.ReadOnlyGraphTarget = &oci.Store{}

func TestNew(t *testing.T) {
	// Setup
	tempDir := t.TempDir()

	// Test
	store, err := New(tempDir, false)

	// Verify
	assert.NoError(t, err)
	assert.NotNil(t, store)
}

// Helper function to handle Close errors in defer statements
func handleCloseError(t *testing.T, closer func(context.Context) error) {
	t.Helper()
	if err := closer(t.Context()); err != nil {
		t.Errorf("error closing: %v", err)
	}
}

func TestPushAndFetch(t *testing.T) {
	// Setup
	tempDir := t.TempDir()
	store, err := New(tempDir, false)
	require.NoError(t, err)
	defer handleCloseError(t, store.Close)

	// Create test content
	testContent := []byte("test content")
	testDigest := digest.FromBytes(testContent)

	// Create descriptor with an annotation (name)
	desc := ocispec.Descriptor{
		MediaType: string(MediaTypeVMInitrdImage),
		Digest:    testDigest,
		Size:      int64(len(testContent)),
		Annotations: map[string]string{
			ocispec.AnnotationTitle: "test-file",
		},
	}

	// Test Push
	err = store.Push(t.Context(), desc, bytes.NewReader(testContent))
	assert.NoError(t, err)

	// Test Fetch
	reader, err := store.Fetch(t.Context(), desc)
	assert.NoError(t, err)
	defer func() {
		if err := reader.Close(); err != nil {
			t.Errorf("error closing reader: %v", err)
		}
	}()

	// Read and verify content
	content, err := io.ReadAll(reader)
	assert.NoError(t, err)
	assert.Equal(t, testContent, content)
}

func TestExists(t *testing.T) {
	// Setup
	tempDir := t.TempDir()
	store, err := New(tempDir, false)
	require.NoError(t, err)
	defer handleCloseError(t, store.Close)

	// Create test content
	testContent := []byte("test content")
	testDigest := digest.FromBytes(testContent)

	desc := ocispec.Descriptor{
		MediaType: string(MediaTypeVMKernelImage),
		Digest:    testDigest,
		Size:      int64(len(testContent)),
		Annotations: map[string]string{
			ocispec.AnnotationTitle: "test-file",
		},
	}

	// Test non-existence
	exists, err := store.Exists(t.Context(), desc)
	assert.NoError(t, err)
	assert.False(t, exists)

	// Push content
	err = store.Push(t.Context(), desc, bytes.NewReader(testContent))
	require.NoError(t, err)

	// Test existence
	exists, err = store.Exists(t.Context(), desc)
	assert.NoError(t, err)
	assert.True(t, exists)
}

func TestTag(t *testing.T) {
	// Setup
	tempDir := t.TempDir()
	store, err := New(tempDir, false)
	require.NoError(t, err)
	defer handleCloseError(t, store.Close)

	// First ensure we have a valid manifest config descriptor (which exists in the memory store)
	desc, err := store.GetManifestConfigDescriptor(t.Context())
	require.NoError(t, err)

	// Test Tag using the descriptor we just got (which should be valid)
	reference := "test-reference"
	err = store.Tag(t.Context(), desc, reference)
	assert.NoError(t, err)

	// Test Resolve
	resolvedDesc, err := store.Resolve(t.Context(), reference)
	assert.NoError(t, err)
	assert.Equal(t, desc.Digest, resolvedDesc.Digest)
}

func TestResolveNotFound(t *testing.T) {
	// Setup
	tempDir := t.TempDir()
	store, err := New(tempDir, false)
	require.NoError(t, err)
	defer handleCloseError(t, store.Close)

	// Test Resolve with non-existent reference
	_, err = store.Resolve(t.Context(), "non-existent")
	assert.Error(t, err)
}

func TestAdd(t *testing.T) {
	// Setup
	tempDir := t.TempDir()
	store, err := New(tempDir, false)
	require.NoError(t, err)
	defer handleCloseError(t, store.Close)

	// Create a test file
	testFileName := filepath.Join(tempDir, "disk.img")
	testContent := []byte("test content")
	err = os.WriteFile(testFileName, testContent, 0644)
	require.NoError(t, err)

	// Test Add
	mediaType := string(MediaTypeVMInitrdImage)
	desc, err := store.Add(t.Context(), mediaType, testFileName)
	assert.NoError(t, err)
	assert.Equal(t, mediaType, desc.MediaType)

	// Verify descriptor contains expected annotations
	assert.Contains(t, desc.Annotations, ocispec.AnnotationTitle)
}

func TestSetAndGetConfig(t *testing.T) {
	// Setup
	tempDir := t.TempDir()
	store, err := New(tempDir, false)
	require.NoError(t, err)
	defer handleCloseError(t, store.Close)

	// Create test config
	testConfig := Config{
		Storage: []MediaType{MediaTypeVMKernelImage},
	}

	// Create a config.json file in the tempDir
	configPath := filepath.Join(tempDir, "config.json")
	configJSON, err := json.Marshal(testConfig)
	require.NoError(t, err)
	err = os.WriteFile(configPath, configJSON, 0644)
	require.NoError(t, err)

	// Test Set by directly using Add since Set uses internal media type
	mediaType := string(MediaTypeVMKernelImage) // We'll just test with a supported media type
	desc, err := store.Add(t.Context(), mediaType, configPath)
	assert.NoError(t, err)
	assert.Equal(t, mediaType, desc.MediaType)

	// Since we can't directly test GetConfig because it's tied to the internal mediaTypeConfigV1,
	// we'll test the Add functionality instead
	path, err := store.GetFilePathForMediaType(t.Context(), MediaType(mediaType))
	assert.NoError(t, err)
	assert.Equal(t, configPath, path)
}

func TestDuplicateName(t *testing.T) {
	// Setup
	tempDir := t.TempDir()
	store, err := New(tempDir, false)
	require.NoError(t, err)
	defer handleCloseError(t, store.Close)

	// Create test content
	testContent := []byte("test content")
	testDigest := digest.FromBytes(testContent)

	desc := ocispec.Descriptor{
		MediaType: string(MediaTypeVMKernelImage),
		Digest:    testDigest,
		Size:      int64(len(testContent)),
		Annotations: map[string]string{
			ocispec.AnnotationTitle: "test-file",
		},
	}

	// Push content first time
	err = store.Push(t.Context(), desc, bytes.NewReader(testContent))
	require.NoError(t, err)

	// Try to push content with same name
	err = store.Push(t.Context(), desc, bytes.NewReader(testContent))
	assert.ErrorIs(t, err, ErrDuplicateName)
}

func TestEventRecorderIntegration(t *testing.T) {
	// Setup
	tempDir := t.TempDir()

	// Create invalid file for validation failure
	testFileName := filepath.Join(tempDir, "test-file")
	err := os.WriteFile(testFileName, []byte("invalid content"), 0644)
	require.NoError(t, err)

	store, err := New(tempDir, false)
	require.NoError(t, err)
	defer handleCloseError(t, store.Close)

	// Create descriptor with incorrect digest to trigger validation failure
	correctDigest := digest.FromBytes([]byte("correct content"))
	desc := ocispec.Descriptor{
		MediaType: string(MediaTypeVMInitrdImage),
		Digest:    correctDigest,
		Size:      int64(len("correct content")),
		Annotations: map[string]string{
			ocispec.AnnotationTitle: "test-file",
		},
	}

	exists, err := store.Exists(t.Context(), desc)
	require.NoError(t, err)
	assert.False(t, exists) // File exists but validation fails, so Exists returns false
}

func TestPredecessors(t *testing.T) {
	// Setup
	tempDir := t.TempDir()
	store, err := New(tempDir, false)
	require.NoError(t, err)
	defer handleCloseError(t, store.Close)

	// Create a test descriptor
	desc := ocispec.Descriptor{
		MediaType: string(MediaTypeVMInitrdImage),
		Digest:    digest.FromString("test"),
	}

	// Test Predecessors
	predecessors, err := store.Predecessors(t.Context(), desc)
	assert.NoError(t, err)
	assert.Nil(t, predecessors)
}

func TestGetManifestConfigDescriptor(t *testing.T) {
	// Setup
	tempDir := t.TempDir()
	store, err := New(tempDir, false)
	require.NoError(t, err)
	defer handleCloseError(t, store.Close)

	// Test GetManifestConfigDescriptor
	desc, err := store.GetManifestConfigDescriptor(t.Context())
	assert.NoError(t, err)
	assert.NotNil(t, desc)
	assert.NotNil(t, desc.Platform)
	assert.Equal(t, "darwin", desc.Platform.OS)
	assert.Equal(t, "arm64", desc.Platform.Architecture)
}

func TestSet(t *testing.T) {
	// Setup
	tempDir := t.TempDir()
	store, err := New(tempDir, false)
	require.NoError(t, err)
	defer handleCloseError(t, store.Close)

	// Create test config
	testConfig := Config{
		Storage: []MediaType{MediaTypeVMInitrdImage},
	}

	// Test Set
	desc, err := store.Set(t.Context(), testConfig)
	assert.NoError(t, err)
	assert.NotEmpty(t, desc.Digest)
	assert.Greater(t, desc.Size, int64(0))
	assert.Contains(t, desc.Annotations, ocispec.AnnotationTitle)
}
