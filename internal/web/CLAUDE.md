# internal/web

- Every `/preview/{id}/...` handler resolves the file exactly as `handlePreviewAudio` does: same session guard, DB lookup and `os.Root` open. ffmpeg (or any subprocess) is handed the confined handle, never a library path.
