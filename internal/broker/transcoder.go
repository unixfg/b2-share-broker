package broker

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

type RemuxRunner interface {
	FastStartRemux(ctx context.Context, inputPath, outputPath string) error
}

type FFmpegRemuxRunner struct {
	Path string
}

func (r FFmpegRemuxRunner) FastStartRemux(ctx context.Context, inputPath, outputPath string) error {
	path := strings.TrimSpace(r.Path)
	if path == "" {
		path = "ffmpeg"
	}
	command := exec.CommandContext(ctx, path,
		"-hide_banner",
		"-y",
		"-i", inputPath,
		"-map", "0",
		"-c", "copy",
		"-movflags", "+faststart",
		outputPath,
	)
	output, err := command.CombinedOutput()
	if err != nil {
		return fmt.Errorf("ffmpeg failed: %w: %s", err, strings.TrimSpace(string(output)))
	}
	return nil
}

type Transcoder struct {
	cfg      Config
	store    ObjectStore
	metadata MetadataStore
	runner   RemuxRunner
	logger   *slog.Logger
	workerID string
}

func NewTranscoder(cfg Config, store ObjectStore, metadata MetadataStore, runner RemuxRunner, logger *slog.Logger) *Transcoder {
	if runner == nil {
		runner = FFmpegRemuxRunner{Path: cfg.FFmpegPath}
	}
	if logger == nil {
		logger = slog.Default()
	}
	return &Transcoder{
		cfg:      cfg,
		store:    store,
		metadata: metadata,
		runner:   runner,
		logger:   logger,
		workerID: hostnameWorkerID(),
	}
}

func (t *Transcoder) Run(ctx context.Context) error {
	ticker := time.NewTicker(t.cfg.TranscoderPoll)
	defer ticker.Stop()
	for {
		processed, err := t.ProcessNext(ctx)
		if err != nil {
			t.logger.Error("transcoder loop failed", "error", err)
		}
		if processed {
			continue
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C:
		}
	}
}

func (t *Transcoder) ProcessNext(ctx context.Context) (bool, error) {
	job, found, err := t.metadata.ClaimNextProcessingJob(ctx, t.workerID)
	if err != nil || !found {
		return found, err
	}
	t.logger.Info("claimed processing job", "jobID", job.ID, "profile", job.Profile, "source", job.SourceObjectKey)
	if err := t.processJob(ctx, job); err != nil {
		message := truncateError(err)
		t.logger.Error("processing job failed", "jobID", job.ID, "error", err)
		if failErr := t.metadata.FailProcessingJob(ctx, job.ID, message); failErr != nil {
			return true, fmt.Errorf("job failed with %q and failure status update failed: %w", message, failErr)
		}
		return true, nil
	}
	t.logger.Info("completed processing job", "jobID", job.ID)
	return true, nil
}

func (t *Transcoder) processJob(ctx context.Context, job ProcessingJob) error {
	if job.Profile != ProcessingProfileMP4FaststartRemux {
		return ValidateProcessingProfile(job.Profile)
	}
	if !strings.EqualFold(job.SourceType, "video/mp4") && !strings.HasSuffix(strings.ToLower(job.SourceObjectKey), ".mp4") {
		return fmt.Errorf("profile %q only supports MP4 objects", job.Profile)
	}

	workDir, err := os.MkdirTemp(t.cfg.TranscoderWorkDir, "job-"+safeFilename(job.ID)+"-")
	if err != nil {
		return err
	}
	defer os.RemoveAll(workDir)

	inputPath := filepath.Join(workDir, "input.mp4")
	outputPath := filepath.Join(workDir, "output.mp4")
	input, err := os.Create(inputPath)
	if err != nil {
		return err
	}
	if err := t.store.DownloadObject(ctx, job.SourceObjectKey, input); err != nil {
		input.Close()
		return err
	}
	if err := input.Close(); err != nil {
		return err
	}
	if err := t.runner.FastStartRemux(ctx, inputPath, outputPath); err != nil {
		return err
	}

	sha256Hex, size, err := hashFile(outputPath)
	if err != nil {
		return err
	}
	objectKey := GenerateObjectKey(t.cfg.ObjectPrefix, sha256Hex, ".mp4")
	object, found, err := t.metadata.GetObject(ctx, sha256Hex)
	if err != nil {
		return err
	}
	if !found {
		output, err := os.Open(outputPath)
		if err != nil {
			return err
		}
		metadata, putErr := t.store.PutObject(ctx, objectKey, "video/mp4", size, output)
		closeErr := output.Close()
		if putErr != nil {
			return putErr
		}
		if closeErr != nil {
			return closeErr
		}
		if metadata.ContentLength > 0 {
			size = metadata.ContentLength
		}
		object = StoredObject{
			SHA256:        sha256Hex,
			ObjectKey:     objectKey,
			Size:          size,
			ContentType:   "video/mp4",
			Extension:     ".mp4",
			FirstFilename: job.DisplayFilename,
			Uploader:      job.Owner,
		}
	}

	return t.metadata.CompleteProcessingJob(ctx, job.ID, object, ShareAlias{
		Slug:            job.AliasSlug,
		ObjectSHA256:    object.SHA256,
		ObjectKey:       object.ObjectKey,
		Owner:           job.Owner,
		DisplayFilename: job.DisplayFilename,
		Visibility:      "public",
	})
}

func hashFile(path string) (string, int64, error) {
	file, err := os.Open(path)
	if err != nil {
		return "", 0, err
	}
	defer file.Close()
	hash := sha256.New()
	size, err := io.Copy(hash, file)
	if err != nil {
		return "", 0, err
	}
	return hex.EncodeToString(hash.Sum(nil)), size, nil
}

func hostnameWorkerID() string {
	hostname, err := os.Hostname()
	if err != nil || strings.TrimSpace(hostname) == "" {
		return "b2-share-transcoder"
	}
	return hostname
}

func safeFilename(value string) string {
	var builder strings.Builder
	for _, r := range value {
		if (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9') || r == '-' {
			builder.WriteRune(r)
		}
	}
	if builder.Len() == 0 {
		return "job"
	}
	return builder.String()
}

func truncateError(err error) string {
	message := strings.TrimSpace(err.Error())
	if len(message) > 800 {
		return message[:800]
	}
	return message
}
