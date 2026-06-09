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

package erofs

import (
	"context"
	"fmt"
	"io"
	"os"
	"path"
	"strings"
	"time"

	"github.com/containerd/log"
	digest "github.com/opencontainers/go-digest"
	ocispec "github.com/opencontainers/image-spec/specs-go/v1"

	"github.com/containerd/containerd/v2/core/content"
	"github.com/containerd/containerd/v2/core/content/index"
	"github.com/containerd/containerd/v2/core/diff"
	"github.com/containerd/containerd/v2/core/images"
	"github.com/containerd/containerd/v2/core/mount"
	"github.com/containerd/containerd/v2/internal/erofsutils"
	"github.com/containerd/containerd/v2/pkg/archive/compression"
	"github.com/containerd/errdefs"

	"github.com/google/uuid"
)

var emptyDesc = ocispec.Descriptor{}

type differ interface {
	diff.Applier
	diff.Comparer
}

// erofsDiff does erofs comparison and application
type erofsDiff struct {
	store content.Store
	// indexStore is optional; when set, lazy-ingest layers are detected and
	// handled by writing a layer.indexed marker instead of extracting bytes.
	indexStore indexStoreInfo
	// enableTarIndex enables generating tar index for tar content
	// instead of fully converting the tar to EROFS format
	enableTarIndex bool
	// enableDmverity enables formatting layers with dm-verity after creation
	enableDmverity bool
}

// indexStoreInfo is the minimal indexed-content store interface the differ needs.
type indexStoreInfo interface {
	Info(ctx context.Context, dgst digest.Digest) (index.Info, error)
}

// DifferOpt is an option for configuring the erofs differ
type DifferOpt func(d *erofsDiff)

// WithMkfsOptions is retained for API compatibility but is now a no-op: the
// pure-Go EROFS implementation does not invoke mkfs.erofs and has no
// equivalent command-line options.
func WithMkfsOptions(_ []string) DifferOpt {
	return func(d *erofsDiff) {}
}

// WithTarIndexMode enables tar index mode for EROFS layers
func WithTarIndexMode() DifferOpt {
	return func(d *erofsDiff) {
		d.enableTarIndex = true
	}
}

// WithDmverity enables dm-verity formatting for EROFS layers
func WithDmverity() DifferOpt {
	return func(d *erofsDiff) {
		d.enableDmverity = true
	}
}

// WithIndexStore sets the indexed content store on the differ. When set, lazy
// layers (those with a chunk-index annotation AND a metadata record in the
// indexed content store) are handled by writing a layer.indexed marker
// instead of extracting bytes. This avoids re-downloading blobs that were
// already lazily ingested during pull.
func WithIndexStore(is indexStoreInfo) DifferOpt {
	return func(d *erofsDiff) {
		d.indexStore = is
	}
}

// NewErofsDiffer creates a new EROFS differ with the provided options
func NewErofsDiffer(store content.Store, opts ...DifferOpt) differ {
	d := &erofsDiff{store: store}
	for _, opt := range opts {
		opt(d)
	}
	return d
}

func (s erofsDiff) Apply(ctx context.Context, desc ocispec.Descriptor, mounts []mount.Mount, opts ...diff.ApplyOpt) (d ocispec.Descriptor, err error) {
	t1 := time.Now()
	defer func() {
		if err == nil {
			log.G(ctx).WithFields(log.Fields{
				"d":      time.Since(t1),
				"digest": desc.Digest,
				"size":   desc.Size,
				"media":  desc.MediaType,
			}).Debugf("diff applied")
		}
	}()

	// ── Handle merged EROFS and raw-device-role layer variants first ────────
	//
	// Merged layers (application/vnd.erofs.layer.merged.v1[+zstd]) are
	// metadata-only EROFS images; they are stored as fsmeta.erofs in the
	// snapshot directory so the existing mountFsMeta path picks them up.
	//
	// Layers carrying the org.erofs.role=device descriptor annotation
	// (erofs-image-spec §7, §10.2) are opaque-byte blobs used as raw EROFS
	// data devices by a subsequent EROFS layer.  Their media type is
	// unrestricted — any +zstd/+gzip-suffixed media type or an
	// uncompressed binary blob works — and only the annotation
	// determines the role.  They are stored as device.<N>.raw where N is
	// the chain position (number of parent IDs at apply time), and the
	// snapshotter appends device= options for each such file at mount
	// time.
	mediaType := desc.MediaType
	role, hasRole := desc.Annotations[index.AnnotationRole]
	if hasRole && role != index.RoleDevice {
		// Per erofs-image-spec §7, an unknown role value MUST cause the
		// consumer to refuse to apply the image.  RoleOverlayData is
		// reserved but not yet defined.
		return emptyDesc, fmt.Errorf("unsupported %s annotation value %q on layer %s", index.AnnotationRole, role, desc.Digest)
	}
	isDeviceRole := hasRole && role == index.RoleDevice
	if isMergedErofsMediaType(mediaType) || isDeviceRole {
		layer, err := erofsutils.MountsToLayer(mounts)
		if err != nil {
			return emptyDesc, err
		}
		ra, err := s.store.ReaderAt(ctx, desc)
		if err != nil {
			return emptyDesc, fmt.Errorf("failed to get reader from content store: %w", err)
		}
		defer ra.Close()

		// Determine the destination path.
		var destPath string
		if isMergedErofsMediaType(mediaType) {
			destPath = path.Join(layer, "fsmeta.erofs")
		} else {
			// Device-role layer: use chain position as the device index.
			chainPos := chainPositionFromMounts(mounts)
			destPath = path.Join(layer, fmt.Sprintf("device.%d.raw", chainPos))
		}

		// Auto-detect compression from the stream magic bytes rather
		// than the media type, since device-role layers may carry any
		// OCI media type (tar+zstd, octet-stream, +gzip, …) and the
		// EROFS spec does not constrain the wrapper format.
		reader, err := compression.DecompressStream(content.NewReader(ra))
		if err != nil {
			return emptyDesc, fmt.Errorf("failed to decompress %s: %w", mediaType, err)
		}
		defer reader.Close()

		f, err := os.Create(destPath)
		if err != nil {
			return emptyDesc, fmt.Errorf("failed to create %s: %w", destPath, err)
		}
		digester := digest.Canonical.Digester()
		if _, err := io.Copy(f, io.TeeReader(reader, digester.Hash())); err != nil {
			f.Close()
			return emptyDesc, fmt.Errorf("failed to write %s: %w", destPath, err)
		}
		fi, _ := f.Stat()
		f.Close()

		log.G(ctx).WithFields(log.Fields{
			"path":  destPath,
			"role":  role,
			"media": mediaType,
		}).Debugf("applied %s layer", mediaType)
		return ocispec.Descriptor{
			MediaType: mediaType,
			Digest:    desc.Digest, // preserve original blob digest
			Size:      fi.Size(),
		}, nil
	}
	// ── End merged / device-role handling ───────────────────────────────────

	var (
		erofsLayerType string
		fastcopy       bool
	)
	diffLayerType := desc.MediaType
	native := erofsutils.IsErofsMediaType(diffLayerType)
	if native {
		base, ext, hasExt := strings.Cut(diffLayerType, "+")
		// Mimic the OCI layer for EROFS blobs for diff.NewProcessorChain(), so
		// there is no need to bother with too much unrelated logic for now.
		diffLayerType = ocispec.MediaTypeImageLayer
		if hasExt {
			// `+zstd` indicates that the original EROFS blob is additionally
			// compressed with standard zstd streams.
			// Only `+zstd` is considered since it is more performant than gzip
			// and has useful features like skippable frames.
			if ext != "zstd" {
				return emptyDesc, fmt.Errorf("unsupported erofs layer suffix: %s", ext)
			}
			diffLayerType = diffLayerType + "+zstd"
		} else {
			fastcopy = true
		}
		erofsLayerType = base
	} else {
		// Non-EROFS layer (tar+gzip, tar+zstd, etc.). Return ErrNotImplemented
		// so the diff service can fall through to the next differ in order
		// (typically the walking differ for tar extraction). The EROFS differ
		// does not handle tar layers in the dispatch chain — ConvertTarErofs
		// is only used when the EROFS differ is called directly (e.g. from the
		// transfer service with an explicit differ="erofs" configuration).
		if _, cerr := images.DiffCompression(ctx, diffLayerType); cerr == nil {
			return emptyDesc, fmt.Errorf("erofs differ: non-native layer %s: %w", desc.MediaType, errdefs.ErrNotImplemented)
		}
		return emptyDesc, fmt.Errorf("unsupported media type: %s", desc.MediaType)
	}

	var config diff.ApplyConfig
	for _, o := range opts {
		if err := o(ctx, desc, &config); err != nil {
			return emptyDesc, fmt.Errorf("failed to apply config opt: %w", err)
		}
	}

	layer, err := erofsutils.MountsToLayer(mounts)
	if err != nil {
		return emptyDesc, err
	}

	// ── Lazy-ingest detection ─────────────────────────────────────────────────
	// If the layer carries a chunk-index annotation AND the indexed content
	// store already has a metadata record for this blob (meaning the transfer
	// service performed a lazy ingest during pull), write layer.indexed and
	// return without touching the content store bytes.
	if s.isLazyLayer(ctx, desc) {
		markerPath := path.Join(layer, "layer.indexed")
		if err := os.WriteFile(markerPath, []byte(desc.Digest.String()), 0644); err != nil {
			return emptyDesc, fmt.Errorf("erofs differ: write layer.indexed: %w", err)
		}
		log.G(ctx).WithFields(log.Fields{
			"digest": desc.Digest,
			"path":   markerPath,
		}).Debug("lazy layer detected; wrote layer.indexed marker")
		return desc, nil
	}
	// ── End lazy-ingest detection ─────────────────────────────────────────────

	ra, err := s.store.ReaderAt(ctx, desc)
	if err != nil {
		return emptyDesc, fmt.Errorf("failed to get reader from content store: %w", err)
	}
	defer ra.Close()

	layerBlobPath := path.Join(layer, "layer.erofs")
	// Allow copy file range when there is an uncompressed native EROFS layer
	if fastcopy {
		f, err := os.Create(layerBlobPath)
		if err != nil {
			return emptyDesc, err
		}
		_, err = io.Copy(f, content.NewReader(ra))
		f.Close()
		if err != nil {
			return emptyDesc, err
		}
		log.G(ctx).WithField("path", layerBlobPath).Debug("Applied layer with uncompressed EROFS blob")
		return desc, nil
	}

	processor := diff.NewProcessorChain(diffLayerType, content.NewReader(ra))
	for {
		if processor, err = diff.GetProcessor(ctx, processor, config.ProcessorPayloads); err != nil {
			return emptyDesc, fmt.Errorf("failed to get stream processor for %s: %w", desc.MediaType, err)
		}
		if processor.MediaType() == ocispec.MediaTypeImageLayer {
			break
		}
	}
	defer processor.Close()

	digester := digest.Canonical.Digester()
	rc := &readCounter{
		r: io.TeeReader(processor, digester.Hash()),
	}

	// Choose between tar index or tar conversion mode
	// Generate deterministic UUID from layer digest
	u := uuid.NewSHA1(uuid.NameSpaceURL, []byte("erofs:blobs/"+desc.Digest))
	if native {
		f, err := os.Create(layerBlobPath)
		if err != nil {
			return emptyDesc, err
		}
		_, err = io.Copy(f, rc)
		f.Close()
		if err != nil {
			return emptyDesc, err
		}
		log.G(ctx).WithField("path", layerBlobPath).Debug("Applied layer with compressed EROFS blob")
	} else if s.enableTarIndex {
		// Use the tar index method: generate tar index and append tar.
		// Pure-Go implementation via go-erofs (no external process).
		err = erofsutils.GenerateTarIndexAndAppendTarGo(ctx, rc, layerBlobPath, u.String())
		if err != nil {
			return emptyDesc, fmt.Errorf("failed to generate tar index: %w", err)
		}
		log.G(ctx).WithField("path", layerBlobPath).Debug("Applied layer using tar index mode (Go)")
	} else {
		// Full tar-to-EROFS conversion using pure-Go implementation.
		err = erofsutils.ConvertTarErofsGo(ctx, rc, layerBlobPath, u.String())
		if err != nil {
			return emptyDesc, fmt.Errorf("failed to convert tar to erofs: %w", err)
		}
		log.G(ctx).WithField("path", layerBlobPath).Debug("Applied layer using tar conversion mode (Go)")
	}

	// Read any trailing data
	if _, err := io.Copy(io.Discard, rc); err != nil {
		return emptyDesc, err
	}

	// Format with dm-verity if enabled
	if s.enableDmverity {
		if err := s.formatDmverityLayer(ctx, layerBlobPath); err != nil {
			return emptyDesc, fmt.Errorf("failed to format dm-verity layer: %w", err)
		}
	}

	if native {
		return ocispec.Descriptor{
			MediaType: erofsLayerType,
			Size:      rc.c,
			Digest:    digester.Digest(),
		}, nil
	}
	return ocispec.Descriptor{
		MediaType: ocispec.MediaTypeImageLayer,
		Size:      rc.c,
		Digest:    digester.Digest(),
	}, nil
}

// isLazyLayer returns true when desc carries a chunk-index annotation AND the
// indexed content store (if configured) already has a metadata record for the
// blob. Both conditions must hold: the annotation signals the blob was produced
// with a chunk index; the index-store record confirms the transfer service
// completed a lazy ingest so the bytes are not in the regular content store.
func (s erofsDiff) isLazyLayer(ctx context.Context, desc ocispec.Descriptor) bool {
	if s.indexStore == nil {
		return false
	}
	if desc.Annotations[index.AnnotationChunkIndexRange] == "" {
		return false
	}
	// Check if the indexed content store has a record.
	if _, err := s.indexStore.Info(ctx, desc.Digest); err != nil {
		return false
	}
	return true
}

// isMergedErofsMediaType reports whether mt is a merged EROFS layer type.
func isMergedErofsMediaType(mt string) bool {
	return mt == "application/vnd.erofs.layer.merged.v1" ||
		mt == "application/vnd.erofs.layer.merged.v1+zstd"
}

// chainPositionFromMounts returns the number of lower EROFS layers already
// in the mount stack, used to derive a unique data-blob filename.
// Each EROFS lower dir in the overlay is one "device slot" for data layers.
func chainPositionFromMounts(mounts []mount.Mount) int {
	n := 0
	for _, m := range mounts {
		for _, opt := range m.Options {
			if strings.HasPrefix(opt, "lowerdir=") || strings.HasPrefix(opt, "device=") {
				n++
			}
		}
	}
	return n
}

type readCounter struct {
	r io.Reader
	c int64
}

func (rc *readCounter) Read(p []byte) (n int, err error) {
	n, err = rc.r.Read(p)
	rc.c += int64(n)
	return
}
