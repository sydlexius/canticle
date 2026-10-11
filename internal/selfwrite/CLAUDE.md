# internal/selfwrite

- **Suppression keys on the FILE path, never its directory.** A directory key would also suppress events for every other file in it.
- **Entries expire.** A crash can never leave a path permanently deaf to external change; do not replace the TTL with a one-shot token.
