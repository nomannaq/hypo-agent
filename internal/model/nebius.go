// Package model is the model gateway: the Nebius client plus retry, rate
// limiting, and usage metering. Called from inside Step bodies in
// internal/executor so every model call is journaled exactly like a tool
// call.
package model

// TODO: Client, Complete(ctx, req) (resp, error) with retry/backoff and a
// metering hook.
