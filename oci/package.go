package oci

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"

	v1 "github.com/opencontainers/image-spec/specs-go/v1"
	"oras.land/oras-go/v2"
)

func Package(ctx context.Context, tag, initrdPath, kernelPath, destinationPath string) (err error) {
	path, err := os.MkdirTemp("", "packager")
	if err != nil {
		return fmt.Errorf("creating temp directory: %v", err)
	}
	defer func() {
		if removeErr := os.RemoveAll(path); removeErr != nil {
			err = errors.Join(err, fmt.Errorf("removing temp files: %w", removeErr))
		}
	}()

	store, err := New(path, false)
	if err != nil {
		return fmt.Errorf("opening oci store: %v", err)
	}
	defer func() {
		if closeErr := store.Close(context.Background()); closeErr != nil {
			err = errors.Join(err, fmt.Errorf("closing store: %w", closeErr))
		}
	}()
	addAndPush := func(path string, mediaType MediaType) (*v1.Descriptor, error) {
		absPath, err := filepath.Abs(path)
		if err != nil {
			return nil, fmt.Errorf("resolving absolute path for %q: %w", path, err)
		}

		descriptor, err := store.Add(ctx, string(mediaType), absPath)
		if err != nil {
			return nil, fmt.Errorf("adding %q to store: %v", absPath, err)
		}

		return &descriptor, nil
	}

	kernelDescriptor, err := addAndPush(kernelPath, MediaTypeVMKernelImage)
	if err != nil {
		return err
	}
	initrdDescriptor, err := addAndPush(initrdPath, MediaTypeVMInitrdImage)
	if err != nil {
		return err
	}

	configDescriptor, err := store.Set(ctx, Config{
		MediaType: MediaTypeConfigV1,
		Storage: []MediaType{
			MediaTypeVMInitrdImage,
			MediaTypeVMKernelImage,
		},
	})
	if err != nil {
		return fmt.Errorf("adding config descriptor to store: %w", err)
	}

	manifestDescriptor, err := oras.PackManifest(ctx, store, oras.PackManifestVersion1_0, "", oras.PackManifestOptions{
		ConfigDescriptor: &configDescriptor,
		Layers: []v1.Descriptor{
			*initrdDescriptor,
			*kernelDescriptor,
		},
	})
	if err != nil {
		return fmt.Errorf("packing manifest descriptor to store: %w", err)
	}

	if err := store.Tag(ctx, manifestDescriptor, tag); err != nil {
		return fmt.Errorf("tagging manifest descriptor: %w", err)
	}

	if err := store.Archive(ctx, destinationPath, v1.Manifest{
		Config: configDescriptor,
		Layers: []v1.Descriptor{
			*initrdDescriptor,
			*kernelDescriptor,
			manifestDescriptor,
		},
	}); err != nil {
		return fmt.Errorf("generating archive: %w", err)
	}

	return nil
}
