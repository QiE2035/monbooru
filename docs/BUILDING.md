# Building from source

```bash
# CPU only, no auto-tagger
go build -o monbooru ./cmd/monbooru

# With auto-tagger (requires the ONNX Runtime shared library on the system)
CGO_ENABLED=1 go build -tags tagger -o monbooru ./cmd/monbooru

./monbooru -config /path/to/monbooru.toml
```

## CLI flags and subcommands

- `-config` - path to the TOML config file.
- `-hash-password` - print a bcrypt hash and exit.
- `healthcheck` - probe the local `/health` endpoint.

## ONNX Runtime

For the `-tags tagger` build, `libonnxruntime.so` must be reachable:
- on `LD_LIBRARY_PATH` / `/usr/lib`, or
- via the `ORT_LIB_PATH` env var (absolute path to the `.so`).

The Docker image bundles ORT v1.21.0 and does not need this.
