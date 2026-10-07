package instrumentalmark

import (
	"encoding/json"
	"fmt"
	"os"
)

// OpMark labels a Record written by Mark.
const OpMark = "mark"

// OpUnmark labels a Record written by Unmark: the manual marker about to be
// removed. Restoring the lyrics a mark replaced is a separate operator action
// from the OpMark records.
const OpUnmark = "unmark"

// MaxBackupBytes caps the size of a sidecar the backup will capture. A larger
// file fails the backup, which (backup-first) leaves everything untouched.
const MaxBackupBytes = 4 << 20

// Record is one restorable JSONL backup line: the bytes of a lyric file that
// Mark is about to replace. Restoring is writing Content back to Path, which
// is all the unmark action needs from it.
type Record struct {
	Op         string `json:"op"`
	WorkItemID int64  `json:"work_item_id"`
	Path       string `json:"path"`
	Content    []byte `json:"content"`
}

// AppendRecord writes rec to f as one JSON line and fsyncs it, so the record
// is durable before the replacement it protects. It is the Report body a
// caller passes in Options.
func AppendRecord(f *os.File, rec Record) error {
	b, err := json.Marshal(rec)
	if err != nil {
		return fmt.Errorf("instrumentalmark: marshal backup record: %w", err)
	}
	if _, err := f.Write(append(b, '\n')); err != nil {
		return fmt.Errorf("instrumentalmark: write backup record: %w", err)
	}
	if err := f.Sync(); err != nil {
		return fmt.Errorf("instrumentalmark: sync backup record: %w", err)
	}
	return nil
}
