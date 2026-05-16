/*
   Copyright The containerd Authors.

   Licensed under the Apache License, Version 2.0 (the "License");
   you may not use this file except in compliance with the License.
   You may obtain a copy of the License at

       http://www.apache.org/licenses/LICENSE-2.0

   Unless required by applicable law or agreed to in writing, software
   distributed under the License is distributed on an "AS IS" BASIS,
   WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
   See the License for the specific language governing permissions and
   limitations under the License.
*/

// Package converter provides image converter
package converter

import (
	"context"

	"github.com/containerd/containerd/v2/core/content"
	"github.com/containerd/containerd/v2/core/images"
	"github.com/containerd/containerd/v2/core/leases"
	"github.com/containerd/platforms"
	"github.com/opencontainers/go-digest"
)

type convertOpts struct {
	layerConvertFunc   ConvertFunc
	docker2oci         bool
	indexConvertFunc   ConvertFunc
	platformMC         platforms.MatchComparer
	updateManifestFunc UpdateManifestFunc
	appendToIndex      bool
}

// Opt is an option for Convert()
type Opt func(*convertOpts) error

// WithLayerConvertFunc specifies the function that converts layers.
func WithLayerConvertFunc(fn ConvertFunc) Opt {
	return func(copts *convertOpts) error {
		copts.layerConvertFunc = fn
		return nil
	}
}

// WithDockerToOCI converts Docker media types into OCI ones.
func WithDockerToOCI(v bool) Opt {
	return func(copts *convertOpts) error {
		copts.docker2oci = true
		return nil
	}
}

// WithPlatform specifies the platform.
// Defaults to all platforms.
func WithPlatform(p platforms.MatchComparer) Opt {
	return func(copts *convertOpts) error {
		copts.platformMC = p
		return nil
	}
}

// WithIndexConvertFunc specifies the function that converts manifests and index (manifest lists).
// Defaults to DefaultIndexConvertFunc.
func WithIndexConvertFunc(fn ConvertFunc) Opt {
	return func(copts *convertOpts) error {
		copts.indexConvertFunc = fn
		return nil
	}
}

// WithUpdateManifest specifies a callback that is invoked after manifest
// conversion.
func WithUpdateManifest(fn UpdateManifestFunc) Opt {
	return func(copts *convertOpts) error {
		copts.updateManifestFunc = fn
		return nil
	}
}

// WithAppendToIndex causes Convert to build a dual-format OCI image index
// that contains both the original manifests and the newly converted ones,
// rather than replacing the original manifests.
//
// The result satisfies the ordering requirement in the EROFS image spec:
// original (tar-based) manifests appear first in the index, converted
// (EROFS) manifests follow with os.features=["erofs"] on each platform
// descriptor so non-EROFS-aware runtimes automatically select the first
// matching tar manifest and ignore the EROFS entries.
//
// When the source image is a single manifest (not an index) the function
// promotes it to an OCI image index that contains the original manifest
// followed by the converted manifest.
//
// When the source image is already an index, original manifests are
// retained in their original order and converted manifests are appended.
// Manifests that do not match the platform filter (when WithPlatform is
// also set) are kept in the original section but are not converted.
//
// WithAppendToIndex is typically combined with WithLayerConvertFunc and
// WithUpdateManifest.  Example usage with EROFS chunked conversion:
//
//	converter.Convert(ctx, client, dstRef, srcRef,
//	    converter.WithLayerConvertFunc(erofs.LayerConvertFuncChunked(idxStore, 0)),
//	    converter.WithUpdateManifest(erofs.UpdateManifestPlatform),
//	    converter.WithAppendToIndex(),
//	)
func WithAppendToIndex() Opt {
	return func(copts *convertOpts) error {
		copts.appendToIndex = true
		return nil
	}
}

// Client is implemented by *containerd.Client .
type Client interface {
	WithLease(ctx context.Context, opts ...leases.Opt) (context.Context, func(context.Context) error, error)
	ContentStore() content.Store
	ImageService() images.Store
}

// Convert converts an image.
func Convert(ctx context.Context, client Client, dstRef, srcRef string, opts ...Opt) (*images.Image, error) {
	var copts convertOpts
	for _, o := range opts {
		if err := o(&copts); err != nil {
			return nil, err
		}
	}
	if copts.platformMC == nil {
		copts.platformMC = platforms.All
	}

	// appendToIndex and indexConvertFunc are mutually exclusive: if the caller
	// already provided a custom indexConvertFunc, respect it.
	if copts.indexConvertFunc == nil {
		if copts.appendToIndex {
			copts.indexConvertFunc = AppendIndexConvertFunc(
				copts.layerConvertFunc,
				copts.docker2oci,
				copts.platformMC,
				copts.updateManifestFunc,
			)
		} else if copts.updateManifestFunc != nil {
			c := &defaultConverter{
				layerConvertFunc:   copts.layerConvertFunc,
				docker2oci:         copts.docker2oci,
				platformMC:         copts.platformMC,
				diffIDMap:          make(map[digest.Digest]digest.Digest),
				updateManifestFunc: copts.updateManifestFunc,
			}
			copts.indexConvertFunc = c.convert
		} else {
			copts.indexConvertFunc = DefaultIndexConvertFunc(copts.layerConvertFunc, copts.docker2oci, copts.platformMC)
		}
	}

	ctx, done, err := client.WithLease(ctx)
	if err != nil {
		return nil, err
	}
	defer done(ctx)

	cs := client.ContentStore()
	is := client.ImageService()
	srcImg, err := is.Get(ctx, srcRef)
	if err != nil {
		return nil, err
	}

	dstDesc, err := copts.indexConvertFunc(ctx, cs, srcImg.Target)
	if err != nil {
		return nil, err
	}

	dstImg := srcImg
	dstImg.Name = dstRef
	if dstDesc != nil {
		dstImg.Target = *dstDesc
	}
	var res images.Image
	if dstRef != srcRef {
		_ = is.Delete(ctx, dstRef)
		res, err = is.Create(ctx, dstImg)
	} else {
		res, err = is.Update(ctx, dstImg)
	}
	return &res, err
}
