# internal/prune

- **`os.Stat` is the sole authority for "gone".** Never infer absence from the database, a scan result or a watcher event. An in-flight guard defers `processing` rows.
- A row gone inside a surviving directory is deleted only by AGE-OUT (`prune_gone_since`), and only after a second sweep confirms it: downtime is not absence.
- **Age-out is BACKUP-FIRST.** The report runs before the delete transaction, and a row whose report failed is not deleted.
- **More than `ageOutMaxPerSweep` (50) rows due at once is a hard stop** for the unattended sweep (it reads as a fault such as a missing union-share branch). The exit is attended: `scan reconcile-paths --yes`. No periodic sweep releases it.
