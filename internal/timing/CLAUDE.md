# internal/timing

- **The thresholds are NOT configurable, and that omission is the design.** `Tolerance` and `CategoricalRatio` are a co-calibrated pair. One predicate serving every caller (the `lyrics` guard, the worker stamp, `revalidate`, the serve-mode sweep) keeps a `timing_outcome` comparable across a deployment's history. Do not add a config key, flag or per-caller override, and do not write a second predicate elsewhere.
- A threshold worth tuning belongs here, as a change to the constant.
- No I/O in this package.
