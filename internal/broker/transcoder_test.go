package broker

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

type fakeMediaRunner struct {
	remuxOutput     []byte
	transcodeOutput []byte
	webMP4          bool
	remuxErr        error
	transcodeErr    error
	width           int
	height          int
	dimensionsErr   error
	thumbnailOutput []byte
	thumbnailErr    error
}

func (r fakeMediaRunner) FastStartRemux(_ context.Context, _, outputPath string) error {
	if r.remuxErr != nil {
		return r.remuxErr
	}
	return os.WriteFile(outputPath, r.remuxOutput, 0o600)
}

func (r fakeMediaRunner) TranscodeMP4(_ context.Context, _, outputPath string) error {
	if r.transcodeErr != nil {
		return r.transcodeErr
	}
	return os.WriteFile(outputPath, r.transcodeOutput, 0o600)
}

func (r fakeMediaRunner) IsWebMP4(context.Context, string) (bool, error) {
	return r.webMP4, nil
}

func (r fakeMediaRunner) VideoDimensions(context.Context, string) (int, int, error) {
	return r.width, r.height, r.dimensionsErr
}

func (r fakeMediaRunner) ExtractThumbnail(_ context.Context, _, outputPath string) error {
	if r.thumbnailErr != nil {
		return r.thumbnailErr
	}
	return os.WriteFile(outputPath, r.thumbnailOutput, 0o600)
}

func TestProcessorFinalizesNonVideoUpload(t *testing.T) {
	cfg := testConfig(t)
	cfg.TranscoderWorkDir = t.TempDir()
	stagingPath := filepath.Join(cfg.StagingDir, "job-1.txt.upload")
	if err := os.WriteFile(stagingPath, []byte("hello"), 0o600); err != nil {
		t.Fatal(err)
	}
	metadata := newMemoryMetadata()
	metadata.aliases["mine.txt"] = ShareAlias{Slug: "mine.txt", Owner: "user-1", DisplayFilename: "mine.txt", Visibility: "public", Status: AliasStatusPending}
	metadata.jobs["job-1"] = ProcessingJob{ID: "job-1", Owner: "user-1", AliasSlug: "mine.txt", StagingPath: stagingPath, Profile: ProcessingProfileUploadFinalize, Status: ProcessingStatusQueued, DisplayFilename: "mine.txt", SourceType: "text/plain"}
	store := &fakeStore{}
	transcoder := NewTranscoder(cfg, store, metadata, fakeMediaRunner{}, slog.Default())

	processed, err := transcoder.ProcessNext(context.Background())

	if err != nil {
		t.Fatal(err)
	}
	if !processed {
		t.Fatal("expected a job to be processed")
	}
	targetSHA := sha256Hex([]byte("hello"))
	targetKey := targetSHA[:2] + "/" + targetSHA + ".txt"
	if metadata.jobs["job-1"].Status != ProcessingStatusCompleted {
		t.Fatalf("job = %#v", metadata.jobs["job-1"])
	}
	if metadata.aliases["mine.txt"].ObjectSHA256 != targetSHA || metadata.aliases["mine.txt"].Status != AliasStatusReady {
		t.Fatalf("alias = %#v", metadata.aliases["mine.txt"])
	}
	if store.putKey != targetKey || store.putType != "text/plain" {
		t.Fatalf("put key/type = %q/%q", store.putKey, store.putType)
	}
	if _, err := os.Stat(stagingPath); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("staging file still exists or stat failed unexpectedly: %v", err)
	}
}

func TestProcessorCompletesRunningJobAtRenamedAlias(t *testing.T) {
	cfg := testConfig(t)
	cfg.TranscoderWorkDir = t.TempDir()
	stagingPath := filepath.Join(cfg.StagingDir, "job-1.txt.upload")
	if err := os.WriteFile(stagingPath, []byte("hello"), 0o600); err != nil {
		t.Fatal(err)
	}
	metadata := newMemoryMetadata()
	metadata.aliases["old-name.txt"] = ShareAlias{Slug: "old-name.txt", Owner: "user-1", DisplayFilename: "old-name.txt", Visibility: "public", Status: AliasStatusPending}
	metadata.jobs["job-1"] = ProcessingJob{ID: "job-1", Owner: "user-1", AliasSlug: "old-name.txt", StagingPath: stagingPath, Profile: ProcessingProfileUploadFinalize, Status: ProcessingStatusQueued, DisplayFilename: "source.txt", SourceType: "text/plain"}
	store := &fakeStore{}
	transcoder := NewTranscoder(cfg, store, metadata, fakeMediaRunner{}, slog.Default())
	job, found, err := metadata.ClaimNextProcessingJob(context.Background(), "worker")
	if err != nil || !found {
		t.Fatalf("claimed job = %#v, found = %t, err = %v", job, found, err)
	}
	if _, found, err := metadata.RenameAlias(context.Background(), "old-name.txt", "user-1", "new-name.txt"); err != nil || !found {
		t.Fatalf("rename found = %t, err = %v", found, err)
	}

	if err := transcoder.processJob(context.Background(), job); err != nil {
		t.Fatal(err)
	}

	targetSHA := sha256Hex([]byte("hello"))
	completed, found, err := metadata.GetProcessingJob(context.Background(), "job-1", "user-1")
	if err != nil || !found || completed.AliasSlug != "new-name.txt" || metadata.aliases["new-name.txt"].ObjectSHA256 != targetSHA {
		t.Fatalf("job = %#v, found = %t, err = %v, alias = %#v", completed, found, err, metadata.aliases["new-name.txt"])
	}
	if old := metadata.aliases["old-name.txt"]; old.RedirectToSlug != "new-name.txt" || old.ObjectSHA256 != "" {
		t.Fatalf("old alias = %#v", old)
	}
}

func TestProcessorRemuxesWebMP4BeforeHashAndUpload(t *testing.T) {
	cfg := testConfig(t)
	cfg.TranscoderWorkDir = t.TempDir()
	stagingPath := filepath.Join(cfg.StagingDir, "job-1.mp4.upload")
	if err := os.WriteFile(stagingPath, []byte("source"), 0o600); err != nil {
		t.Fatal(err)
	}
	output := []byte("faststart-output")
	metadata := newMemoryMetadata()
	metadata.aliases["mine.mp4"] = ShareAlias{Slug: "mine.mp4", Owner: "user-1", DisplayFilename: "input.mov", Visibility: "public", Status: AliasStatusPending}
	metadata.jobs["job-1"] = ProcessingJob{ID: "job-1", Owner: "user-1", AliasSlug: "mine.mp4", StagingPath: stagingPath, Profile: ProcessingProfileMP4Web, Status: ProcessingStatusQueued, DisplayFilename: "input.mov", SourceType: "video/quicktime"}
	store := &fakeStore{}
	transcoder := NewTranscoder(cfg, store, metadata, fakeMediaRunner{remuxOutput: output, webMP4: true}, slog.Default())

	processed, err := transcoder.ProcessNext(context.Background())

	if err != nil {
		t.Fatal(err)
	}
	if !processed {
		t.Fatal("expected a job to be processed")
	}
	targetSHA := sha256Hex(output)
	targetKey := targetSHA[:2] + "/" + targetSHA + ".mp4"
	if store.putKey != targetKey || store.putType != "video/mp4" {
		t.Fatalf("put key/type = %q/%q", store.putKey, store.putType)
	}
	if metadata.aliases["mine.mp4"].ObjectSHA256 != targetSHA {
		t.Fatalf("alias = %#v", metadata.aliases["mine.mp4"])
	}
	if metadata.objects[targetSHA].ContentType != "video/mp4" {
		t.Fatalf("object = %#v", metadata.objects[targetSHA])
	}
}

func TestProcessorTranscodesWhenRemuxIsNotWebMP4(t *testing.T) {
	cfg := testConfig(t)
	cfg.TranscoderWorkDir = t.TempDir()
	stagingPath := filepath.Join(cfg.StagingDir, "job-1.mp4.upload")
	if err := os.WriteFile(stagingPath, []byte("source"), 0o600); err != nil {
		t.Fatal(err)
	}
	transcoded := []byte("transcoded-output")
	metadata := newMemoryMetadata()
	metadata.aliases["mine.mp4"] = ShareAlias{Slug: "mine.mp4", Owner: "user-1", DisplayFilename: "input.mkv", Visibility: "public", Status: AliasStatusPending}
	metadata.jobs["job-1"] = ProcessingJob{ID: "job-1", Owner: "user-1", AliasSlug: "mine.mp4", StagingPath: stagingPath, Profile: ProcessingProfileMP4Web, Status: ProcessingStatusQueued, DisplayFilename: "input.mkv", SourceType: "video/x-matroska"}
	store := &fakeStore{}
	transcoder := NewTranscoder(cfg, store, metadata, fakeMediaRunner{remuxOutput: []byte("remux"), transcodeOutput: transcoded, webMP4: false}, slog.Default())

	processed, err := transcoder.ProcessNext(context.Background())

	if err != nil {
		t.Fatal(err)
	}
	if !processed {
		t.Fatal("expected a job to be processed")
	}
	targetSHA := sha256Hex(transcoded)
	if metadata.aliases["mine.mp4"].ObjectSHA256 != targetSHA {
		t.Fatalf("alias = %#v", metadata.aliases["mine.mp4"])
	}
}

func TestProcessorReusesDerivativeForDuplicateSource(t *testing.T) {
	cfg := testConfig(t)
	cfg.TranscoderWorkDir = t.TempDir()
	stagingPath := filepath.Join(cfg.StagingDir, "job-2.mp4.upload")
	if err := os.WriteFile(stagingPath, []byte("source"), 0o600); err != nil {
		t.Fatal(err)
	}
	sourceSHA := sha256Hex([]byte("source"))
	targetSHA := sha256Hex([]byte("existing-mp4"))
	targetKey := targetSHA[:2] + "/" + targetSHA + ".mp4"
	metadata := newMemoryMetadata()
	metadata.objects[targetSHA] = StoredObject{SHA256: targetSHA, ObjectKey: targetKey, Size: 12, ContentType: "video/mp4", Extension: ".mp4", Status: "ready"}
	metadata.derivatives[sourceSHA+"|"+ProcessingProfileMP4Web] = ObjectDerivative{SourceSHA256: sourceSHA, TargetSHA256: targetSHA, Profile: ProcessingProfileMP4Web, JobID: "job-1"}
	metadata.aliases["mine.mp4"] = ShareAlias{Slug: "mine.mp4", Owner: "user-1", DisplayFilename: "input.mkv", Visibility: "public", Status: AliasStatusPending}
	metadata.jobs["job-2"] = ProcessingJob{ID: "job-2", Owner: "user-1", AliasSlug: "mine.mp4", SourceSHA256: sourceSHA, StagingPath: stagingPath, Profile: ProcessingProfileMP4Web, Status: ProcessingStatusQueued, DisplayFilename: "input.mkv", SourceType: "video/x-matroska"}
	store := &fakeStore{objects: map[string][]byte{targetKey: []byte("existing-mp4")}}
	runner := fakeMediaRunner{remuxErr: errors.New("remux must not run"), transcodeErr: errors.New("transcode must not run")}
	transcoder := NewTranscoder(cfg, store, metadata, runner, slog.Default())

	processed, err := transcoder.ProcessNext(context.Background())

	if err != nil {
		t.Fatal(err)
	}
	if !processed {
		t.Fatal("expected a job to be processed")
	}
	if metadata.jobs["job-2"].Status != ProcessingStatusCompleted {
		t.Fatalf("job = %#v", metadata.jobs["job-2"])
	}
	if metadata.aliases["mine.mp4"].ObjectSHA256 != targetSHA || metadata.aliases["mine.mp4"].Status != AliasStatusReady {
		t.Fatalf("alias = %#v", metadata.aliases["mine.mp4"])
	}
	if store.putKey != "" {
		t.Fatalf("unexpected upload to %q", store.putKey)
	}
	if _, err := os.Stat(stagingPath); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("staging file still exists or stat failed unexpectedly: %v", err)
	}
}

func TestProcessorTranscodesWhenDerivativeTargetIsUnavailable(t *testing.T) {
	cfg := testConfig(t)
	cfg.TranscoderWorkDir = t.TempDir()
	stagingPath := filepath.Join(cfg.StagingDir, "job-2.mp4.upload")
	if err := os.WriteFile(stagingPath, []byte("source"), 0o600); err != nil {
		t.Fatal(err)
	}
	sourceSHA := sha256Hex([]byte("source"))
	targetSHA := sha256Hex([]byte("missing-mp4"))
	targetKey := targetSHA[:2] + "/" + targetSHA + ".mp4"
	transcoded := []byte("transcoded-output")
	metadata := newMemoryMetadata()
	metadata.objects[targetSHA] = StoredObject{SHA256: targetSHA, ObjectKey: targetKey, Size: 11, ContentType: "video/mp4", Extension: ".mp4", Status: "ready"}
	metadata.derivatives[sourceSHA+"|"+ProcessingProfileMP4Web] = ObjectDerivative{SourceSHA256: sourceSHA, TargetSHA256: targetSHA, Profile: ProcessingProfileMP4Web, JobID: "job-1"}
	metadata.aliases["mine.mp4"] = ShareAlias{Slug: "mine.mp4", Owner: "user-1", DisplayFilename: "input.mkv", Visibility: "public", Status: AliasStatusPending}
	metadata.jobs["job-2"] = ProcessingJob{ID: "job-2", Owner: "user-1", AliasSlug: "mine.mp4", SourceSHA256: sourceSHA, StagingPath: stagingPath, Profile: ProcessingProfileMP4Web, Status: ProcessingStatusQueued, DisplayFilename: "input.mkv", SourceType: "video/x-matroska"}
	store := &fakeStore{headErr: errors.New("missing")}
	transcoder := NewTranscoder(cfg, store, metadata, fakeMediaRunner{remuxOutput: []byte("remux"), transcodeOutput: transcoded}, slog.Default())

	processed, err := transcoder.ProcessNext(context.Background())

	if err != nil {
		t.Fatal(err)
	}
	if !processed {
		t.Fatal("expected a job to be processed")
	}
	if metadata.aliases["mine.mp4"].ObjectSHA256 != sha256Hex(transcoded) {
		t.Fatalf("alias = %#v", metadata.aliases["mine.mp4"])
	}
	if len(metadata.unavailable) != 1 || metadata.unavailable[0] != targetSHA {
		t.Fatalf("unavailable = %#v", metadata.unavailable)
	}
}

func TestProcessorEnrichesVideoObjectWithDimensionsAndThumbnail(t *testing.T) {
	cfg := testConfig(t)
	cfg.TranscoderWorkDir = t.TempDir()
	stagingPath := filepath.Join(cfg.StagingDir, "job-1.mp4.upload")
	if err := os.WriteFile(stagingPath, []byte("source"), 0o600); err != nil {
		t.Fatal(err)
	}
	transcoded := []byte("transcoded-output")
	metadata := newMemoryMetadata()
	metadata.aliases["mine.mp4"] = ShareAlias{Slug: "mine.mp4", Owner: "user-1", DisplayFilename: "input.mkv", Visibility: "public", Status: AliasStatusPending}
	metadata.jobs["job-1"] = ProcessingJob{ID: "job-1", Owner: "user-1", AliasSlug: "mine.mp4", StagingPath: stagingPath, Profile: ProcessingProfileMP4Web, Status: ProcessingStatusQueued, DisplayFilename: "input.mkv", SourceType: "video/x-matroska"}
	store := &fakeStore{}
	runner := fakeMediaRunner{remuxOutput: []byte("remux"), transcodeOutput: transcoded, width: 1920, height: 1080, thumbnailOutput: []byte("jpeg-bytes")}
	transcoder := NewTranscoder(cfg, store, metadata, runner, slog.Default())

	processed, err := transcoder.ProcessNext(context.Background())

	if err != nil {
		t.Fatal(err)
	}
	if !processed {
		t.Fatal("expected a job to be processed")
	}
	targetSHA := sha256Hex(transcoded)
	thumbnailKey := targetSHA[:2] + "/" + targetSHA + ".jpg"
	object := metadata.objects[targetSHA]
	if object.Width != 1920 || object.Height != 1080 || object.ThumbnailKey != thumbnailKey {
		t.Fatalf("object = %#v", object)
	}
	if string(store.objects[thumbnailKey]) != "jpeg-bytes" {
		t.Fatalf("thumbnail bytes = %q", store.objects[thumbnailKey])
	}
}

func TestParseVideoDimensions(t *testing.T) {
	tests := []struct {
		output        string
		width, height int
		wantErr       bool
	}{
		{output: "640,480", width: 640, height: 480},
		{output: "1080,1920\n", width: 1080, height: 1920},
		{output: "1080,1920,", width: 1080, height: 1920},
		{output: "1920,1080,0,0", width: 1920, height: 1080},
		{output: "", wantErr: true},
		{output: "1080", wantErr: true},
		{output: "wide,480", wantErr: true},
		{output: "1080,tall", wantErr: true},
	}
	for _, test := range tests {
		width, height, err := parseVideoDimensions(test.output)
		if test.wantErr {
			if err == nil {
				t.Fatalf("parseVideoDimensions(%q) = %d,%d, want error", test.output, width, height)
			}
			continue
		}
		if err != nil || width != test.width || height != test.height {
			t.Fatalf("parseVideoDimensions(%q) = %d,%d,%v, want %d,%d", test.output, width, height, err, test.width, test.height)
		}
	}
}

func TestProcessorSkipsUploadWhenReadyObjectStillExists(t *testing.T) {
	cfg := testConfig(t)
	cfg.TranscoderWorkDir = t.TempDir()
	stagingPath := filepath.Join(cfg.StagingDir, "job-1.txt.upload")
	if err := os.WriteFile(stagingPath, []byte("same-target"), 0o600); err != nil {
		t.Fatal(err)
	}
	targetSHA := sha256Hex([]byte("same-target"))
	targetKey := targetSHA[:2] + "/" + targetSHA + ".txt"
	metadata := newMemoryMetadata()
	metadata.objects[targetSHA] = StoredObject{SHA256: targetSHA, ObjectKey: targetKey, Size: 11, ContentType: "text/plain", Extension: ".txt", Status: "ready"}
	metadata.aliases["mine.txt"] = ShareAlias{Slug: "mine.txt", Owner: "user-1", DisplayFilename: "mine.txt", Visibility: "public", Status: AliasStatusPending}
	metadata.jobs["job-1"] = ProcessingJob{ID: "job-1", Owner: "user-1", AliasSlug: "mine.txt", StagingPath: stagingPath, Profile: ProcessingProfileUploadFinalize, Status: ProcessingStatusQueued, DisplayFilename: "mine.txt", SourceType: "text/plain"}
	store := &fakeStore{objects: map[string][]byte{targetKey: []byte("same-target")}}
	transcoder := NewTranscoder(cfg, store, metadata, fakeMediaRunner{}, slog.Default())

	processed, err := transcoder.ProcessNext(context.Background())

	if err != nil {
		t.Fatal(err)
	}
	if !processed {
		t.Fatal("expected a job to be processed")
	}
	if store.putKey != "" {
		t.Fatalf("unexpected upload to %q", store.putKey)
	}
	if metadata.aliases["mine.txt"].ObjectSHA256 != targetSHA {
		t.Fatalf("alias = %#v", metadata.aliases["mine.txt"])
	}
}

func TestProcessorReuploadsWhenMetadataObjectIsMissingFromB2(t *testing.T) {
	cfg := testConfig(t)
	cfg.TranscoderWorkDir = t.TempDir()
	stagingPath := filepath.Join(cfg.StagingDir, "job-1.txt.upload")
	if err := os.WriteFile(stagingPath, []byte("same-target"), 0o600); err != nil {
		t.Fatal(err)
	}
	targetSHA := sha256Hex([]byte("same-target"))
	targetKey := targetSHA[:2] + "/" + targetSHA + ".txt"
	metadata := newMemoryMetadata()
	metadata.objects[targetSHA] = StoredObject{SHA256: targetSHA, ObjectKey: targetKey, Size: 11, ContentType: "text/plain", Extension: ".txt", Status: "ready"}
	metadata.aliases["mine.txt"] = ShareAlias{Slug: "mine.txt", Owner: "user-1", DisplayFilename: "mine.txt", Visibility: "public", Status: AliasStatusPending}
	metadata.jobs["job-1"] = ProcessingJob{ID: "job-1", Owner: "user-1", AliasSlug: "mine.txt", StagingPath: stagingPath, Profile: ProcessingProfileUploadFinalize, Status: ProcessingStatusQueued, DisplayFilename: "mine.txt", SourceType: "text/plain"}
	store := &fakeStore{headErr: errors.New("missing")}
	transcoder := NewTranscoder(cfg, store, metadata, fakeMediaRunner{}, slog.Default())

	processed, err := transcoder.ProcessNext(context.Background())

	if err != nil {
		t.Fatal(err)
	}
	if !processed {
		t.Fatal("expected a job to be processed")
	}
	if store.putKey != targetKey {
		t.Fatalf("put key = %q, want %q", store.putKey, targetKey)
	}
	if len(metadata.unavailable) != 1 || metadata.unavailable[0] != targetSHA {
		t.Fatalf("unavailable = %#v", metadata.unavailable)
	}
}

func TestProcessorMarksJobFailedAndLeavesAliasPendingOnFailure(t *testing.T) {
	cfg := testConfig(t)
	cfg.TranscoderWorkDir = t.TempDir()
	stagingPath := filepath.Join(cfg.StagingDir, "job-1.mp4.upload")
	if err := os.WriteFile(stagingPath, []byte("source"), 0o600); err != nil {
		t.Fatal(err)
	}
	metadata := newMemoryMetadata()
	metadata.aliases["mine.mp4"] = ShareAlias{Slug: "mine.mp4", Owner: "user-1", DisplayFilename: "input.mp4", Visibility: "public", Status: AliasStatusPending}
	metadata.jobs["job-1"] = ProcessingJob{ID: "job-1", Owner: "user-1", AliasSlug: "mine.mp4", StagingPath: stagingPath, Profile: ProcessingProfileMP4Web, Status: ProcessingStatusQueued, DisplayFilename: "input.mp4", SourceType: "video/mp4"}
	store := &fakeStore{}
	transcoder := NewTranscoder(cfg, store, metadata, fakeMediaRunner{remuxErr: errors.New("bad media"), transcodeErr: errors.New("bad transcode")}, slog.Default())

	processed, err := transcoder.ProcessNext(context.Background())

	if err != nil {
		t.Fatal(err)
	}
	if !processed {
		t.Fatal("expected a job to be processed")
	}
	if metadata.jobs["job-1"].Status != ProcessingStatusFailed || !strings.Contains(metadata.jobs["job-1"].Error, "bad transcode") {
		t.Fatalf("job = %#v", metadata.jobs["job-1"])
	}
	if metadata.aliases["mine.mp4"].Status != AliasStatusFailed || metadata.aliases["mine.mp4"].ObjectSHA256 != "" {
		t.Fatalf("alias = %#v", metadata.aliases["mine.mp4"])
	}
	if _, err := os.Stat(stagingPath); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("staging file still exists or stat failed unexpectedly: %v", err)
	}
}

func sha256Hex(value []byte) string {
	sum := sha256.Sum256(value)
	return hex.EncodeToString(sum[:])
}
