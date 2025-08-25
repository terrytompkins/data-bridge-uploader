//go:build !libjxl

package main

import (
	"context"
	"fmt"
)

// LibjxlCompressor is a stub unless built with -tags libjxl
type LibjxlCompressor struct{}

func (LibjxlCompressor) Name() string { return "libjxl" }

func (LibjxlCompressor) CompressPNGToJXL(ctx context.Context, in FileItem, stageRoot string, verbose Verbose) (UploadItem, error) {
	return UploadItem{}, fmt.Errorf("compressor=libjxl selected, but binary not built with -tags libjxl (native libjxl mode not available)")
}
