package broker

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

type MediaProcessorRunner interface {
	FastStartRemux(ctx context.Context, inputPath, outputPath string) error
	TranscodeMP4(ctx context.Context, inputPath, outputPath string) error
	IsWebMP4(ctx context.Context, inputPath string) (bool, error)
	VideoDimensions(ctx context.Context, inputPath string) (int, int, error)
	ExtractThumbnail(ctx context.Context, inputPath, outputPath string) error
}

type FFmpegMediaProcessor struct {
	FFmpegPath  string
	FFprobePath string
}

func (r FFmpegMediaProcessor) FastStartRemux(ctx context.Context, inputPath, outputPath string) error {
	command := exec.CommandContext(ctx, r.ffmpegPath(),
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
		return fmt.Errorf("ffmpeg remux failed: %w: %s", err, strings.TrimSpace(string(output)))
	}
	return nil
}

func (r FFmpegMediaProcessor) TranscodeMP4(ctx context.Context, inputPath, outputPath string) error {
	command := exec.CommandContext(ctx, r.ffmpegPath(),
		"-hide_banner",
		"-y",
		"-i", inputPath,
		"-map", "0:v:0",
		"-map", "0:a:0?",
		"-c:v", "h264_nvenc",
		"-preset", "p4",
		"-cq", "23",
		"-pix_fmt", "yuv420p",
		"-c:a", "aac",
		"-b:a", "160k",
		"-movflags", "+faststart",
		outputPath,
	)
	output, err := command.CombinedOutput()
	if err != nil {
		return fmt.Errorf("ffmpeg transcode failed: %w: %s", err, strings.TrimSpace(string(output)))
	}
	return nil
}

func (r FFmpegMediaProcessor) VideoDimensions(ctx context.Context, inputPath string) (int, int, error) {
	command := exec.CommandContext(ctx, r.ffprobePath(),
		"-v", "error",
		"-select_streams", "v:0",
		"-show_entries", "stream=width,height",
		"-of", "csv=p=0",
		inputPath,
	)
	output, err := command.Output()
	if err != nil {
		return 0, 0, fmt.Errorf("ffprobe dimensions failed: %w", err)
	}
	return parseVideoDimensions(string(output))
}

func parseVideoDimensions(output string) (int, int, error) {
	fields := strings.Split(strings.TrimSpace(output), ",")
	if len(fields) < 2 {
		return 0, 0, fmt.Errorf("unexpected ffprobe dimensions output %q", strings.TrimSpace(output))
	}
	width, err := strconv.Atoi(fields[0])
	if err != nil {
		return 0, 0, fmt.Errorf("unexpected ffprobe width %q: %w", fields[0], err)
	}
	height, err := strconv.Atoi(fields[1])
	if err != nil {
		return 0, 0, fmt.Errorf("unexpected ffprobe height %q: %w", fields[1], err)
	}
	return width, height, nil
}

func (r FFmpegMediaProcessor) ExtractThumbnail(ctx context.Context, inputPath, outputPath string) error {
	var lastErr error
	for _, seek := range []string{"1", "0"} {
		command := exec.CommandContext(ctx, r.ffmpegPath(),
			"-hide_banner",
			"-y",
			"-ss", seek,
			"-i", inputPath,
			"-frames:v", "1",
			"-vf", "scale='min(1280,iw)':-2",
			"-q:v", "4",
			outputPath,
		)
		output, err := command.CombinedOutput()
		if err == nil {
			if info, statErr := os.Stat(outputPath); statErr == nil && info.Size() > 0 {
				return nil
			}
			err = fmt.Errorf("thumbnail output is empty")
		}
		lastErr = fmt.Errorf("ffmpeg thumbnail failed: %w: %s", err, strings.TrimSpace(string(output)))
	}
	return lastErr
}

func (r FFmpegMediaProcessor) IsWebMP4(ctx context.Context, inputPath string) (bool, error) {
	command := exec.CommandContext(ctx, r.ffprobePath(),
		"-v", "error",
		"-show_entries", "stream=codec_type,codec_name",
		"-of", "json",
		inputPath,
	)
	output, err := command.Output()
	if err != nil {
		return false, fmt.Errorf("ffprobe failed: %w", err)
	}
	var probe struct {
		Streams []struct {
			CodecType string `json:"codec_type"`
			CodecName string `json:"codec_name"`
		} `json:"streams"`
	}
	if err := json.Unmarshal(output, &probe); err != nil {
		return false, err
	}
	hasVideo := false
	for _, stream := range probe.Streams {
		switch stream.CodecType {
		case "video":
			hasVideo = true
			if stream.CodecName != "h264" {
				return false, nil
			}
		case "audio":
			if stream.CodecName != "aac" {
				return false, nil
			}
		}
	}
	return hasVideo, nil
}

func (r FFmpegMediaProcessor) ffmpegPath() string {
	path := strings.TrimSpace(r.FFmpegPath)
	if path == "" {
		return "ffmpeg"
	}
	return path
}

func (r FFmpegMediaProcessor) ffprobePath() string {
	path := strings.TrimSpace(r.FFprobePath)
	if path == "" {
		return "ffprobe"
	}
	return path
}

type Transcoder struct {
	cfg      Config
	store    ObjectStore
	metadata MetadataStore
	runner   MediaProcessorRunner
	logger   *slog.Logger
	workerID string
}

func NewTranscoder(cfg Config, store ObjectStore, metadata MetadataStore, runner MediaProcessorRunner, logger *slog.Logger) *Transcoder {
	if runner == nil {
		runner = FFmpegMediaProcessor{FFmpegPath: cfg.FFmpegPath}
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
			t.logger.Error("processor loop failed", "error", err)
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
	t.logger.Info("claimed processing job", "jobID", job.ID, "profile", job.Profile, "stagingPath", job.StagingPath)
	if err := t.processJob(ctx, job); err != nil {
		message := truncateError(err)
		t.logger.Error("processing job failed", "jobID", job.ID, "error", err)
		if failErr := t.metadata.FailProcessingJob(ctx, job.ID, message); failErr != nil {
			return true, fmt.Errorf("job failed with %q and failure status update failed: %w", message, failErr)
		}
		if err := removeIfExists(job.StagingPath); err != nil {
			t.logger.Warn("failed to remove staged upload after failure", "jobID", job.ID, "path", job.StagingPath, "error", err)
		}
		return true, nil
	}
	t.logger.Info("completed processing job", "jobID", job.ID)
	return true, nil
}

func (t *Transcoder) processJob(ctx context.Context, job ProcessingJob) error {
	if err := ValidateProcessingProfile(job.Profile); err != nil {
		return err
	}
	if strings.TrimSpace(job.StagingPath) == "" {
		return fmt.Errorf("processing job %q has no staged upload", job.ID)
	}
	defer removeIfExists(job.StagingPath)

	if strings.TrimSpace(job.SourceSHA256) != "" {
		reused, err := t.reuseDerivative(ctx, job)
		if err != nil {
			return err
		}
		if reused {
			return nil
		}
	}

	final := processedFile{
		Path:        job.StagingPath,
		ContentType: strings.TrimSpace(job.SourceType),
		Extension:   ExtensionFor(job.DisplayFilename, job.SourceType),
	}
	if final.ContentType == "" {
		final.ContentType = "application/octet-stream"
	}
	if job.Profile == ProcessingProfileMP4Web {
		video, err := t.normalizeVideo(ctx, job)
		if err != nil {
			return err
		}
		t.enrichVideoMetadata(ctx, job, &video)
		final = video
	}
	if final.WorkDir != "" {
		defer os.RemoveAll(final.WorkDir)
	}

	current, found, err := t.metadata.GetProcessingJob(ctx, job.ID, "")
	if err != nil {
		return err
	}
	if !found || current.Status == ProcessingStatusCanceled {
		return nil
	}
	if current.Status != ProcessingStatusRunning {
		return fmt.Errorf("processing job %q is %q, not running", job.ID, current.Status)
	}

	sha256Hex, size, err := hashFile(final.Path)
	if err != nil {
		return err
	}
	objectKey := GenerateObjectKey(sha256Hex, final.Extension)
	object, found, err := t.readyObject(ctx, sha256Hex)
	if err != nil {
		return err
	}
	uploadedObjectKey := ""
	if !found {
		file, err := os.Open(final.Path)
		if err != nil {
			return err
		}
		metadata, putErr := t.store.PutObject(ctx, objectKey, final.ContentType, size, file)
		closeErr := file.Close()
		if putErr != nil {
			return putErr
		}
		if closeErr != nil {
			return closeErr
		}
		if metadata.ContentLength > 0 {
			size = metadata.ContentLength
		}
		uploadedObjectKey = objectKey
		object = StoredObject{
			SHA256:        sha256Hex,
			ObjectKey:     objectKey,
			Size:          size,
			ContentType:   final.ContentType,
			Extension:     final.Extension,
			FirstFilename: job.DisplayFilename,
			Uploader:      job.Owner,
			Status:        "ready",
			Width:         final.Width,
			Height:        final.Height,
		}
		if final.ThumbnailPath != "" {
			thumbnailKey := GenerateObjectKey(sha256Hex, ".jpg")
			if err := t.uploadThumbnail(ctx, thumbnailKey, final.ThumbnailPath); err != nil {
				t.logger.Warn("failed to upload video thumbnail", "jobID", job.ID, "thumbnailKey", thumbnailKey, "error", err)
			} else {
				object.ThumbnailKey = thumbnailKey
			}
		}
	}

	current, found, err = t.metadata.GetProcessingJob(ctx, job.ID, "")
	if err != nil {
		return err
	}
	if !found || current.Status == ProcessingStatusCanceled {
		if uploadedObjectKey != "" {
			if deleteErr := t.store.DeleteObject(ctx, uploadedObjectKey); deleteErr != nil {
				t.logger.Warn("failed to remove uploaded object for canceled job", "jobID", job.ID, "objectKey", uploadedObjectKey, "error", deleteErr)
			}
		}
		return nil
	}
	if current.Status != ProcessingStatusRunning {
		return fmt.Errorf("processing job %q is %q, not running", job.ID, current.Status)
	}

	return t.metadata.CompleteProcessingJob(ctx, job.ID, object, ShareAlias{
		Slug:            job.AliasSlug,
		ObjectSHA256:    object.SHA256,
		ObjectKey:       object.ObjectKey,
		Owner:           job.Owner,
		DisplayFilename: job.DisplayFilename,
		Visibility:      "public",
		Status:          AliasStatusReady,
	})
}

type processedFile struct {
	Path          string
	ContentType   string
	Extension     string
	WorkDir       string
	Width         int
	Height        int
	ThumbnailPath string
}

func (t *Transcoder) normalizeVideo(ctx context.Context, job ProcessingJob) (processedFile, error) {
	workDir, err := os.MkdirTemp(t.cfg.TranscoderWorkDir, "job-"+safeFilename(job.ID)+"-")
	if err != nil {
		return processedFile{}, err
	}

	remuxPath := filepath.Join(workDir, "remux.mp4")
	if err := t.runner.FastStartRemux(ctx, job.StagingPath, remuxPath); err == nil {
		webMP4, probeErr := t.runner.IsWebMP4(ctx, remuxPath)
		if probeErr == nil && webMP4 {
			finalPath := filepath.Join(workDir, "final.mp4")
			if err := copyFile(remuxPath, finalPath); err != nil {
				_ = os.RemoveAll(workDir)
				return processedFile{}, err
			}
			return processedFile{Path: finalPath, ContentType: "video/mp4", Extension: ".mp4", WorkDir: workDir}, nil
		}
		if probeErr != nil {
			t.logger.Warn("failed to probe remuxed MP4, falling back to transcode", "jobID", job.ID, "error", probeErr)
		}
	} else {
		t.logger.Info("fast-start remux failed, falling back to transcode", "jobID", job.ID, "error", err)
	}

	outputPath := filepath.Join(workDir, "transcoded.mp4")
	if err := t.runner.TranscodeMP4(ctx, job.StagingPath, outputPath); err != nil {
		_ = os.RemoveAll(workDir)
		return processedFile{}, err
	}
	finalPath := filepath.Join(workDir, "final.mp4")
	if err := copyFile(outputPath, finalPath); err != nil {
		_ = os.RemoveAll(workDir)
		return processedFile{}, err
	}
	return processedFile{Path: finalPath, ContentType: "video/mp4", Extension: ".mp4", WorkDir: workDir}, nil
}

func (t *Transcoder) enrichVideoMetadata(ctx context.Context, job ProcessingJob, final *processedFile) {
	width, height, err := t.runner.VideoDimensions(ctx, final.Path)
	if err != nil {
		t.logger.Warn("failed to probe video dimensions", "jobID", job.ID, "error", err)
	} else {
		final.Width = width
		final.Height = height
	}
	if final.WorkDir == "" {
		return
	}
	thumbnailPath := filepath.Join(final.WorkDir, "thumbnail.jpg")
	if err := t.runner.ExtractThumbnail(ctx, final.Path, thumbnailPath); err != nil {
		t.logger.Warn("failed to extract video thumbnail", "jobID", job.ID, "error", err)
		return
	}
	if info, err := os.Stat(thumbnailPath); err != nil || info.Size() == 0 {
		return
	}
	final.ThumbnailPath = thumbnailPath
}

func (t *Transcoder) uploadThumbnail(ctx context.Context, thumbnailKey, thumbnailPath string) error {
	file, err := os.Open(thumbnailPath)
	if err != nil {
		return err
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil {
		return err
	}
	_, err = t.store.PutObject(ctx, thumbnailKey, "image/jpeg", info.Size(), file)
	return err
}

func (t *Transcoder) reuseDerivative(ctx context.Context, job ProcessingJob) (bool, error) {
	derived, found, err := t.metadata.GetDerivedObject(ctx, job.SourceSHA256, job.Profile)
	if err != nil {
		return false, err
	}
	if !found {
		return false, nil
	}
	object, ready, err := t.readyObject(ctx, derived.SHA256)
	if err != nil {
		return false, err
	}
	if !ready {
		return false, nil
	}

	current, found, err := t.metadata.GetProcessingJob(ctx, job.ID, "")
	if err != nil {
		return false, err
	}
	if !found || current.Status == ProcessingStatusCanceled {
		return true, nil
	}
	if current.Status != ProcessingStatusRunning {
		return false, fmt.Errorf("processing job %q is %q, not running", job.ID, current.Status)
	}

	t.logger.Info("reusing existing derivative for duplicate source",
		"jobID", job.ID,
		"profile", job.Profile,
		"sourceSHA256", job.SourceSHA256,
		"objectSHA256", object.SHA256,
	)
	err = t.metadata.CompleteProcessingJob(ctx, job.ID, object, ShareAlias{
		Slug:            job.AliasSlug,
		ObjectSHA256:    object.SHA256,
		ObjectKey:       object.ObjectKey,
		Owner:           job.Owner,
		DisplayFilename: job.DisplayFilename,
		Visibility:      "public",
		Status:          AliasStatusReady,
	})
	if err != nil {
		return false, err
	}
	return true, nil
}

func (t *Transcoder) readyObject(ctx context.Context, sha256Hex string) (StoredObject, bool, error) {
	object, found, err := t.metadata.GetObject(ctx, sha256Hex)
	if err != nil || !found {
		return object, found, err
	}
	if object.Status != "ready" {
		return StoredObject{}, false, nil
	}
	if _, err := t.store.HeadObject(ctx, object.ObjectKey); err != nil {
		if markErr := t.metadata.MarkObjectUnavailable(ctx, object.SHA256, "missing"); markErr != nil {
			return StoredObject{}, false, markErr
		}
		return StoredObject{}, false, nil
	}
	return object, true, nil
}

func copyFile(sourcePath, targetPath string) error {
	source, err := os.Open(sourcePath)
	if err != nil {
		return err
	}
	defer source.Close()
	target, err := os.OpenFile(targetPath, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	_, copyErr := io.Copy(target, source)
	closeErr := target.Close()
	if copyErr != nil {
		return copyErr
	}
	return closeErr
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

func removeIfExists(path string) error {
	if strings.TrimSpace(path) == "" {
		return nil
	}
	if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
		return err
	}
	return nil
}

func hostnameWorkerID() string {
	hostname, err := os.Hostname()
	if err != nil || strings.TrimSpace(hostname) == "" {
		return "b2-share-processor"
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
