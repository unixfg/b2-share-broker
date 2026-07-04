package broker

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
)

const (
	ProcessingProfileUploadFinalize = "upload-finalize"
	ProcessingProfileMP4Web         = "mp4-web"

	ProcessingStatusQueued    = "queued"
	ProcessingStatusRunning   = "running"
	ProcessingStatusCompleted = "completed"
	ProcessingStatusFailed    = "failed"
	ProcessingStatusCanceled  = "canceled"

	AliasStatusPending = "pending"
	AliasStatusReady   = "ready"
	AliasStatusFailed  = "failed"
)

func ValidateProcessingProfile(profile string) error {
	switch profile {
	case ProcessingProfileUploadFinalize, ProcessingProfileMP4Web:
		return nil
	default:
		return fmt.Errorf("unsupported profile %q", profile)
	}
}

func NewProcessingJobID() (string, error) {
	var bytes [16]byte
	if _, err := rand.Read(bytes[:]); err != nil {
		return "", err
	}
	bytes[6] = (bytes[6] & 0x0f) | 0x40
	bytes[8] = (bytes[8] & 0x3f) | 0x80
	encoded := hex.EncodeToString(bytes[:])
	return encoded[0:8] + "-" + encoded[8:12] + "-" + encoded[12:16] + "-" + encoded[16:20] + "-" + encoded[20:], nil
}
