#!/usr/bin/env bash
# Regenerate the embedded codeintel_<lang>.wasm tree-sitter modules (Path A:
# self-contained WASI reactor modules, statically linking tree-sitter core +
# the grammar + extract.c). CGO-free at runtime (hosted under wazero).
#
# Prereqs (no root required — wasi-sdk is a self-contained tarball):
#   1. Download wasi-sdk and point WASI_SDK at the extracted dir:
#        https://github.com/WebAssembly/wasi-sdk/releases
#        export WASI_SDK=/path/to/wasi-sdk-XX.0-x86_64-linux
#   2. Run this script from the repo root or anywhere; it clones the pinned
#      grammar sources into a temp dir and emits *.wasm next to this script.
#
# USAGE:
#   build.sh                 # (re)build ALL grammars
#   build.sh java c cpp      # build only the named grammars (leaves the rest
#                            #   of the *.wasm in place — useful for onboarding
#                            #   a new grammar without churning existing ones)
#
# OUTPUT MODEL (W1 relief valve, ADR-0006): this script emits raw *.wasm AND
# then zstd-compresses each to *.wasm.zst via zstdpack.go. Only the *.wasm.zst
# is committed + embedded (go:embed grammars/*.wasm.zst); the raw *.wasm is
# gitignored build output, decompressed in-memory at runtime. zstdpack.go also
# enforces the 20MB compressed-embed budget.
#
# Pinned versions (see VERSIONS.txt). Bump intentionally; re-run; commit the
# regenerated *.wasm.zst. The grammar set is DATA — see docs/codeintel/adding-a-language.md.
set -euo pipefail

HERE="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
: "${WASI_SDK:?set WASI_SDK to an extracted wasi-sdk dir}"
CLANG="$WASI_SDK/bin/clang"
WORK="$(mktemp -d)"
trap 'rm -rf "$WORK"' EXIT

TS_CORE_REF="cbee4672665173d1702d836353ef7648dc2b2fac"   # tree-sitter core

# Grammar table: lang -> "repo-url|ref|grammar_fn|subdir"
#   subdir is the path within the clone that contains src/ (default ".").
# Most grammars live under github.com/tree-sitter; some Extended ones live in
# other orgs (full URL given). The js grammar serves both .js and .jsx (one
# module). tsx + typescript come from the same repo (different subdirs).
declare -A GRAMMAR=(
  [rust]="https://github.com/tree-sitter/tree-sitter-rust.git|v0.24.2|tree_sitter_rust|."
  [typescript]="https://github.com/tree-sitter/tree-sitter-typescript.git|75b3874edb2dc714fb1fd77a32013d0f8699989f|tree_sitter_typescript|typescript"
  [tsx]="https://github.com/tree-sitter/tree-sitter-typescript.git|75b3874edb2dc714fb1fd77a32013d0f8699989f|tree_sitter_tsx|tsx"
  [python]="https://github.com/tree-sitter/tree-sitter-python.git|26855eabccb19c6abf499fbc5b8dc7cc9ab8bc64|tree_sitter_python|."
  [javascript]="https://github.com/tree-sitter/tree-sitter-javascript.git|58404d8cf191d69f2674a8fd507bd5776f46cb11|tree_sitter_javascript|."
  [java]="https://github.com/tree-sitter/tree-sitter-java.git|v0.23.5|tree_sitter_java|."
  [c]="https://github.com/tree-sitter/tree-sitter-c.git|v0.24.2|tree_sitter_c|."
  [cpp]="https://github.com/tree-sitter/tree-sitter-cpp.git|v0.23.4|tree_sitter_cpp|."
  [csharp]="https://github.com/tree-sitter/tree-sitter-c-sharp.git|v0.23.5|tree_sitter_c_sharp|."
  [ruby]="https://github.com/tree-sitter/tree-sitter-ruby.git|v0.23.1|tree_sitter_ruby|."
  [php]="https://github.com/tree-sitter/tree-sitter-php.git|v0.24.2|tree_sitter_php|php"
  [kotlin]="https://github.com/fwcd/tree-sitter-kotlin.git|0.3.8|tree_sitter_kotlin|."
  [swift]="https://github.com/alex-pinkus/tree-sitter-swift.git|0.7.3-with-generated-files|tree_sitter_swift|."
  [scala]="https://github.com/tree-sitter/tree-sitter-scala.git|v0.26.0|tree_sitter_scala|."
  [bash]="https://github.com/tree-sitter/tree-sitter-bash.git|v0.25.1|tree_sitter_bash|."
  [lua]="https://github.com/tree-sitter-grammars/tree-sitter-lua.git|v0.5.0|tree_sitter_lua|."
)

# Build order (assoc arrays are unordered; this keeps output stable).
ALL_LANGS=(rust typescript tsx python javascript java c cpp csharp ruby php kotlin swift scala bash lua)

# Selection: args = subset to build; none = all.
if [ "$#" -gt 0 ]; then
  WANT=(" $* ")
else
  WANT=(" ${ALL_LANGS[*]} ")
fi
want() { [[ "${WANT[*]}" == *" $1 "* ]]; }

CFLAGS=( --target=wasm32-wasip1 -O2 -flto -fno-exceptions
         -I"$WORK/core/lib/include" -I"$WORK/core/lib/src" )
LDFLAGS=( -mexec-model=reactor
          -Wl,--export=ci_alloc -Wl,--export=ci_free -Wl,--export=ci_run
          -Wl,--export=ci_out_ptr -Wl,--export=ci_out_len
          -Wl,--export=__wasm_call_ctors )

clone() { # repo ref dest
  git clone --quiet "$1" "$3"
  git -C "$3" checkout --quiet "$2"
}

echo ">>> fetching tree-sitter core @ $TS_CORE_REF"
clone https://github.com/tree-sitter/tree-sitter.git "$TS_CORE_REF" "$WORK/core"

# build_lang compiles one grammar from the GRAMMAR table into codeintel_<lang>.wasm.
build_lang() { # lang
  local lang="$1"
  IFS='|' read -r url ref gfn subdir <<<"${GRAMMAR[$lang]}"
  local clonedir="$WORK/$lang"
  echo ">>> fetching $url @ $ref"
  clone "$url" "$ref" "$clonedir"
  local gdir="$clonedir/$subdir"
  local srcdir="$gdir/src"
  local srcs=("$srcdir/parser.c")
  [ -f "$srcdir/scanner.c" ] && srcs+=("$srcdir/scanner.c")
  [ -f "$srcdir/scanner.cc" ] && srcs+=("$srcdir/scanner.cc")

  echo ">>> building codeintel_$lang.wasm ($gfn; ${#srcs[@]} grammar src)"
  local objs=() n=0 s
  "$CLANG" "${CFLAGS[@]}" -c "$WORK/core/lib/src/lib.c" -o "$WORK/core_$lang.o"
  objs+=("$WORK/core_$lang.o")
  for s in "${srcs[@]}"; do
    "$CLANG" "${CFLAGS[@]}" -I"$srcdir" -c "$s" -o "$WORK/g_${lang}_$n.o"
    objs+=("$WORK/g_${lang}_$n.o"); n=$((n+1))
  done
  "$CLANG" "${CFLAGS[@]}" -DTS_LANGUAGE_FN="$gfn" -c "$HERE/extract.c" -o "$WORK/extract_$lang.o"
  objs+=("$WORK/extract_$lang.o")
  "$CLANG" "${CFLAGS[@]}" "${LDFLAGS[@]}" "${objs[@]}" -o "$HERE/codeintel_$lang.wasm"
}

for lang in "${ALL_LANGS[@]}"; do
  if want "$lang"; then build_lang "$lang"; fi
done

echo "=== raw sizes ==="
ls -la "$HERE"/*.wasm

echo "=== compressing embeds (zstd, ADR-0006) ==="
# Find the repo root so `go run` resolves the module; zstdpack.go resolves the
# grammars dir itself either way.
ROOT="$(cd "$HERE/../../../../.." && pwd)"
if command -v go >/dev/null 2>&1; then
  (cd "$ROOT" && go run ./internal/codeintel/parse/treesitter/grammars/zstdpack.go)
else
  echo "!!! go not on PATH — run 'go run ./internal/codeintel/parse/treesitter/grammars/zstdpack.go' to produce *.wasm.zst" >&2
  exit 1
fi
echo "=== committed/embedded artifacts (*.wasm.zst) ==="
ls -la "$HERE"/*.wasm.zst
