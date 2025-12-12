package oci

import (
	"k8s.io/apimachinery/pkg/util/sets"
)

// MediaType represents a media type.
type MediaType string

const (
	MediaTypeVMInitrdImage MediaType = "application/vnd.vm.initrd.image.v1"
	MediaTypeVMKernelImage MediaType = "application/vnd.vm.kernel.image.v1"
	MediaTypeConfigV1      MediaType = "application/vnd.vm.config.v1+json"
)

// mediaTypeToTitle maps media types to their titles.
var mediaTypeToTitle = map[MediaType]string{
	MediaTypeConfigV1:      "config.json",
	MediaTypeVMInitrdImage: "initrd.img",
	MediaTypeVMKernelImage: "kernel",
}

// Title returns the title of the media type.
func (mt MediaType) Title() string {
	return mediaTypeToTitle[mt]
}

// supportedMediaTypes contains the supported media types.
var supportedMediaTypes = sets.NewString(
	string(MediaTypeConfigV1),
	string(MediaTypeVMInitrdImage),
	string(MediaTypeVMKernelImage),
)

// IsMediaTypeSupported checks if the media type is supported.
func IsMediaTypeSupported(mediaType string) bool {
	return supportedMediaTypes.Has(mediaType)
}
