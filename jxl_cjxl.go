//go:build !libjxl || libjxl

package main

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"time"
)

// CjxlCompressor shells out to cjxl (lossless)
type CjxlCompressor struct{}

func (CjxlCompressor) Name() string { return "cjxl" }

func (CjxlCompressor) CompressPNGToJXL(ctx context.Context, in FileItem, stageRoot string, verbose Verbose) (UploadItem, error) {
	start := time.Now()
	destRel := in.Rel[:len(in.Rel)-len(filepath.Ext(in.Rel))] + ".jxl"
	destPath := filepath.Join(stageRoot, destRel)
	if err := ensureDir(filepath.Dir(destPath)); err != nil {
		return UploadItem{}, err
	}
	if _, err := exec.LookPath("cjxl"); err != nil {
		return UploadItem{}, fmt.Errorf("cjxl not found in PATH: %w", err)
	}
	// Lossless: -d 0 ; effort 5 for balance
	cmd := exec.CommandContext(ctx, "cjxl", in.Path, destPath, "-d", "0", "-e", "5")
	if err := cmd.Run(); err != nil {
		return UploadItem{}, fmt.Errorf("cjxl run: %w", err)
	}
	fi, err := os.Stat(destPath)
	if err != nil { return UploadItem{}, err }
	return UploadItem{
		LocalPath:    destPath,
		Key:          filepath.ToSlash(destRel),
		OrigSize:     in.Size,
		OutputSize:   fi.Size(),
		CompressTime: time.Since(start),
	}, nil
}
