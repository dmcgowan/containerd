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
	"strconv"
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
	"github.com/containerd/containerd/v2/internal/dmverity"
	"github.com/containerd/containerd/v2/internal/erofsutils"
	"github.com/containerd/containerd/v2/pkg/archive/compression"

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
		// Non-EROFS layer (tar+gzip, tar+zstd, etc.).
		// When this differ is used as the explicit Applier (e.g. differ="erofs"
		// in the transfer service unpack config, or the diff service order puts
		// erofs first), convert the tar stream to an EROFS blob directly via
		// ConvertTarErofs / GenerateTarIndexAndAppendTar below.  diffLayerType
		// is already the correct OCI media type for GetProcessor to decompress.
		// For truly unrecognised media types that DiffCompression rejects, bail
		// out — there is nothing we can do.
		if _, cerr := images.DiffCompression(ctx, diffLayerType); cerr != nil {
			return emptyDesc, fmt.Errorf("unsupported media type: %s", desc.MediaType)
		}
		// native remains false; erofsLayerType/fastcopy stay at zero values.
		// Execution continues to the GetProcessor + ConvertTarErofs path below.
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
		// If the descriptor carries org.erofs.dmverity.* annotations,
		// persist a parallel layer.dmverity sidecar so the snapshotter
		// can thread the verity params into the block-mount options.
		// The merkle tree is already inside the blob bytes (appended
		// after the EROFS image, before the chunk-index trailer); the
		// sidecar carries only what's needed to set up the verity
		// device at mount: root_digest, hash_offset, block_size.
		if err := writeLazyDmverityMarker(ctx, layer, desc); err != nil {
			return emptyDesc, fmt.Errorf("erofs differ: write layer.dmverity: %w", err)
		}
		// Return the uncompressed digest so the unpacker's diff-id check passes.
		// The compressed-blob digest is preserved in the layer.indexed marker.
		uncompDgst := desc.Digest
		if v, ok := desc.Annotations[index.AnnotationUncompressedDigest]; ok && v != "" {
			if d, err := digest.Parse(v); err == nil {
				uncompDgst = d
			}
		}
		log.G(ctx).WithFields(log.Fields{
			"digest":     desc.Digest,
			"uncomp":     uncompDgst,
			"path":       markerPath,
		}).Debug("lazy layer detected; wrote layer.indexed marker")
		return ocispec.Descriptor{
			MediaType: desc.MediaType,
			Digest:    uncompDgst,
			Size:      desc.Size,
		}, nil
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
		// Persist convert-time dm-verity annotations as an eager
		// sidecar so the EROFS mount plugin can activate the verity
		// device on mount.  No-op when the descriptor lacks the
		// org.erofs.dmverity.* annotations.  Skipped when the differ
		// is configured to format verity itself (s.enableDmverity), as
		// that path writes its own sidecar from a freshly-built tree.
		if !s.enableDmverity {
			if err := writeEagerDmverityMarker(ctx, layerBlobPath, desc); err != nil {
				return emptyDesc, fmt.Errorf("erofs differ: write eager layer.erofs.dmverity: %w", err)
			}
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
		err = erofsutils.GenerateTarIndexAndAppendTar(ctx, rc, layerBlobPath, u.String())
		if err != nil {
			return emptyDesc, fmt.Errorf("failed to generate tar index: %w", err)
		}
		log.G(ctx).WithField("path", layerBlobPath).Debug("Applied layer using tar index mode")
	} else {
		// Full tar-to-EROFS conversion using pure-Go implementation.
		err = erofsutils.ConvertTarErofs(ctx, rc, layerBlobPath, u.String())
		if err != nil {
			return emptyDesc, fmt.Errorf("failed to convert tar to erofs: %w", err)
		}
		log.G(ctx).WithField("path", layerBlobPath).Debug("Applied layer using tar conversion mode")
	}

	// Read any trailing data
	if _, err := io.Copy(io.Discard, rc); err != nil {
		return emptyDesc, err
	}

	// Format with dm-verity if the differ is configured to build the
	// hash tree itself.  Otherwise, if this was a native EROFS source
	// (raw or +zstd) and the descriptor carries org.erofs.dmverity.*
	// annotations, persist the convert-time verity params as a sidecar
	// so the mount plugin can set up the verity device.  The merkle
	// tree is already present on disk inside layer.erofs at the
	// annotation's hash_offset (it was preserved verbatim by the
	// fastcopy or processor-chain decompression above).
	if s.enableDmverity {
		if err := s.formatDmverityLayer(ctx, layerBlobPath); err != nil {
			return emptyDesc, fmt.Errorf("failed to format dm-verity layer: %w", err)
		}
	} else if native {
		if err := writeEagerDmverityMarker(ctx, layerBlobPath, desc); err != nil {
			return emptyDesc, fmt.Errorf("erofs differ: write eager layer.erofs.dmverity: %w", err)
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

// dmverityMetaFromAnnotations parses the org.erofs.dmverity.* annotations
// off the descriptor.  Returns (nil, false) when the layer is not a
// dm-verity-enabled blob (i.e. either annotation is missing or the
// hash-offset annotation is malformed — in which case the caller is
// expected to proceed without verity).  Malformed values are logged.
func dmverityMetaFromAnnotations(ctx context.Context, desc ocispec.Descriptor) (*dmverity.DmverityMetadata, bool) {
	if desc.Annotations == nil {
		return nil, false
	}
	rootDigest := desc.Annotations[index.AnnotationDmVerityRootDigest]
	hashOffsetStr := desc.Annotations[index.AnnotationDmVerityHashOffset]
	if rootDigest == "" || hashOffsetStr == "" {
		return nil, false
	}
	hashOffset, err := strconv.ParseUint(hashOffsetStr, 10, 64)
	if err != nil {
		log.G(ctx).WithError(err).WithFields(log.Fields{
			"digest":     desc.Digest,
			"annotation": index.AnnotationDmVerityHashOffset,
			"value":      hashOffsetStr,
		}).Warn("erofs differ: ignoring malformed dm-verity hash_offset annotation; mount will proceed without verification")
		return nil, false
	}
	blockSize := uint32(0) // 0 means "use default 4096" downstream
	if v := desc.Annotations[index.AnnotationDmVerityBlockSize]; v != "" {
		if n, err := strconv.ParseUint(v, 10, 32); err == nil {
			blockSize = uint32(n)
		}
	}
	return &dmverity.DmverityMetadata{
		RootHash:   rootDigest,
		HashOffset: hashOffset,
		BlockSize:  blockSize,
	}, true
}

// writeLazyDmverityMarker writes a layer.dmverity sidecar containing
// the dm-verity parameters needed to set up a verity device at mount,
// IFF the descriptor carries the org.erofs.dmverity.root_digest +
// org.erofs.dmverity.hash_offset annotations.  Missing or
// unparseable annotations are NOT a hard error: the lazy ingest is
// allowed to proceed without verity (matching today's behaviour).
// Hard-fail policy is enforced LATER at mount time — the block
// handler refuses to mount when verity params are present but the
// kernel can't honour them.
//
// The sidecar lives at <layer>/layer.dmverity, parallel to
// layer.indexed, so the snapshotter's lazy block-mount path can pick
// it up alongside.
func writeLazyDmverityMarker(ctx context.Context, layer string, desc ocispec.Descriptor) error {
	meta, ok := dmverityMetaFromAnnotations(ctx, desc)
	if !ok {
		return nil
	}
	markerPath := path.Join(layer, "layer.dmverity")
	if err := dmverity.WriteMetadata(markerPath, meta); err != nil {
		return err
	}
	log.G(ctx).WithFields(log.Fields{
		"digest":      desc.Digest,
		"root_digest": meta.RootHash,
		"hash_offset": meta.HashOffset,
		"block_size":  meta.EffectiveBlockSize(),
		"path":        markerPath,
	}).Debug("lazy layer: wrote layer.dmverity sidecar")
	return nil
}

// writeEagerDmverityMarker writes the dm-verity sidecar for an eagerly-
// applied EROFS layer (raw or +zstd that was unpacked into layer.erofs).
// The sidecar lives at MetadataPath(layerBlobPath), i.e.
// `<layer>/layer.erofs.dmverity`, which is exactly the path the regular
// EROFS mount plugin reads to drive setupDmVerityDevice — so this
// turns convert-produced annotations into a mount-active verity device
// without needing any further plumbing.
//
// Like the lazy variant, missing/unparseable annotations are not a
// hard error: layers without verity simply proceed without it.  The
// hard-fail policy lives at mount time.
func writeEagerDmverityMarker(ctx context.Context, layerBlobPath string, desc ocispec.Descriptor) error {
	meta, ok := dmverityMetaFromAnnotations(ctx, desc)
	if !ok {
		return nil
	}
	markerPath := dmverity.MetadataPath(layerBlobPath)
	if err := dmverity.WriteMetadata(markerPath, meta); err != nil {
		return err
	}
	log.G(ctx).WithFields(log.Fields{
		"digest":      desc.Digest,
		"root_digest": meta.RootHash,
		"hash_offset": meta.HashOffset,
		"block_size":  meta.EffectiveBlockSize(),
		"path":        markerPath,
	}).Debug("eager layer: wrote layer.erofs.dmverity sidecar")
	return nil
}
