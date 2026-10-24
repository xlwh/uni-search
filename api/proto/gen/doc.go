// Package unisearchpb contains protoc-generated Go code for UniSearch
// gRPC services (design 05 §7). Regenerate with: make proto
// @spec: ./active_specs/2026-10-20-gap-fill/02_spec.md#2.4
package unisearchpb

//go:generate protoc --go_out=. --go-grpc_out=. --go_opt=paths=source_relative --go-grpc_opt=paths=source_relative ../shard.proto ../meta.proto
