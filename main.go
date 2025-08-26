package main

import (
	"bufio"
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/feature/s3/manager"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/aws-sdk-go-v2/service/s3/types"
)

// ===================== Types & Config =====================

type Verbose int

const (
	V0 Verbose = 0 // silent
	V1 Verbose = 1 // batch completion
	V2 Verbose = 2 // batch statistics
	V3 Verbose = 3 // per-image stats
)

type Flags struct {
	SkipCompression     bool
	CompressorName      string
	BatchSize           int
	Verbose             Verbose
	AutoTune            bool
	InputDir            string
	Bucket              string
	MRAPArn             string
	Prefix              string
	StageDir            string
	DeleteStaged        bool
	Resume              bool
	StateFile           string
	ResumeCheckHead     bool
	Accel               bool
	PartSizeMB          int64
	UploaderConcurrency int
	UploadParallel      int
	QueueTimeout        time.Duration
}

type FileItem struct {
	Path string
	Rel  string
	Size int64
}

type UploadItem struct {
	LocalPath    string
	Key          string // without prefix; prefix applied during upload
	OrigSize     int64
	OutputSize   int64
	CompressTime time.Duration
}

type Batch struct {
	ID        int
	Items     []UploadItem
	TotalIn   int64
	TotalOut  int64
	CompDur   time.Duration
	UploadDur time.Duration
	SkippedC  bool // compression skipped
}

type Summary struct {
	Files          int64
	BytesIn        int64
	BytesOut       int64
	Duration       time.Duration
	ThroughputMBps float64
	AvgCompression float64 // out/in
	CompressedUsed bool
	FinalBatchSize int
}

// ===================== Utils =====================

func must(err error) {
	if err != nil {
		log.Fatal(err)
	}
}

// validateBucket tests bucket connectivity and configuration
func validateBucket(ctx context.Context, client *s3.Client, bucket string, useAccel bool) error {
	// Test basic connectivity by listing objects (limit to 1)
	_, err := client.ListObjectsV2(ctx, &s3.ListObjectsV2Input{
		Bucket:  aws.String(bucket),
		MaxKeys: aws.Int32(1),
	})
	if err != nil {
		return fmt.Errorf("cannot access bucket %s: %w", bucket, err)
	}

	// If using transfer acceleration, verify it's enabled
	if useAccel {
		accel, err := client.GetBucketAccelerateConfiguration(ctx, &s3.GetBucketAccelerateConfigurationInput{
			Bucket: aws.String(bucket),
		})
		if err != nil {
			return fmt.Errorf("cannot check transfer acceleration for bucket %s: %w", bucket, err)
		}
		if accel.Status != types.BucketAccelerateStatusEnabled {
			return fmt.Errorf("transfer acceleration is not enabled on bucket %s (status: %s)", bucket, accel.Status)
		}
	}

	return nil
}

func isPNG(p string) bool {
	ext := strings.ToLower(filepath.Ext(p))
	return ext == ".png"
}

func listFiles(root string) ([]FileItem, error) {
	var out []FileItem
	err := filepath.WalkDir(root, func(p string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			return nil
		}
		inf, err := d.Info()
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(root, p)
		out = append(out, FileItem{Path: p, Rel: rel, Size: inf.Size()})
		return nil
	})
	return out, err
}

func ensureDir(p string) error { return os.MkdirAll(p, 0o755) }
func mb(n int64) float64       { return float64(n) / 1024.0 / 1024.0 }
func max(a, b int64) int64 {
	if a > b {
		return a
	}
	return b
}

// ===================== Compression Interfaces =====================

type Compressor interface {
	Name() string
	CompressPNGToJXL(ctx context.Context, in FileItem, stageRoot string, verbose Verbose) (UploadItem, error)
}

// Implemented in jxl_cjxl.go
// type CjxlCompressor struct{}

// Implemented in jxl_native_stub.go (and optionally jxl_native.go if built with -tags libjxl)
// type LibjxlCompressor struct{}

// ===================== State (Resumable uploads) =====================

type stateRecord struct {
	Key      string `json:"key"`
	Local    string `json:"local"`
	Size     int64  `json:"size"`
	Uploaded bool   `json:"uploaded"`
	ETag     string `json:"etag,omitempty"`
	Time     string `json:"time"`
}

type State struct {
	path         string
	done         map[string]stateRecord
	mu           sync.Mutex
	f            *os.File
	verifyRemote bool
	client       *s3.Client
	bucket       string
	prefix       string
}

func loadState(path string) (*State, error) {
	s := &State{path: path, done: make(map[string]stateRecord)}
	// open/create appendable file
	if err := ensureDir(filepath.Dir(path)); err != nil {
		return nil, err
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o644)
	if err != nil {
		return nil, err
	}
	// read existing
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" {
			continue
		}
		var r stateRecord
		if err := json.Unmarshal([]byte(line), &r); err == nil && r.Uploaded {
			s.done[r.Key] = r
		}
	}
	// reopen for append
	f.Close()
	f, err = os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		return nil, err
	}
	s.f = f
	return s, nil
}

func (s *State) markUploaded(key, local, etag string, size int64) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	rec := stateRecord{
		Key:      key,
		Local:    local,
		Size:     size,
		Uploaded: true,
		ETag:     etag,
		Time:     time.Now().UTC().Format(time.RFC3339),
	}
	b, _ := json.Marshal(rec)
	if _, err := s.f.Write(append(b, '\n')); err != nil {
		return err
	}
	s.done[key] = rec
	return nil
}

func (s *State) isUploaded(key string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	_, ok := s.done[key]
	return ok
}

// ===================== S3 Uploader =====================

type S3Uploader struct {
	client         *s3.Client
	uploader       *manager.Uploader
	bucket         string
	prefix         string
	verbose        Verbose
	state          *State
	uploadParallel int
}

type S3Opts struct {
	Accel               bool
	UseARNRegion        bool
	PartSize            int64
	UploaderConcurrency int
}

func NewS3Uploader(ctx context.Context, bucket, prefix string, verbose Verbose, state *State, opts S3Opts) (*S3Uploader, error) {
	cfg, err := config.LoadDefaultConfig(ctx)
	if err != nil {
		return nil, err
	}
	client := s3.NewFromConfig(cfg, func(o *s3.Options) {
		o.UseAccelerate = opts.Accel
		o.UseARNRegion = opts.UseARNRegion
	})
	up := manager.NewUploader(client, func(u *manager.Uploader) {
		if opts.PartSize > 0 {
			u.PartSize = opts.PartSize
		} else {
			u.PartSize = 16 * 1024 * 1024 // 16 MiB default
		}
		if opts.UploaderConcurrency > 0 {
			u.Concurrency = opts.UploaderConcurrency
		} else {
			u.Concurrency = runtime.NumCPU()
		}
	})
	return &S3Uploader{client: client, uploader: up, bucket: bucket, prefix: prefix, verbose: verbose, state: state, uploadParallel: 1}, nil
}

func (u *S3Uploader) headExists(ctx context.Context, key string, size int64) bool {
	if u.state == nil || !u.state.verifyRemote {
		return false
	}
	_, err := u.client.HeadObject(ctx, &s3.HeadObjectInput{
		Bucket: aws.String(u.bucket),
		Key:    aws.String(key),
	})
	// If not found, err will be typed; here treat nil as exists.
	return err == nil
}

func (u *S3Uploader) UploadBatch(ctx context.Context, b *Batch) error {
	start := time.Now()
	sem := make(chan struct{}, u.uploadParallel)
	var wg sync.WaitGroup
	var firstErr atomic.Value

	for i, it := range b.Items {
		key := filepath.ToSlash(filepath.Join(u.prefix, it.Key))

		// Resume logic
		if u.state != nil && (u.state.isUploaded(key) || u.headExists(ctx, key, it.OutputSize)) {
			if u.verbose >= V3 {
				fmt.Printf("[resume skip] batch=%d idx=%d key=%s\n", b.ID, i, key)
			}
			continue
		}

		wg.Add(1)
		sem <- struct{}{}
		go func(idx int, it UploadItem, key string) {
			defer wg.Done()
			defer func() { <-sem }()

			f, err := os.Open(it.LocalPath)
			if err != nil {
				firstErr.Store(err)
				return
			}
			defer f.Close()

			out, err := u.uploader.Upload(ctx, &s3.PutObjectInput{
				Bucket: aws.String(u.bucket),
				Key:    aws.String(key),
				Body:   f,
				// You can set StorageClass, Metadata, Checksums here as needed.
			})
			if err != nil {
				firstErr.Store(err)
				return
			}
			if u.state != nil {
				etag := ""
				if out != nil && out.ETag != nil {
					etag = *out.ETag
				}
				_ = u.state.markUploaded(key, it.LocalPath, etag, it.OutputSize)
			}
			if u.verbose >= V3 {
				ratio := float64(it.OutputSize) / float64(max(1, it.OrigSize))
				fmt.Printf("[upload img] batch=%d idx=%d key=%s size_out=%.2fMB ratio=%.3f\n",
					b.ID, idx, key, float64(it.OutputSize)/1024.0/1024.0, ratio)
			}
		}(i, it, key)
	}
	wg.Wait()

	if v := firstErr.Load(); v != nil {
		return v.(error)
	}

	b.UploadDur = time.Since(start)
	return nil
}

// ===================== Batching & Pipeline =====================

func compressBatch(ctx context.Context, comp Compressor, batchID int, files []FileItem, stageRoot string, v Verbose) (*Batch, error) {
	b := &Batch{ID: batchID}
	start := time.Now()

	var totalIn, totalOut int64
	items := make([]UploadItem, len(files))

	workers := runtime.NumCPU()
	type job struct {
		idx int
		fi  FileItem
	}
	jobs := make(chan job)
	errs := make(chan error, 1)
	var wg sync.WaitGroup

	for w := 0; w < workers; w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := range jobs {
				if !isPNG(j.fi.Path) {
					items[j.idx] = UploadItem{
						LocalPath:  j.fi.Path,
						Key:        filepath.ToSlash(j.fi.Rel),
						OrigSize:   j.fi.Size,
						OutputSize: j.fi.Size,
					}
					continue
				}
				it, err := comp.CompressPNGToJXL(ctx, j.fi, stageRoot, v)
				if err != nil {
					select {
					case errs <- err:
					default:
					}
					return
				}
				items[j.idx] = it
				if v >= V3 {
					ratio := float64(it.OutputSize) / float64(max(1, it.OrigSize))
					fmt.Printf("[compress img] batch=%d rel=%s time=%s in=%.2fMB out=%.2fMB ratio=%.3f\n",
						batchID, j.fi.Rel, it.CompressTime, float64(it.OrigSize)/1024.0/1024.0, float64(it.OutputSize)/1024.0/1024.0, ratio)
				}
			}
		}()
	}

	for i, fi := range files {
		select {
		case jobs <- job{idx: i, fi: fi}:
		case err := <-errs:
			close(jobs)
			return nil, err
		}
	}
	close(jobs)
	wg.Wait()

	select {
	case err := <-errs:
		return nil, err
	default:
	}

	for _, it := range items {
		totalIn += it.OrigSize
		totalOut += it.OutputSize
	}
	b.Items = items
	b.TotalIn = totalIn
	b.TotalOut = totalOut
	b.CompDur = time.Since(start)
	if v >= V1 {
		if v >= V2 {
			ratio := float64(b.TotalOut) / float64(max(1, b.TotalIn))
			fmt.Printf("[batch compressed] id=%d files=%d in=%.2fMB out=%.2fMB ratio=%.3f time=%s\n",
				batchID, len(files), mb(totalIn), mb(totalOut), ratio, b.CompDur)
		} else {
			fmt.Printf("[batch compressed] id=%d files=%d time=%s\n", batchID, len(files), b.CompDur)
		}
	}
	return b, nil
}

func passthroughBatch(batchID int, files []FileItem, v Verbose) *Batch {
	var items []UploadItem
	var total int64
	for _, fi := range files {
		items = append(items, UploadItem{
			LocalPath:  fi.Path,
			Key:        filepath.ToSlash(fi.Rel),
			OrigSize:   fi.Size,
			OutputSize: fi.Size,
		})
		total += fi.Size
	}
	b := &Batch{ID: batchID, Items: items, TotalIn: total, TotalOut: total, SkippedC: true}
	if v >= V1 {
		fmt.Printf("[batch ready (no-compress)] id=%d files=%d size=%.2fMB\n", batchID, len(files), mb(total))
	}
	return b
}

// ===================== main =====================

func main() {
	var (
		skipCompression     = flag.Bool("skip-compression", false, "Skip PNG->JXL compression")
		compressorName      = flag.String("compressor", "cjxl", "Compressor: cjxl | libjxl")
		batchSize           = flag.Int("batch-size", 200, "Images per batch")
		verboseLevel        = flag.Int("verbose-level", 1, "Verbosity 0..3")
		autoTune            = flag.Bool("auto-tune-batch-size", true, "Auto tune batch size based on compress vs upload time")
		inputDir            = flag.String("input-dir", "", "Directory of images to process")
		bucket              = flag.String("bucket-name", "", "Target S3 bucket (ignored if --mrap-arn provided)")
		mrapArn             = flag.String("mrap-arn", "", "S3 Multi-Region Access Point ARN (overrides --bucket-name)")
		prefix              = flag.String("prefix", "", "Target S3 prefix (path)")
		stageDir            = flag.String("stage-dir", "", "Optional staging dir for .jxl files (default: system temp)")
		deleteStaged        = flag.Bool("delete-staged", false, "Delete staged .jxl after successful upload (default false)")
		resume              = flag.Bool("resume", true, "Resume mode: record successful uploads and skip them on retry")
		stateFile           = flag.String("state-file", "", "Path to state file for resume (default: <stageDir>/upload_state.jsonl)")
		resumeHeadCheck     = flag.Bool("resume-remote-check", false, "On resume, HEAD S3 before uploading to skip already-present keys")
		accel               = flag.Bool("accel", false, "Enable S3 Transfer Acceleration (not used with MRAP)")
		partSizeMB          = flag.Int64("part-size-mb", 16, "Multipart part size in MiB (min 5)")
		uploaderConcurrency = flag.Int("uploader-concurrency", runtime.NumCPU(), "Multipart concurrency per large file")
		uploadParallel      = flag.Int("upload-parallel", 1, "Number of files uploaded in parallel within a batch")
		queueTimeout        = flag.Duration("queue-timeout", 60*time.Second, "Timeout for upload queue (for slow networks)")
	)
	flag.Parse()

	if *inputDir == "" || (*bucket == "" && *mrapArn == "") {
		flag.Usage()
		os.Exit(2)
	}
	v := Verbose(*verboseLevel)
	if *batchSize <= 0 {
		*batchSize = 1
	}
	if *stageDir == "" {
		*stageDir = filepath.Join(os.TempDir(), "data-bridge-stage")
	}
	must(ensureDir(*stageDir))
	if *stateFile == "" {
		*stateFile = filepath.Join(*stageDir, "upload_state.jsonl")
	}
	if *partSizeMB < 5 {
		*partSizeMB = 5
	}

	files, err := listFiles(*inputDir)
	must(err)
	if len(files) == 0 {
		log.Fatal("no files found")
	}

	ctx := context.Background()

	// State
	var st *State
	if *resume {
		st, err = loadState(*stateFile)
		must(err)
	}

	target := *bucket
	useArnRegion := false
	if *mrapArn != "" {
		target = *mrapArn
		useArnRegion = true
		if *accel {
			fmt.Println("[warn] --accel ignored when using MRAP")
		}
	}

	opts := S3Opts{
		Accel:               *accel && !useArnRegion,
		UseARNRegion:        useArnRegion,
		PartSize:            (*partSizeMB) * 1024 * 1024,
		UploaderConcurrency: *uploaderConcurrency,
	}
	upl, err := NewS3Uploader(ctx, target, *prefix, v, st, opts)
	must(err)
	upl.uploadParallel = *uploadParallel

	// Validate bucket connectivity and configuration
	if v >= V1 {
		fmt.Printf("[validate] testing bucket connectivity...\n")
	}
	if err := validateBucket(ctx, upl.client, target, *accel && !useArnRegion); err != nil {
		log.Fatalf("bucket validation failed: %v", err)
	}
	if v >= V1 {
		fmt.Printf("[validate] bucket connectivity verified ✓\n")
	}

	// Print S3 configuration for debugging
	if v >= V1 {
		fmt.Printf("[config] target=%s prefix=%s accel=%v mrap=%v part-size=%dMB uploader-concurrency=%d upload-parallel=%d\n",
			target, *prefix, opts.Accel, opts.UseARNRegion, *partSizeMB, *uploaderConcurrency, *uploadParallel)
	}
	if st != nil {
		st.verifyRemote = *resumeHeadCheck
		st.client = upl.client
		st.bucket = target
		st.prefix = *prefix
	}

	startAll := time.Now()
	var totalIn, totalOut int64
	var filesCount int64

	// Compressor selection
	var comp Compressor
	if !*skipCompression {
		switch strings.ToLower(*compressorName) {
		case "cjxl":
			if _, err := exec.LookPath("cjxl"); err != nil {
				log.Fatalf("compressor=cjxl but cjxl not in PATH: %v", err)
			}
			comp = CjxlCompressor{}
		case "libjxl":
			comp = LibjxlCompressor{} // may error at runtime if not built with -tags libjxl
		default:
			log.Fatalf("unknown compressor: %s", *compressorName)
		}
		if v >= V1 {
			fmt.Printf("Using compressor: %s\n", comp.Name())
		}
	}

	// Upload queue with larger capacity to handle slow networks
	uploadQueue := make(chan *Batch, 10)
	var uploadErr atomic.Value
	var wg sync.WaitGroup

	// Uploader goroutine: one batch at a time
	wg.Add(1)
	go func() {
		defer wg.Done()
		for b := range uploadQueue {
			if v >= V1 {
				fmt.Printf("[batch upload start] id=%d files=%d size=%.2fMB\n", b.ID, len(b.Items), mb(b.TotalOut))
			}

			// Add upload progress feedback for slow networks
			uploadStart := time.Now()
			err := upl.UploadBatch(ctx, b)
			uploadDur := time.Since(uploadStart)

			if err != nil {
				fmt.Printf("[upload error] batch=%d error=%v\n", b.ID, err)
				uploadErr.Store(err)
				return
			}

			// Show upload progress for slow uploads
			if v >= V1 && uploadDur > 30*time.Second {
				fmt.Printf("[upload progress] batch=%d completed in %s (%.2f MB/s)\n",
					b.ID, uploadDur, mb(b.TotalOut)/uploadDur.Seconds())
			}
			if v >= V1 {
				if v >= V2 {
					fmt.Printf("[batch uploaded] id=%d files=%d time=%s\n", b.ID, len(b.Items), b.UploadDur)
				} else {
					fmt.Printf("[batch uploaded] id=%d\n", b.ID)
				}
			}
			// cleanup staged files
			if !b.SkippedC && *deleteStaged {
				for _, it := range b.Items {
					_ = os.Remove(it.LocalPath)
				}
			}
			// totals
			filesCount += int64(len(b.Items))
			totalIn += b.TotalIn
			totalOut += b.TotalOut
			// auto-tune
			if *autoTune && !b.SkippedC {
				const skew = 1.25
				if b.CompDur > time.Duration(float64(b.UploadDur)*skew) {
					newSize := int(float64(*batchSize) * 0.8)
					if newSize < 50 {
						newSize = 50
					}
					if newSize != *batchSize {
						if v >= V1 {
							fmt.Printf("[tune] comp slower (%s vs %s) -> shrink batch %d -> %d\n", b.CompDur, b.UploadDur, *batchSize, newSize)
						}
						*batchSize = newSize
					}
				} else if b.UploadDur > time.Duration(float64(b.CompDur)*skew) {
					newSize := int(float64(*batchSize) * 1.2)
					if newSize > 2000 {
						newSize = 2000
					}
					if newSize != *batchSize {
						if v >= V1 {
							fmt.Printf("[tune] upload slower (%s vs %s) -> grow batch %d -> %d\n", b.UploadDur, b.CompDur, *batchSize, newSize)
						}
						*batchSize = newSize
					}
				}
			}
		}
	}()

	// Producer: prepare batches and queue
	batchID := 1
	lastProgress := time.Now()
	for i := 0; i < len(files); {
		if errVal := uploadErr.Load(); errVal != nil {
			log.Fatalf("upload failed: %v", errVal.(error))
		}

		end := i + *batchSize
		if end > len(files) {
			end = len(files)
		}
		slice := files[i:end]

		var b *Batch
		if *skipCompression {
			b = passthroughBatch(batchID, slice, v)
		} else {
			var err error
			b, err = compressBatch(ctx, comp, batchID, slice, *stageDir, v)
			if err != nil {
				log.Fatalf("compression failed: %v", err)
			}
		}

		// Send batch to upload queue with adaptive timeout and progress feedback
		select {
		case uploadQueue <- b:
			// Successfully queued
			if v >= V1 {
				queueLen := len(uploadQueue)
				if queueLen > 5 {
					fmt.Printf("[queue] batch %d queued (queue depth: %d)\n", b.ID, queueLen)
				}
			}
		case <-time.After(*queueTimeout):
			// Instead of failing, show warning and continue with reduced parallelism
			fmt.Printf("[warn] upload queue full for %s - uploads are very slow\n", *queueTimeout)
			fmt.Printf("[warn] continuing with current settings, but consider reducing --upload-parallel or --batch-size\n")

			// Try again with a longer timeout
			select {
			case uploadQueue <- b:
				if v >= V1 {
					fmt.Printf("[queue] batch %d queued after retry\n", b.ID)
				}
			case <-time.After(120 * time.Second):
				log.Fatalf("upload queue still full after 3 minutes - network may be too slow for current settings")
			}
		}

		// Show periodic progress for long-running operations
		if v >= V1 && time.Since(lastProgress) > 30*time.Second {
			progress := float64(i) / float64(len(files)) * 100
			fmt.Printf("[progress] %.1f%% complete (%d/%d files processed)\n", progress, i, len(files))
			lastProgress = time.Now()
		}

		i = end
		batchID++
	}
	close(uploadQueue)
	wg.Wait()

	if errVal := uploadErr.Load(); errVal != nil {
		log.Fatalf("upload failed: %v", errVal.(error))
	}

	durAll := time.Since(startAll)
	avgComp := 1.0
	if totalIn > 0 {
		avgComp = float64(totalOut) / float64(totalIn)
	}
	fmt.Printf("SUMMARY | files=%d in=%.2fMB out=%.2fMB ratio=%.3f time=%s throughput=%.2f MB/s compression=%v final_batch=%d\n",
		filesCount, mb(totalIn), mb(totalOut), avgComp, durAll,
		mb(totalOut)/durAll.Seconds(), !*skipCompression, *batchSize)
}
