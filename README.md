# yammm-lsp

[![Go Version](https://img.shields.io/badge/go-1.26+-blue.svg)](https://go.dev/)
[![License: MIT](https://img.shields.io/badge/License-MIT-yellow.svg)](LICENSE)

Language Server Protocol (LSP) server for [YAMMM](https://github.com/simon-lentz/yammm) schema files (`.yammm`).

## Features

- **Real-time diagnostics** -- parse errors, semantic errors, import resolution issues
- **Go-to-definition** -- navigate to type definitions (local and imported)
- **Hover** -- type details, property constraints, documentation
- **Completion** -- context-aware suggestions for keywords, types, properties, imports
- **Document symbols** -- hierarchical outline for schemas, types, and properties
- **Formatting** -- canonical yammm style

## Installation

### VS Code Extension (recommended)

Install from the [VS Code Marketplace](https://marketplace.visualstudio.com/items?itemName=simon-lentz.yammm). The extension bundles the `yammm-lsp` binary.

### Build from source

Requires Go 1.26+.

```bash
git clone https://github.com/simon-lentz/yammm-lsp.git
cd yammm-lsp
make build
```

This produces a `yammm-lsp` binary in the working directory. Add it to your `PATH`.

```bash
yammm-lsp --version
```

## Development

### Prerequisites

- Go 1.26+
- Node.js 18+ and npm (for VS Code extension development)

### Build and test

```bash
make test          # run all tests
make lint          # run golangci-lint
make build         # build the binary
```

### VS Code extension

```bash
make build-vscode    # build binary + compile extension (native platform)
make package-vscode  # package as .vsix
```

Install the `.vsix` in VS Code:
1. Open Command Palette (`Cmd+Shift+P` / `Ctrl+Shift+P`)
2. Run **Extensions: Install from VSIX...**
3. Select `editors/vscode/yammm-*.vsix`

### Cross-platform builds

```bash
make build-all         # build LSP binary for all 6 platforms
make build-vscode-all  # build extension with all platform binaries
```

## DSL Reference

For the complete YAMMM schema language specification, see [docs/SPEC.md](https://github.com/simon-lentz/yammm/blob/main/docs/SPEC.md) in the yammm repository.

## License

MIT -- see [LICENSE](LICENSE).
