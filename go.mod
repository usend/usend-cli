module usend.et/cli

go 1.26.0

// The release binaries are built with this toolchain (setup-go reads go.mod; Go switches to it otherwise):
// keep it on the latest patch release, govulncheck reports standard-library fixes against it.
toolchain go1.26.10

require usend.et/sdk/go v1.3.0
