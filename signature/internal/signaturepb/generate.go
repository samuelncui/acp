// Package signaturepb holds the Protobuf form of the managed signature xattr entry.
package signaturepb

//go:generate protoc --go_out=. --go_opt=paths=source_relative signature.proto
