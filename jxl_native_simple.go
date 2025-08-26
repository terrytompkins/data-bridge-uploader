//go:build libjxl

package main

/*
#cgo pkg-config: libjxl
#include <jxl/encode.h>
#include <jxl/color_encoding.h>
#include <jxl/types.h>
#include <stdlib.h>
#include <string.h>

JxlEncoder* create_jxl_encoder() {
    return JxlEncoderCreate(NULL);
}

int set_basic_info(JxlEncoder* enc, uint32_t width, uint32_t height, int bits_per_sample, int num_channels) {
    JxlBasicInfo basic_info;
    JxlEncoderInitBasicInfo(&basic_info);
    basic_info.xsize = width;
    basic_info.ysize = height;
    basic_info.bits_per_sample = bits_per_sample;
    basic_info.exponent_bits_per_sample = 0;
    basic_info.uses_original_profile = JXL_FALSE;
    basic_info.have_animation = JXL_FALSE;
    basic_info.orientation = JXL_ORIENT_IDENTITY;

    return JxlEncoderSetBasicInfo(enc, &basic_info);
}

int set_color_encoding(JxlEncoder* enc) {
    JxlColorEncoding color_encoding;
    JxlColorEncodingSetToSRGB(&color_encoding, JXL_FALSE);
    return JxlEncoderSetColorEncoding(enc, &color_encoding);
}

int add_frame_simple(JxlEncoder* enc, const uint8_t* pixels, size_t pixels_size, int num_channels, int bits_per_sample) {
    JxlEncoderFrameSettings* frame_settings = JxlEncoderFrameSettingsCreate(enc, NULL);
    if (!frame_settings) {
        return JXL_ENC_ERROR;
    }

    // Set lossless compression
    if (JxlEncoderSetFrameDistance(frame_settings, 0.0f) != JXL_ENC_SUCCESS) {
        return JXL_ENC_ERROR;
    }

    JxlPixelFormat pixel_format = {num_channels, bits_per_sample == 16 ? JXL_TYPE_UINT16 : JXL_TYPE_UINT8, JXL_LITTLE_ENDIAN, 0};

    if (JxlEncoderAddImageFrame(frame_settings, &pixel_format, pixels, pixels_size) != JXL_ENC_SUCCESS) {
        return JXL_ENC_ERROR;
    }

    return JXL_ENC_SUCCESS;
}

int get_compressed_data(JxlEncoder* enc, uint8_t* output, size_t output_size, size_t* output_used) {
    uint8_t* next_out = output;
    size_t avail_out = output_size;
    JxlEncoderStatus status = JxlEncoderProcessOutput(enc, &next_out, &avail_out);
    *output_used = output_size - avail_out;
    return status;
}
*/
import "C"
import (
	"context"
	"fmt"
	"image"
	"image/png"
	"os"
	"path/filepath"
	"time"
)

// LibjxlCompressor uses native libjxl for compression
type LibjxlCompressor struct{}

func (LibjxlCompressor) Name() string { return "libjxl" }

func (LibjxlCompressor) CompressPNGToJXL(ctx context.Context, in FileItem, stageRoot string, verbose Verbose) (UploadItem, error) {
	start := time.Now()
	destRel := in.Rel[:len(in.Rel)-len(filepath.Ext(in.Rel))] + ".jxl"
	destPath := filepath.Join(stageRoot, destRel)

	if err := ensureDir(filepath.Dir(destPath)); err != nil {
		return UploadItem{}, err
	}

	// Read PNG image
	file, err := os.Open(in.Path)
	if err != nil {
		return UploadItem{}, fmt.Errorf("failed to open PNG file: %w", err)
	}
	defer file.Close()

	img, err := png.Decode(file)
	if err != nil {
		return UploadItem{}, fmt.Errorf("failed to decode PNG: %w", err)
	}

	// Compress using libjxl
	compressedData, err := compressWithLibjxlSimple(img)
	if err != nil {
		return UploadItem{}, fmt.Errorf("libjxl compression failed: %w", err)
	}

	// Write compressed data to file
	if err := os.WriteFile(destPath, compressedData, 0644); err != nil {
		return UploadItem{}, fmt.Errorf("failed to write JXL file: %w", err)
	}

	fi, err := os.Stat(destPath)
	if err != nil {
		return UploadItem{}, err
	}

	return UploadItem{
		LocalPath:    destPath,
		Key:          filepath.ToSlash(destRel),
		OrigSize:     in.Size,
		OutputSize:   fi.Size(),
		CompressTime: time.Since(start),
	}, nil
}

func compressWithLibjxlSimple(img image.Image) ([]byte, error) {
	// Create encoder
	enc := C.create_jxl_encoder()
	if enc == nil {
		return nil, fmt.Errorf("failed to create JXL encoder")
	}
	// Note: We'll clean up the encoder manually to avoid CGO issues

	// Set basic info (will be updated with correct values after format detection)
	bounds := img.Bounds()
	width := C.uint32_t(bounds.Dx())
	height := C.uint32_t(bounds.Dy())

	// Convert to RGBA for simplicity
	rgba := image.NewRGBA(bounds)
	for y := bounds.Min.Y; y < bounds.Max.Y; y++ {
		for x := bounds.Min.X; x < bounds.Max.X; x++ {
			rgba.Set(x, y, img.At(x, y))
		}
	}

	// Prepare pixel data (RGBA)
	pixels := make([]byte, width*height*4)
	idx := 0
	for y := bounds.Min.Y; y < bounds.Max.Y; y++ {
		for x := bounds.Min.X; x < bounds.Max.X; x++ {
			r, g, b, a := rgba.At(x, y).RGBA()
			pixels[idx] = byte(r >> 8)
			pixels[idx+1] = byte(g >> 8)
			pixels[idx+2] = byte(b >> 8)
			pixels[idx+3] = byte(a >> 8)
			idx += 4
		}
	}

	numChannels := 4
	bitsPerSample := 8

	// Set basic info with correct format
	if C.set_basic_info(enc, width, height, C.int(bitsPerSample), C.int(numChannels)) != C.JXL_ENC_SUCCESS {
		return nil, fmt.Errorf("failed to set basic info")
	}

	// Set color encoding
	if C.set_color_encoding(enc) != C.JXL_ENC_SUCCESS {
		return nil, fmt.Errorf("failed to set color encoding")
	}

	// Add frame
	pixelsSize := C.size_t(len(pixels))
	if C.add_frame_simple(enc, (*C.uint8_t)(&pixels[0]), pixelsSize, C.int(numChannels), C.int(bitsPerSample)) != C.JXL_ENC_SUCCESS {
		return nil, fmt.Errorf("failed to add frame")
	}

	// Finalize encoding
	C.JxlEncoderCloseInput(enc)

	// Get compressed data
	var compressedData []byte
	buffer := make([]byte, 65536) // 64KB buffer

	for {
		var outputUsed C.size_t
		status := C.get_compressed_data(enc, (*C.uint8_t)(&buffer[0]), C.size_t(len(buffer)), &outputUsed)

		if status == C.JXL_ENC_SUCCESS {
			compressedData = append(compressedData, buffer[:outputUsed]...)
			break
		} else if status == C.JXL_ENC_NEED_MORE_OUTPUT {
			compressedData = append(compressedData, buffer[:outputUsed]...)
			// Increase buffer size if needed
			if len(buffer) < 1024*1024 { // 1MB max
				buffer = make([]byte, len(buffer)*2)
			}
		} else {
			return nil, fmt.Errorf("compression failed with status: %d", status)
		}
	}

	// Clean up encoder
	C.JxlEncoderDestroy(enc)

	return compressedData, nil
}
