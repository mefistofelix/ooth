# Go toolchain patch

`main.go` is a standalone patch application tool, invoked by the root `build.sh`.
It embeds the files in `patches/`, checks the pinned Go version and modifies only
the dedicated toolchain passed through `-goroot`, under the root `build/`.

This directory contains the maintained patch sources. The patched standard
library and its generated files stay inside that toolchain; they are not part
of ooth's application source in `src/`. No global Go installation is changed.
