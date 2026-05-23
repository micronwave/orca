// Package store provides FileStore — the sole read/write layer for all durable
// structured artifacts. Every component that persists or retrieves a structured
// artifact goes through *FileStore.
//
// Exception: raw debug log files written by the CapsuleRunner/Adapter are the
// one allowed direct filesystem write. For capsule execution transcripts this
// follows <orcaDir>/capsules/<capsuleID>/transcript.log.
// They are unstructured debug artifacts, not consumed by runtime components.
// orca.md §8, §9.
//
// *FileStore appends artifact-creation events to the FileLog on every Save
// call. This is the mechanism by which obligation_planner, verifier_engine, and
// context_compiler get their artifacts logged without holding a direct FileLog
// dependency.
package store
