// corten-matrix - A Matrix-iMessage puppeting bridge.
// Copyright (C) 2024 Ludvig Rhodin
//
// This program is free software: you can redistribute it and/or modify
// it under the terms of the GNU Affero General Public License as published by
// the Free Software Foundation, either version 3 of the License, or
// (at your option) any later version.

package connector

/*
#cgo linux LDFLAGS: -ldl
#include <stdlib.h>
#include <string.h>
#include <dlfcn.h>
#include <libheif/heif.h>

// libheif is loaded at RUNTIME via dlopen — it is NOT linked at build time.
// This keeps it out of the redistributed binary (it's LGPL + patent-encumbered
// codecs), lets the binary build with zero libheif dependency (so it cross-builds
// cleanly), and lets the binary RUN before libheif exists so `setup` can install
// it via the system package manager. HEIC features are simply disabled until then.
//
// We keep <libheif/heif.h> for the types/enums but redirect every function call
// to a dlopen'd pointer (via the macros below), so nothing links against -lheif.
#define HEIF_DECL(n) static __typeof__(n)* p_##n = 0;
HEIF_DECL(heif_context_alloc) HEIF_DECL(heif_context_read_from_memory) HEIF_DECL(heif_context_free)
HEIF_DECL(heif_context_get_primary_image_handle) HEIF_DECL(heif_context_get_number_of_top_level_images)
HEIF_DECL(heif_image_handle_get_number_of_metadata_blocks) HEIF_DECL(heif_image_handle_get_list_of_metadata_block_IDs)
HEIF_DECL(heif_image_handle_get_metadata_size) HEIF_DECL(heif_image_handle_get_metadata)
HEIF_DECL(heif_image_handle_get_raw_color_profile_size) HEIF_DECL(heif_image_handle_get_raw_color_profile)
HEIF_DECL(heif_image_handle_get_metadata_content_type) HEIF_DECL(heif_image_handle_release)
HEIF_DECL(heif_decode_image) HEIF_DECL(heif_image_get_width) HEIF_DECL(heif_image_get_height)
HEIF_DECL(heif_image_get_plane_readonly) HEIF_DECL(heif_image_release)
#undef HEIF_DECL

static int cr_heif_state = 0; // 0=unloaded, 1=ok, -1=failed
static int cr_heif_load(void) {
	if (cr_heif_state != 0) return cr_heif_state > 0;
	void* h = dlopen("libheif.so.1", RTLD_NOW | RTLD_GLOBAL);
	if (!h) h = dlopen("libheif.1.dylib", RTLD_NOW | RTLD_GLOBAL);
	if (!h) h = dlopen("libheif.dylib", RTLD_NOW | RTLD_GLOBAL);
	if (!h) h = dlopen("libheif.so", RTLD_NOW | RTLD_GLOBAL);
	if (!h) { cr_heif_state = -1; return 0; }
#define HEIF_LOAD(n) p_##n = (__typeof__(n)*)dlsym(h, #n); if (!p_##n) { cr_heif_state = -1; return 0; }
	HEIF_LOAD(heif_context_alloc) HEIF_LOAD(heif_context_read_from_memory) HEIF_LOAD(heif_context_free)
	HEIF_LOAD(heif_context_get_primary_image_handle) HEIF_LOAD(heif_context_get_number_of_top_level_images)
	HEIF_LOAD(heif_image_handle_get_number_of_metadata_blocks) HEIF_LOAD(heif_image_handle_get_list_of_metadata_block_IDs)
	HEIF_LOAD(heif_image_handle_get_metadata_size) HEIF_LOAD(heif_image_handle_get_metadata)
	HEIF_LOAD(heif_image_handle_get_raw_color_profile_size) HEIF_LOAD(heif_image_handle_get_raw_color_profile)
	HEIF_LOAD(heif_image_handle_get_metadata_content_type) HEIF_LOAD(heif_image_handle_release)
	HEIF_LOAD(heif_decode_image) HEIF_LOAD(heif_image_get_width) HEIF_LOAD(heif_image_get_height)
	HEIF_LOAD(heif_image_get_plane_readonly) HEIF_LOAD(heif_image_release)
#undef HEIF_LOAD
	cr_heif_state = 1;
	return 1;
}

// Redirect the call sites below to the dlopen'd pointers (no -lheif link).
#define heif_context_alloc p_heif_context_alloc
#define heif_context_read_from_memory p_heif_context_read_from_memory
#define heif_context_free p_heif_context_free
#define heif_context_get_primary_image_handle p_heif_context_get_primary_image_handle
#define heif_context_get_number_of_top_level_images p_heif_context_get_number_of_top_level_images
#define heif_image_handle_get_number_of_metadata_blocks p_heif_image_handle_get_number_of_metadata_blocks
#define heif_image_handle_get_list_of_metadata_block_IDs p_heif_image_handle_get_list_of_metadata_block_IDs
#define heif_image_handle_get_metadata_size p_heif_image_handle_get_metadata_size
#define heif_image_handle_get_metadata p_heif_image_handle_get_metadata
#define heif_image_handle_get_raw_color_profile_size p_heif_image_handle_get_raw_color_profile_size
#define heif_image_handle_get_raw_color_profile p_heif_image_handle_get_raw_color_profile
#define heif_image_handle_get_metadata_content_type p_heif_image_handle_get_metadata_content_type
#define heif_image_handle_release p_heif_image_handle_release
#define heif_decode_image p_heif_decode_image
#define heif_image_get_width p_heif_image_get_width
#define heif_image_get_height p_heif_image_get_height
#define heif_image_get_plane_readonly p_heif_image_get_plane_readonly
#define heif_image_release p_heif_image_release

// extractAllMetadataFromHEIC extracts EXIF, ICC color profile, and XMP metadata
// from HEIC data in a single parse pass. Callers must free any non-NULL output
// pointers.
static void extractAllMetadataFromHEIC(
	const void* data, size_t data_len,
	unsigned char** exif_out, size_t* exif_len,
	unsigned char** icc_out, size_t* icc_len,
	unsigned char** xmp_out, size_t* xmp_len)
{
	*exif_out = NULL; *exif_len = 0;
	*icc_out  = NULL; *icc_len  = 0;
	*xmp_out  = NULL; *xmp_len  = 0;

	if (!cr_heif_load()) return;  // load libheif before any heif_* call (NULL ptr otherwise)
	struct heif_context* ctx = heif_context_alloc();
	if (!ctx) return;

	struct heif_error err = heif_context_read_from_memory(ctx, data, data_len, NULL);
	if (err.code != heif_error_Ok) {
		heif_context_free(ctx);
		return;
	}

	struct heif_image_handle* handle = NULL;
	err = heif_context_get_primary_image_handle(ctx, &handle);
	if (err.code != heif_error_Ok) {
		heif_context_free(ctx);
		return;
	}

	// ── EXIF ────────────────────────────────────────────────────
	int n = heif_image_handle_get_number_of_metadata_blocks(handle, "Exif");
	if (n > 0) {
		heif_item_id exif_id;
		heif_image_handle_get_list_of_metadata_block_IDs(handle, "Exif", &exif_id, 1);
		size_t exif_size = heif_image_handle_get_metadata_size(handle, exif_id);
		if (exif_size > 4) {
			unsigned char* raw = (unsigned char*)malloc(exif_size);
			if (raw) {
				err = heif_image_handle_get_metadata(handle, exif_id, raw);
				if (err.code == heif_error_Ok) {
					// The first 4 bytes are a big-endian offset to the TIFF header.
					// Per libheif docs, skip them to get the raw TIFF/EXIF data.
					uint32_t tiff_offset = ((uint32_t)raw[0] << 24) |
					                       ((uint32_t)raw[1] << 16) |
					                       ((uint32_t)raw[2] << 8)  |
					                       (uint32_t)raw[3];
					size_t skip = 4 + tiff_offset;
					if (skip < exif_size) {
						size_t tiff_len = exif_size - skip;
						*exif_out = (unsigned char*)malloc(tiff_len);
						if (*exif_out) {
							memcpy(*exif_out, raw + skip, tiff_len);
							*exif_len = tiff_len;
						}
					}
				}
				free(raw);
			}
		}
	}

	// ── ICC color profile ───────────────────────────────────────
	size_t icc_size = heif_image_handle_get_raw_color_profile_size(handle);
	if (icc_size > 0) {
		*icc_out = (unsigned char*)malloc(icc_size);
		if (*icc_out) {
			err = heif_image_handle_get_raw_color_profile(handle, *icc_out);
			if (err.code == heif_error_Ok) {
				*icc_len = icc_size;
			} else {
				free(*icc_out);
				*icc_out = NULL;
			}
		}
	}

	// ── XMP (stored as "mime" metadata with XML content type) ───
	int mn = heif_image_handle_get_number_of_metadata_blocks(handle, "mime");
	if (mn > 0) {
		heif_item_id* ids = (heif_item_id*)malloc(mn * sizeof(heif_item_id));
		if (ids) {
			heif_image_handle_get_list_of_metadata_block_IDs(handle, "mime", ids, mn);
			for (int i = 0; i < mn; i++) {
				const char* ct = heif_image_handle_get_metadata_content_type(handle, ids[i]);
				if (ct && strstr(ct, "xml")) {
					size_t xmp_size = heif_image_handle_get_metadata_size(handle, ids[i]);
					if (xmp_size > 0) {
						*xmp_out = (unsigned char*)malloc(xmp_size);
						if (*xmp_out) {
							err = heif_image_handle_get_metadata(handle, ids[i], *xmp_out);
							if (err.code == heif_error_Ok) {
								*xmp_len = xmp_size;
							} else {
								free(*xmp_out);
								*xmp_out = NULL;
							}
						}
					}
					break;
				}
			}
			free(ids);
		}
	}

	heif_image_handle_release(handle);
	heif_context_free(ctx);
}

// getTopLevelImageCount returns the number of top-level images in the HEIC.
static int getTopLevelImageCount(const void* data, size_t data_len) {
	if (!cr_heif_load()) return -1;  // load libheif before any heif_* call (NULL ptr otherwise)
	struct heif_context* ctx = heif_context_alloc();
	if (!ctx) return -1;

	struct heif_error err = heif_context_read_from_memory(ctx, data, data_len, NULL);
	if (err.code != heif_error_Ok) {
		heif_context_free(ctx);
		return -1;
	}

	int n = heif_context_get_number_of_top_level_images(ctx);
	heif_context_free(ctx);
	return n;
}

// decodeHEICToRGBA decodes a HEIC image to interleaved RGBA pixel data.
// NULL decoding options are used so that all ISOBMFF transforms (rotation,
// mirroring) are applied automatically — the output pixels match the
// intended visual orientation. The caller must free *pixels_out.
static int decodeHEICToRGBA(
	const void* data, size_t data_len,
	unsigned char** pixels_out, int* width_out, int* height_out, int* stride_out)
{
	*pixels_out = NULL;
	*width_out = 0;
	*height_out = 0;
	*stride_out = 0;

	if (!cr_heif_load()) return -1;  // load libheif before any heif_* call (NULL ptr otherwise)
	struct heif_context* ctx = heif_context_alloc();
	if (!ctx) return -1;

	struct heif_error err = heif_context_read_from_memory(ctx, data, data_len, NULL);
	if (err.code != heif_error_Ok) {
		heif_context_free(ctx);
		return -1;
	}

	struct heif_image_handle* handle = NULL;
	err = heif_context_get_primary_image_handle(ctx, &handle);
	if (err.code != heif_error_Ok) {
		heif_context_free(ctx);
		return -1;
	}

	struct heif_image* img = NULL;
	// NULL options → apply all ISOBMFF transforms (irot/imir) during decode.
	err = heif_decode_image(handle, &img,
		heif_colorspace_RGB, heif_chroma_interleaved_RGBA, NULL);
	if (err.code != heif_error_Ok) {
		heif_image_handle_release(handle);
		heif_context_free(ctx);
		return -1;
	}

	int w = heif_image_get_width(img, heif_channel_interleaved);
	int h = heif_image_get_height(img, heif_channel_interleaved);
	int stride;
	const uint8_t* plane = heif_image_get_plane_readonly(
		img, heif_channel_interleaved, &stride);
	if (!plane || w <= 0 || h <= 0) {
		heif_image_release(img);
		heif_image_handle_release(handle);
		heif_context_free(ctx);
		return -1;
	}

	size_t total = (size_t)h * (size_t)stride;
	*pixels_out = (unsigned char*)malloc(total);
	if (!*pixels_out) {
		heif_image_release(img);
		heif_image_handle_release(handle);
		heif_context_free(ctx);
		return -1;
	}
	memcpy(*pixels_out, plane, total);
	*width_out = w;
	*height_out = h;
	*stride_out = stride;

	heif_image_release(img);
	heif_image_handle_release(handle);
	heif_context_free(ctx);
	return 0;
}
*/
import "C"

import (
	"bytes"
	"encoding/binary"
	"fmt"
	"image"
	"image/jpeg"
	"path/filepath"
	"strings"
	"unsafe"

	"github.com/rs/zerolog"
)

// maxHEICInputSize is the maximum HEIC file size we'll attempt to convert (100 MB).
const maxHEICInputSize = 100 * 1024 * 1024

// isHEIC returns true if the MIME type indicates an HEIC/HEIF image.
func isHEIC(mimeType string) bool {
	return mimeType == "image/heic" || mimeType == "image/heif"
}

// heicMetadata holds all metadata extracted from a HEIC file.
type heicMetadata struct {
	exif []byte // Raw TIFF/EXIF data (without the 4-byte offset prefix)
	icc  []byte // Raw ICC color profile
	xmp  []byte // Raw XMP/RDF-XML data
}

// extractMetadata extracts EXIF, ICC, and XMP metadata from HEIC data using the
// libheif C API in a single parse pass. Returns empty fields for any metadata
// type that is not present.
func extractMetadata(data []byte) heicMetadata {
	var exifPtr, iccPtr, xmpPtr *C.uchar
	var exifLen, iccLen, xmpLen C.size_t

	C.extractAllMetadataFromHEIC(
		unsafe.Pointer(&data[0]), C.size_t(len(data)),
		&exifPtr, &exifLen,
		&iccPtr, &iccLen,
		&xmpPtr, &xmpLen,
	)

	var meta heicMetadata
	if exifPtr != nil {
		meta.exif = C.GoBytes(unsafe.Pointer(exifPtr), C.int(exifLen))
		C.free(unsafe.Pointer(exifPtr))
	}
	if iccPtr != nil {
		meta.icc = C.GoBytes(unsafe.Pointer(iccPtr), C.int(iccLen))
		C.free(unsafe.Pointer(iccPtr))
	}
	if xmpPtr != nil {
		meta.xmp = C.GoBytes(unsafe.Pointer(xmpPtr), C.int(xmpLen))
		C.free(unsafe.Pointer(xmpPtr))
	}
	return meta
}

// decodeHEICImage decodes HEIC data to a Go image.NRGBA via the libheif C API.
// All ISOBMFF transforms (rotation, mirroring) are applied during decoding,
// so the returned image is in the correct visual orientation.
func decodeHEICImage(data []byte) (image.Image, error) {
	var pixels *C.uchar
	var width, height, stride C.int

	rc := C.decodeHEICToRGBA(
		unsafe.Pointer(&data[0]), C.size_t(len(data)),
		&pixels, &width, &height, &stride,
	)
	if rc != 0 {
		return nil, fmt.Errorf("libheif decode failed")
	}
	defer C.free(unsafe.Pointer(pixels))

	w := int(width)
	h := int(height)
	s := int(stride)

	// libheif RGBA is non-premultiplied, matching Go's image.NRGBA format.
	img := image.NewNRGBA(image.Rect(0, 0, w, h))

	// Create a Go slice view over the C pixel buffer (no copy yet).
	pixelData := unsafe.Slice((*byte)(unsafe.Pointer(pixels)), h*s)

	if s == img.Stride {
		copy(img.Pix, pixelData)
	} else {
		// Row-by-row copy when libheif stride differs from Go stride.
		rowBytes := w * 4
		for y := 0; y < h; y++ {
			copy(img.Pix[y*img.Stride:y*img.Stride+rowBytes],
				pixelData[y*s:y*s+rowBytes])
		}
	}

	return img, nil
}

// resetEXIFOrientation sets the EXIF Orientation tag (0x0112) to 1 (Normal).
// This is necessary because libheif applies ISOBMFF transforms during
// decoding, so the pixels are already correctly oriented. Without resetting
// this tag, JPEG viewers that honor EXIF Orientation would rotate the image
// a second time.
func resetEXIFOrientation(exif []byte) {
	if len(exif) < 8 {
		return
	}

	var bo binary.ByteOrder
	switch string(exif[0:2]) {
	case "II":
		bo = binary.LittleEndian
	case "MM":
		bo = binary.BigEndian
	default:
		return
	}

	// TIFF magic number
	if bo.Uint16(exif[2:4]) != 42 {
		return
	}

	ifdOffset := int(bo.Uint32(exif[4:8]))
	if ifdOffset+2 > len(exif) {
		return
	}

	entryCount := int(bo.Uint16(exif[ifdOffset : ifdOffset+2]))
	for i := 0; i < entryCount; i++ {
		entryStart := ifdOffset + 2 + i*12
		if entryStart+12 > len(exif) {
			return
		}
		tag := bo.Uint16(exif[entryStart : entryStart+2])
		if tag == 0x0112 { // Orientation
			bo.PutUint16(exif[entryStart+8:entryStart+10], 1) // 1 = Normal
			return
		}
	}
}

// maxICCChunkData is the maximum ICC profile data per APP2 chunk.
// APP2 overhead: 2 (length) + 12 ("ICC_PROFILE\0") + 1 (chunk#) + 1 (total) = 16.
const maxICCChunkData = 0xFFFF - 2 - 12 - 1 - 1

// injectMetadataIntoJPEG splices EXIF, ICC, and XMP metadata into a JPEG
// immediately after the SOI marker.
//
// JPEG marker layout after injection:
//
//	FF D8                        (SOI)
//	FF E1 [len] Exif\0\0 [TIFF] (APP1 — EXIF, if present)
//	FF E1 [len] XMP-ns\0 [XMP]  (APP1 — XMP, if present)
//	FF E2 [len] ICC_PROFILE\0   (APP2 — ICC chunk 1, if present)
//	FF E2 [len] ICC_PROFILE\0   (APP2 — ICC chunk N, if present)
//	[rest of original JPEG]
func injectMetadataIntoJPEG(jpegData []byte, meta heicMetadata, log *zerolog.Logger) []byte {
	if len(jpegData) < 2 || jpegData[0] != 0xFF || jpegData[1] != 0xD8 {
		return jpegData // not a valid JPEG
	}

	// Nothing to inject
	if meta.exif == nil && meta.icc == nil && meta.xmp == nil {
		return jpegData
	}

	// Estimate capacity
	extra := 0
	if meta.exif != nil {
		extra += len(meta.exif) + 10 // APP1 marker + length + "Exif\0\0"
	}
	if meta.xmp != nil {
		extra += len(meta.xmp) + 33 // APP1 marker + length + XMP namespace
	}
	if meta.icc != nil {
		numChunks := (len(meta.icc) + maxICCChunkData - 1) / maxICCChunkData
		extra += numChunks * (2 + 2 + 14) + len(meta.icc) // markers + headers + data
	}

	var buf bytes.Buffer
	buf.Grow(len(jpegData) + extra)

	// SOI
	buf.Write(jpegData[:2])

	// ── APP1 — EXIF ─────────────────────────────────────────────
	if meta.exif != nil {
		exifHeader := []byte("Exif\x00\x00")
		app1PayloadLen := len(exifHeader) + len(meta.exif)
		app1Len := 2 + app1PayloadLen // length field includes its own 2 bytes

		if app1Len > 0xFFFF {
			log.Warn().Int("exif_bytes", len(meta.exif)).
				Msg("EXIF data too large for JPEG APP1 segment, dropping EXIF")
		} else {
			buf.Write([]byte{0xFF, 0xE1})                             // APP1 marker
			lenBytes := make([]byte, 2)                               //nolint:mnd
			binary.BigEndian.PutUint16(lenBytes, uint16(app1Len))     //nolint:gosec
			buf.Write(lenBytes)                                       // length
			buf.Write(exifHeader)                                     // "Exif\0\0"
			buf.Write(meta.exif)                                      // raw TIFF data
		}
	}

	// ── APP1 — XMP ──────────────────────────────────────────────
	if meta.xmp != nil {
		xmpHeader := []byte("http://ns.adobe.com/xap/1.0/\x00")
		app1PayloadLen := len(xmpHeader) + len(meta.xmp)
		app1Len := 2 + app1PayloadLen

		if app1Len > 0xFFFF {
			log.Warn().Int("xmp_bytes", len(meta.xmp)).
				Msg("XMP data too large for JPEG APP1 segment, dropping XMP")
		} else {
			buf.Write([]byte{0xFF, 0xE1})                             // APP1 marker
			lenBytes := make([]byte, 2)                               //nolint:mnd
			binary.BigEndian.PutUint16(lenBytes, uint16(app1Len))     //nolint:gosec
			buf.Write(lenBytes)                                       // length
			buf.Write(xmpHeader)                                      // XMP namespace
			buf.Write(meta.xmp)                                       // XMP payload
		}
	}

	// ── APP2 — ICC Profile (chunked if >65519 bytes) ────────────
	if meta.icc != nil {
		totalChunks := (len(meta.icc) + maxICCChunkData - 1) / maxICCChunkData
		if totalChunks > 255 {
			log.Warn().Int("icc_bytes", len(meta.icc)).Int("chunks_needed", totalChunks).
				Msg("ICC profile too large for JPEG (>255 APP2 chunks), dropping ICC")
		} else {
			iccSignature := []byte("ICC_PROFILE\x00")
			for chunk := 0; chunk < totalChunks; chunk++ {
				start := chunk * maxICCChunkData
				end := start + maxICCChunkData
				if end > len(meta.icc) {
					end = len(meta.icc)
				}
				chunkData := meta.icc[start:end]

				// Length = 2 (self) + 12 (signature) + 1 (chunk#) + 1 (total) + data
				segLen := 2 + len(iccSignature) + 2 + len(chunkData)

				buf.Write([]byte{0xFF, 0xE2})                             // APP2 marker
				lenBytes := make([]byte, 2)                               //nolint:mnd
				binary.BigEndian.PutUint16(lenBytes, uint16(segLen))       //nolint:gosec
				buf.Write(lenBytes)                                       // length
				buf.Write(iccSignature)                                   // "ICC_PROFILE\0"
				buf.WriteByte(byte(chunk + 1))                            // 1-based chunk number
				buf.WriteByte(byte(totalChunks))                          // total chunks
				buf.Write(chunkData)                                      // profile data
			}
		}
	}

	// Rest of original JPEG (everything after SOI)
	buf.Write(jpegData[2:])
	return buf.Bytes()
}

// convertHEICToJPEG decodes HEIC/HEIF bytes and re-encodes as JPEG,
// preserving EXIF, ICC color profile, and XMP metadata if present.
// Returns the JPEG bytes, updated MIME type, updated filename, the decoded
// image (for callers that need dimensions/thumbnails), and any error.
func convertHEICToJPEG(data []byte, mimeType, fileName string, quality int, log *zerolog.Logger) ([]byte, string, string, image.Image, error) {
	if len(data) > maxHEICInputSize {
		return nil, mimeType, fileName, nil, fmt.Errorf("HEIC input too large: %d bytes (max %d)", len(data), maxHEICInputSize)
	}

	// Clamp quality to valid range
	if quality < 1 || quality > 100 {
		quality = 95
	}

	// Extract all metadata via C API
	meta := extractMetadata(data)
	if meta.exif != nil {
		log.Debug().Int("exif_bytes", len(meta.exif)).Msg("Extracted EXIF from HEIC")
		// Reset EXIF Orientation to 1 (Normal) since libheif applies ISOBMFF
		// transforms during decoding — the pixels are already correctly
		// oriented. Without this, viewers would double-rotate the image.
		resetEXIFOrientation(meta.exif)
	}
	if meta.icc != nil {
		log.Debug().Int("icc_bytes", len(meta.icc)).Msg("Extracted ICC profile from HEIC")
	}
	if meta.xmp != nil {
		log.Debug().Int("xmp_bytes", len(meta.xmp)).Msg("Extracted XMP from HEIC")
	}

	// Decode HEIC to RGBA pixels via C API. NULL decoding options apply all
	// ISOBMFF transforms (rotation, mirroring) automatically.
	goImg, err := decodeHEICImage(data)
	if err != nil {
		return nil, mimeType, fileName, nil, fmt.Errorf("decodeHEICImage: %w", err)
	}

	var buf bytes.Buffer
	if err := jpeg.Encode(&buf, goImg, &jpeg.Options{Quality: quality}); err != nil {
		return nil, mimeType, fileName, nil, fmt.Errorf("jpeg.Encode: %w", err)
	}

	jpegBytes := injectMetadataIntoJPEG(buf.Bytes(), meta, log)

	newName := strings.TrimSuffix(fileName, filepath.Ext(fileName)) + ".jpg"
	return jpegBytes, "image/jpeg", newName, goImg, nil
}

// heicConvertSem serializes HEIC decoding process-wide. Decoding a HEIC
// allocates a full RGBA bitmap (width*height*4) in C plus a copy in the Go
// image — ~100-250 MB for a typical 12 MP iPhone photo. The backfill fan-out
// would otherwise run many decodes at once and peg memory: the concurrent
// high-water mark is retained by the Go heap and glibc's allocator long after
// the decodes finish, so it looks like a leak even though every C resource is
// freed. Capping concurrency to one decode bounds peak image memory, the same
// way transcodeSem bounds ffmpeg. The slot is held only for the decode/convert.
var heicConvertSem = make(chan struct{}, 1)

// maybeConvertHEIC handles HEIC/HEIF images. When conversion is enabled, it
// converts to JPEG and returns the updated data, MIME type, filename, and the
// decoded image. When conversion is disabled but the MIME type is HEIC/HEIF,
// it decodes the image for dimension/thumbnail extraction while returning the
// original data unchanged. Returns a nil image only for non-HEIC types or on
// decode failure.
func maybeConvertHEIC(log *zerolog.Logger, data []byte, mimeType, fileName string, quality int, enabled bool) ([]byte, string, string, image.Image) {
	if !isHEIC(mimeType) || len(data) == 0 {
		return data, mimeType, fileName, nil
	}

	// One HEIC decode at a time across the whole process — bounds peak image
	// memory so a backfill of many photos can't pile up bitmaps and OOM a
	// small host. Acquired only for actual HEIC work (non-HEIC returned above).
	heicConvertSem <- struct{}{}
	defer func() { <-heicConvertSem }()

	if !enabled {
		// Decode for dimensions/thumbnail only, keep original HEIC data
		if len(data) > maxHEICInputSize {
			log.Warn().Int("heic_bytes", len(data)).Int("max_bytes", maxHEICInputSize).
				Msg("HEIC input too large to decode for dimensions, skipping")
			return data, mimeType, fileName, nil
		}
		img, err := decodeHEICImage(data)
		if err != nil {
			log.Warn().Err(err).Msg("Failed to decode HEIC for dimensions")
		}
		return data, mimeType, fileName, img
	}

	// Check for animated/multi-frame HEIC
	if n := C.getTopLevelImageCount(unsafe.Pointer(&data[0]), C.size_t(len(data))); n > 1 {
		log.Warn().Int("frame_count", int(n)).
			Msg("Animated/multi-frame HEIC detected, only primary image will be converted")
	}

	jpegData, newMime, newName, decodedImg, err := convertHEICToJPEG(data, mimeType, fileName, quality, log)
	if err != nil {
		log.Warn().Err(err).Msg("HEIC to JPEG conversion failed, uploading original")
		return data, mimeType, fileName, nil
	}
	log.Info().
		Int("original_bytes", len(data)).
		Int("jpeg_bytes", len(jpegData)).
		Msg("Converted HEIC to JPEG")
	return jpegData, newMime, newName, decodedImg
}
