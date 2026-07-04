package broker

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"log/slog"
	"os"
	"strings"
	"testing"
)

type fakeRemuxRunner struct {
	output []byte
	err    error
}

func (r fakeRemuxRunner) FastStartRemux(_ context.Context, _, outputPath string) error {
	if r.err != nil {
		return r.err
	}
	return os.WriteFile(outputPath, r.output, 0o600)
}

func TestTranscoderCompletesRemuxAndRepointsAlias(t *testing.T) {
	cfg := testConfig()
	cfg.TranscoderWorkDir = t.TempDir()
	sourceKey := "s/" + testSHA256 + ".mp4"
	metadata := newMemoryMetadata()
	metadata.objects[testSHA256] = StoredObject{SHA256: testSHA256, ObjectKey: sourceKey, Size: 5, ContentType: "video/mp4", Extension: ".mp4", FirstFilename: "input.mp4", Uploader: "user-1"}
	metadata.aliases["mine.mp4"] = ShareAlias{Slug: "mine.mp4", ObjectSHA256: testSHA256, ObjectKey: sourceKey, Owner: "user-1", DisplayFilename: "input.mp4", Visibility: "public"}
	metadata.jobs["job-1"] = ProcessingJob{ID: "job-1", Owner: "user-1", AliasSlug: "mine.mp4", SourceSHA256: testSHA256, SourceObjectKey: sourceKey, Profile: ProcessingProfileMP4FaststartRemux, Status: ProcessingStatusQueued, DisplayFilename: "input.mp4", SourceType: "video/mp4"}
	store := &fakeStore{objects: map[string][]byte{sourceKey: []byte("input")}}
	output := []byte("faststart-output")
	transcoder := NewTranscoder(cfg, store, metadata, fakeRemuxRunner{output: output}, slog.Default())

	processed, err := transcoder.ProcessNext(context.Background())

	if err != nil {
		t.Fatal(err)
	}
	if !processed {
		t.Fatal("expected a job to be processed")
	}
	targetSHA := sha256Hex(output)
	targetKey := "s/" + targetSHA + ".mp4"
	if metadata.jobs["job-1"].Status != ProcessingStatusCompleted {
		t.Fatalf("job = %#v", metadata.jobs["job-1"])
	}
	if metadata.aliases["mine.mp4"].ObjectSHA256 != targetSHA {
		t.Fatalf("alias = %#v", metadata.aliases["mine.mp4"])
	}
	if store.putKey != targetKey || store.putType != "video/mp4" {
		t.Fatalf("put key/type = %q/%q", store.putKey, store.putType)
	}
	if len(metadata.history) != 1 || metadata.history[0].ObjectSHA256 != testSHA256 {
		t.Fatalf("history = %#v", metadata.history)
	}
	derivative := metadata.derivatives[testSHA256+"|"+ProcessingProfileMP4FaststartRemux]
	if derivative.TargetSHA256 != targetSHA || derivative.JobID != "job-1" {
		t.Fatalf("derivative = %#v", derivative)
	}
}

func TestTranscoderSkipsUploadWhenRemuxedObjectExists(t *testing.T) {
	cfg := testConfig()
	cfg.TranscoderWorkDir = t.TempDir()
	sourceKey := "s/" + testSHA256 + ".mp4"
	output := []byte("same-target")
	targetSHA := sha256Hex(output)
	targetKey := "s/" + targetSHA + ".mp4"
	metadata := newMemoryMetadata()
	metadata.objects[testSHA256] = StoredObject{SHA256: testSHA256, ObjectKey: sourceKey, Size: 5, ContentType: "video/mp4", Extension: ".mp4", FirstFilename: "input.mp4", Uploader: "user-1"}
	metadata.objects[targetSHA] = StoredObject{SHA256: targetSHA, ObjectKey: targetKey, Size: int64(len(output)), ContentType: "video/mp4", Extension: ".mp4", FirstFilename: "other.mp4", Uploader: "user-2"}
	metadata.aliases["mine.mp4"] = ShareAlias{Slug: "mine.mp4", ObjectSHA256: testSHA256, ObjectKey: sourceKey, Owner: "user-1", DisplayFilename: "input.mp4", Visibility: "public"}
	metadata.jobs["job-1"] = ProcessingJob{ID: "job-1", Owner: "user-1", AliasSlug: "mine.mp4", SourceSHA256: testSHA256, SourceObjectKey: sourceKey, Profile: ProcessingProfileMP4FaststartRemux, Status: ProcessingStatusQueued, DisplayFilename: "input.mp4", SourceType: "video/mp4"}
	store := &fakeStore{objects: map[string][]byte{sourceKey: []byte("input")}}
	transcoder := NewTranscoder(cfg, store, metadata, fakeRemuxRunner{output: output}, slog.Default())

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
	if metadata.aliases["mine.mp4"].ObjectSHA256 != targetSHA {
		t.Fatalf("alias = %#v", metadata.aliases["mine.mp4"])
	}
}

func TestTranscoderMarksJobFailedAndLeavesAliasOnRemuxFailure(t *testing.T) {
	cfg := testConfig()
	cfg.TranscoderWorkDir = t.TempDir()
	sourceKey := "s/" + testSHA256 + ".mp4"
	metadata := newMemoryMetadata()
	metadata.objects[testSHA256] = StoredObject{SHA256: testSHA256, ObjectKey: sourceKey, Size: 5, ContentType: "video/mp4", Extension: ".mp4", FirstFilename: "input.mp4", Uploader: "user-1"}
	metadata.aliases["mine.mp4"] = ShareAlias{Slug: "mine.mp4", ObjectSHA256: testSHA256, ObjectKey: sourceKey, Owner: "user-1", DisplayFilename: "input.mp4", Visibility: "public"}
	metadata.jobs["job-1"] = ProcessingJob{ID: "job-1", Owner: "user-1", AliasSlug: "mine.mp4", SourceSHA256: testSHA256, SourceObjectKey: sourceKey, Profile: ProcessingProfileMP4FaststartRemux, Status: ProcessingStatusQueued, DisplayFilename: "input.mp4", SourceType: "video/mp4"}
	store := &fakeStore{objects: map[string][]byte{sourceKey: []byte("input")}}
	transcoder := NewTranscoder(cfg, store, metadata, fakeRemuxRunner{err: errors.New("bad media")}, slog.Default())

	processed, err := transcoder.ProcessNext(context.Background())

	if err != nil {
		t.Fatal(err)
	}
	if !processed {
		t.Fatal("expected a job to be processed")
	}
	if metadata.jobs["job-1"].Status != ProcessingStatusFailed || !strings.Contains(metadata.jobs["job-1"].Error, "bad media") {
		t.Fatalf("job = %#v", metadata.jobs["job-1"])
	}
	if metadata.aliases["mine.mp4"].ObjectSHA256 != testSHA256 {
		t.Fatalf("alias changed after failure: %#v", metadata.aliases["mine.mp4"])
	}
}

func sha256Hex(value []byte) string {
	sum := sha256.Sum256(value)
	return hex.EncodeToString(sum[:])
}
