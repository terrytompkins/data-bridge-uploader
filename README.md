# data-bridge-uploader v3 (pipelined PNG→JXL + S3 with resume)

This version adds:
- **Compressor toggle**: `--compressor cjxl|libjxl` to switch between shelling to `cjxl` and a **native libjxl** path. (The native path is behind a build tag; see below.)
- **Resumable uploads**: records successful object keys to a **state file** and skips them on subsequent runs; optional `HEAD` check against S3.
- **Safer default**: `--delete-staged=false` by default (keeps .jxl files unless you opt in).
- **S3 Transfer Acceleration**: `--accel` flag for faster uploads to distant regions.
- **S3 Multi-Region Access Point**: `--mrap-arn` for global access points.
- **Multipart tuning**: `--part-size-mb` and `--uploader-concurrency` for S3 upload optimization.
- **File-level parallelism**: `--upload-parallel` for concurrent file uploads within batches.
- **Network resilience**: Adaptive queue management and configurable timeouts for varying network conditions.

## Prerequisites

### Required Software

1. **Go** (version 1.22 or later):
   ```bash
   go version
   ```

2. **cjxl** (for compression - optional but recommended):
   ```bash
   # Install via package manager (Ubuntu/Debian)
   sudo apt-get install libjxl-tools
   
   # Install via Homebrew (macOS)
   brew install libjxl
   
   # Verify installation
   cjxl --version
   ```

### AWS Configuration

Ensure you have AWS credentials configured for S3 access:
```bash
# Option 1: AWS CLI configuration
aws configure

# Option 2: Environment variables
export AWS_ACCESS_KEY_ID=your_access_key
export AWS_SECRET_ACCESS_KEY=your_secret_key
export AWS_REGION=us-east-1

# Option 3: IAM roles (for EC2/ECS deployment)
# No additional configuration needed
```

## Build

### Basic Build (Recommended)

```bash
# Default build - creates 'uploader' executable
go build -o uploader .

# Verify the build
./uploader --help
```

### Optimized Build (Smaller executable)

```bash
# Strips debug information for smaller binary
go build -ldflags="-s -w" -o uploader-optimized .
```

### Cross-Platform Builds

For deployment to different operating systems or architectures:

```bash
# Linux ARM64 (common for edge devices)
GOOS=linux GOARCH=arm64 go build -o uploader-arm64 .

# Linux x86_64
GOOS=linux GOARCH=amd64 go build -o uploader-linux .

# Windows
GOOS=windows GOARCH=amd64 go build -o uploader.exe .

# macOS ARM64 (Apple Silicon)
GOOS=darwin GOARCH=arm64 go build -o uploader-darwin-arm64 .

# macOS x86_64 (Intel)
GOOS=darwin GOARCH=amd64 go build -o uploader-darwin-amd64 .
```

### Native libjxl Build (Advanced)

```bash
# Enable native libjxl compression (requires CGO and libjxl development headers)
go build -tags libjxl -o uploader-libjxl .
```

**Prerequisites for libjxl build:**
```bash
# Ubuntu/Debian
sudo apt-get install libjxl-dev pkg-config

# macOS
brew install libjxl pkg-config

# Verify pkg-config can find libjxl
pkg-config --libs libjxl
```

> **Note**: The `libjxl` mode provides native compression without shelling out to the `cjxl` command line tool, which can be more efficient and reliable. It requires CGO and the libjxl development headers to be installed.

### Build Verification

After building, verify the executable works:
```bash
# Check help output
./uploader --help

# Test with invalid parameters (should show usage)
./uploader -input-dir /nonexistent -bucket-name test
```

### Deployment Considerations

**For Medical Device Deployment:**
- Use **Linux ARM64** builds for edge devices
- Consider **optimized builds** (`-ldflags="-s -w"`) for smaller binaries
- Ensure `cjxl` is installed on target devices if using compression
- Configure appropriate AWS credentials or IAM roles

**For Development:**
- Use **native builds** for your development platform
- Keep debug information for troubleshooting
- Use verbose logging (`-verbose-level 2` or `3`) during testing

## Run

```bash
./uploader   -input-dir /data/run42/images   -bucket-name my-iot-bucket   -prefix ops/2025-08-22/run42/   -batch-size 200   -verbose-level 2   -auto-tune-batch-size=true   -compressor cjxl   -resume=true   -state-file /var/tmp/dbu/run42.state   -resume-remote-check=false   -delete-staged=false   -accel=true   -part-size-mb 32   -uploader-concurrency 8   -upload-parallel 4
```

### Flags

- `--skip-compression` (bool): skip PNG→JXL; upload as-is.
- `--compressor cjxl|libjxl`: choose compressor when compression is enabled.
- `--batch-size` (int): images per batch (default **200**).
- `--verbose-level` (0..3): 0=silent, 1=batch events, 2=per-batch stats, 3=per-image stats.
- `--auto-tune-batch-size` (bool): adapts batch size by ±20% when one stage is ~25% slower than the other.
- `--input-dir` (path): source images root.
- `--bucket-name` (string): S3 bucket (ignored if `--mrap-arn` provided).
- `--mrap-arn` (string): S3 Multi-Region Access Point ARN (overrides `--bucket-name`).
- `--prefix` (string): S3 key prefix (`s3://bucket/prefix/...`).
- `--stage-dir` (path): where temporary `.jxl` files are written (default: system temp).
- `--delete-staged` (bool, default **false**): delete staged `.jxl` after upload.
- `--resume` (bool, default **true**): record success and skip already-uploaded keys on rerun.
- `--state-file` (path): JSONL state file (default: `<stage-dir>/upload_state.jsonl`).
- `--resume-remote-check` (bool): when resuming, do a `HeadObject` and skip if present (adds one RTT per object).
- `--accel` (bool): enable S3 Transfer Acceleration (not used with MRAP).
- `--part-size-mb` (int): multipart part size in MiB (default **16**, min 5).
- `--uploader-concurrency` (int): multipart concurrency per large file (default **CPU cores**).
- `--upload-parallel` (int): number of files uploaded in parallel within a batch (default **1**).
- `--queue-timeout` (duration): timeout for upload queue, useful for slow networks (default **1m0s**).

### How the pipeline works

1. **Batching**: files are sliced into batches (`--batch-size`).  
2. **Compression stage** (if enabled): PNG files are turned into **lossless JPEG XL** (distance 0), using either:
   - **`cjxl`** (external tool) with effort 5 default, or
   - **`libjxl`** (compile-time option; stubbed here—add your CGO binding later).
3. **Upload stage**: exactly **one batch uploads at a time** (to avoid saturating clinic WAN), via AWS SDK for Go v2 `manager.Uploader` (multipart + persistent connections).  
4. **Overlap**: while batch *N* uploads, batch *N+1* compresses, up to a queue depth of 10 (adaptive for slow networks).
5. **Auto-tune**: after each upload, compares compression vs. upload time and adjusts batch size to keep both stages busy.
6. **Parallel uploads**: within each batch, `--upload-parallel` files can upload concurrently (default 1 to be gentle on WAN).

### S3 Optimization Features

- **Transfer Acceleration**: Use `--accel` for faster uploads to distant regions (requires bucket acceleration enabled).
- **Multi-Region Access Points**: Use `--mrap-arn` for global access points with automatic failover.
- **Multipart tuning**: Adjust `--part-size-mb` (16-100 MiB) and `--uploader-concurrency` for optimal throughput.
- **File parallelism**: Increase `--upload-parallel` for faster batch uploads (be mindful of WAN capacity).

### Resumable behavior (small-object friendly)

- A local **state file** (JSON lines) records each successfully uploaded key.  
- On restart with `--resume=true`, the uploader **skips** any key found in the state file.  
- Optionally `--resume-remote-check=true` sends an S3 `HEAD` before uploading each file to skip already-present objects (slower but extra safe).  
- This is optimized for **many small images** (your case). For single multi-GB files, you'd implement true multipart **part-level resume**.

### Lossless JXL defaults

- Uses **distance 0** (lossless) and **effort 5**. You can change the effort by editing the `cjxl` arguments in `jxl_cjxl.go` if you find a better CPU/ratio tradeoff for your device.

### Native libjxl Support

The uploader now includes full native libjxl support:

- **Built-in implementation**: `jxl_native.go` provides CGO bindings to libjxl C API
- **Lossless compression**: Uses distance 0 for pixel-perfect compression
- **Parallel processing**: Leverages libjxl's built-in threading
- **No external dependencies**: No need for `cjxl` command line tool when using `--compressor libjxl`

**Usage:**
```bash
# Build with native libjxl support
go build -tags libjxl -o uploader-libjxl .

# Use native compression
./uploader-libjxl --compressor libjxl -input-dir /path/to/images -bucket-name my-bucket
```

**Benefits over cjxl:**
- **Faster**: No process spawning overhead
- **More reliable**: No external tool dependencies
- **Better integration**: Direct memory management and error handling
- **Cross-platform**: Works on any platform with libjxl development headers

### Example output (verbosity=2)

```
Using compressor: cjxl
[batch compressed] id=1 files=200 in=312.45MB out=212.37MB ratio=0.680 time=34.2s
[batch upload start] id=1 files=200 size=212.37MB
[batch uploaded] id=1 files=200 time=28.5s
[tune] upload slower (28.5s vs 34.2s) -> grow batch 200 -> 240
...
SUMMARY | files=4800 in=8123.17MB out=5510.51MB ratio=0.679 time=13m42s throughput=6.70 MB/s compression=true final_batch=240
```

### Operational tips

- For clinics with **fast uplinks**, run with `--skip-compression` (no temp I/O, simpler pipeline).  
- For clinics with **slow uplinks**, keep compression on; consider increasing `--batch-size` until upload time ~= compression time.  
- For **long-haul clinics**, enable `--accel` and consider larger `--part-size-mb` (32-64 MiB).
- For **global deployments**, use `--mrap-arn` for automatic region failover.
- For **high-bandwidth clinics**, increase `--upload-parallel` (2-4) for faster batch uploads.
- For **very slow networks**, increase `--queue-timeout` (e.g., `--queue-timeout 5m`) to prevent premature failures.
- Consider enabling **S3 Transfer Acceleration** on the bucket and pointing your AWS config to the accelerate endpoint for long-haul clinics.
