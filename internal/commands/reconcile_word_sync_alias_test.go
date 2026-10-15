package commands

import "github.com/sydlexius/canticle/internal/wordsyncrepair"

// wordSyncBackupRecord is one JSONL backup line (see wordsyncrepair). It is
// used only by the reconcile-word-sync command tests.
type wordSyncBackupRecord = wordsyncrepair.BackupRecord
