package store

import "time"

// Purposes of a stored batch file, as the OpenAI Files API names them: an
// uploaded input is "batch", a result or error file the platform wrote is
// "batch_output".
const (
	BatchFilePurposeInput  = "batch"
	BatchFilePurposeOutput = "batch_output"
)

// Statuses of a batch, as the OpenAI Batch API names them.
const (
	BatchValidating = "validating"
	BatchFailed     = "failed"
	BatchInProgress = "in_progress"
	BatchFinalizing = "finalizing"
	BatchCompleted  = "completed"
	BatchExpired    = "expired"
	BatchCancelling = "cancelling"
	BatchCancelled  = "cancelled"
)

// BatchTerminal reports whether a batch in status has finished for good.
func BatchTerminal(status string) bool {
	switch status {
	case BatchFailed, BatchCompleted, BatchExpired, BatchCancelled:
		return true
	}
	return false
}

// BatchFile is the metadata of one file in the batch file area: the bytes are
// on disk under the data directory (internal/server/batch), named by ID. The
// owner is the user who created it, through an API key or the console; a
// caller sees the files of its own user, the console's administrators see all.
type BatchFile struct {
	ID       string `gorm:"primaryKey;size:48;index:idx_batch_files_user_created,priority:3;index:idx_batch_files_created,priority:2"`
	UserID   uint   `gorm:"not null;default:0;index:idx_batch_files_user_created,priority:1"`
	APIKeyID uint   `gorm:"not null;default:0"`
	Purpose  string `gorm:"size:16;not null"`
	Filename string `gorm:"size:255;not null"`
	Bytes    int64  `gorm:"not null;default:0"`
	// Lines is the number of requests in an input file; zero for an output.
	Lines int `gorm:"not null;default:0"`
	// Endpoint is the url every request of an input file asks for; empty when
	// they differ, which no batch accepts.
	Endpoint  string    `gorm:"size:40;not null;default:''"`
	CreatedAt time.Time `gorm:"index:idx_batch_files_user_created,priority:2;index:idx_batch_files_created,priority:1"`
}

// Batch is one asynchronous batch of requests over an input file. The counts
// and CheckpointOffset move with the executor's checkpoints, not per request:
// CheckpointOffset is the byte length of the partial result file that is
// durably written, the truncation point a resumed run starts from.
type Batch struct {
	ID               string `gorm:"primaryKey;size:48;index:idx_batches_user_created,priority:3;index:idx_batches_created,priority:2"`
	UserID           uint   `gorm:"not null;default:0;index:idx_batches_user_created,priority:1"`
	APIKeyID         uint   `gorm:"not null;default:0"`
	Endpoint         string `gorm:"size:40;not null"`
	InputFileID      string `gorm:"size:48;not null;index"`
	OutputFileID     string `gorm:"size:48;not null;default:''"`
	ErrorFileID      string `gorm:"size:48;not null;default:''"`
	CompletionWindow string `gorm:"size:8;not null"`
	// ModelOverride is the served name every request of the batch is sent to,
	// replacing its line's own body.model; empty when each line names its own.
	ModelOverride string `gorm:"size:128;not null;default:''"`
	// PriorityClass is the name of the request priority class the batch's
	// requests run at; empty runs them at the batch class. A name rather
	// than an id, so a finished batch still says what it ran at once the
	// class is deleted.
	PriorityClass string `gorm:"size:64;not null;default:''"`
	Status        string `gorm:"size:16;not null;index"`
	// ErrorCode and ErrorMessage say why a failed batch failed.
	ErrorCode        string    `gorm:"size:64;not null;default:''"`
	ErrorMessage     string    `gorm:"size:512;not null;default:''"`
	Total            int       `gorm:"not null;default:0"`
	Completed        int       `gorm:"not null;default:0"`
	Failed           int       `gorm:"not null;default:0"`
	CheckpointOffset int64     `gorm:"not null;default:0"`
	CreatedAt        time.Time `gorm:"index:idx_batches_user_created,priority:2;index:idx_batches_created,priority:1"`
	ExpiresAt        time.Time `gorm:"not null"`
	InProgressAt     *time.Time
	FinalizingAt     *time.Time
	CompletedAt      *time.Time
	FailedAt         *time.Time
	ExpiredAt        *time.Time
	CancellingAt     *time.Time
	CancelledAt      *time.Time
}

// BatchMetadata is one key of a batch's caller-supplied metadata.
type BatchMetadata struct {
	BatchID string `gorm:"primaryKey;size:48"`
	Key     string `gorm:"primaryKey;size:64"`
	Value   string `gorm:"size:512;not null"`
}
