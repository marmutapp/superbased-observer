// extract.c — self-contained tree-sitter extraction harness compiled to a
// WASI reactor module (one per language). ALL tree-sitter API use (which
// passes TSNode structs by value) happens INSIDE this module; the host
// (wazero, pure Go, CGO-free) sees only a flat pointer/length ABI, so the
// awkward struct-by-value marshalling never crosses the boundary. The
// grammar's tree_sitter_<lang>() is named at compile time via
// -DTS_LANGUAGE_FN. Build with build.sh.
//
// Host ABI:
//   uint8_t* ci_alloc(uint32_t n)              allocate a host-writable buffer
//   void     ci_free(uint8_t* p)               free it
//   int32_t  ci_run(src,src_len,q,q_len)       parse src, run query q
//   uint32_t ci_out_ptr(void)                  pointer to the record stream
//   uint32_t ci_out_len(void)                  its length
//
// Output is a little-endian binary record stream. Each record:
//   u8  rectype  ('D' def, 'C' call, 'I' import)
//   u32 start_byte, u32 end_byte, u32 start_row, u32 end_row
//   u8  kind     (def: f/m/c/i/t/o/M ; else 0)
//   u32 name_len ; name_len bytes (UTF-8, no terminator)

#include <stdint.h>
#include <stdlib.h>
#include <string.h>
#include "tree_sitter/api.h"

extern const TSLanguage *TS_LANGUAGE_FN(void);

__attribute__((export_name("ci_alloc"))) uint8_t *ci_alloc(uint32_t n) {
  return (uint8_t *)malloc(n ? n : 1);
}
__attribute__((export_name("ci_free"))) void ci_free(uint8_t *p) { free(p); }

static uint8_t *g_out = NULL;
static uint32_t g_out_len = 0;
static uint32_t g_out_cap = 0;

static int out_reserve(uint32_t extra) {
  if (g_out_len + extra <= g_out_cap) return 1;
  uint32_t cap = g_out_cap ? g_out_cap : 4096;
  while (cap < g_out_len + extra) cap *= 2;
  uint8_t *nb = (uint8_t *)realloc(g_out, cap);
  if (!nb) return 0;
  g_out = nb;
  g_out_cap = cap;
  return 1;
}
static void out_u8(uint8_t v) {
  if (!out_reserve(1)) return;
  g_out[g_out_len++] = v;
}
static void out_u32(uint32_t v) {
  if (!out_reserve(4)) return;
  g_out[g_out_len++] = (uint8_t)(v & 0xff);
  g_out[g_out_len++] = (uint8_t)((v >> 8) & 0xff);
  g_out[g_out_len++] = (uint8_t)((v >> 16) & 0xff);
  g_out[g_out_len++] = (uint8_t)((v >> 24) & 0xff);
}
static void out_bytes(const char *p, uint32_t n) {
  if (!out_reserve(n)) return;
  memcpy(g_out + g_out_len, p, n);
  g_out_len += n;
}

static void emit(uint8_t rectype, TSNode span, uint8_t kind, const char *src,
                 TSNode name_node) {
  uint32_t sb = ts_node_start_byte(span);
  uint32_t eb = ts_node_end_byte(span);
  TSPoint sp = ts_node_start_point(span);
  TSPoint ep = ts_node_end_point(span);
  uint32_t nstart = ts_node_start_byte(name_node);
  uint32_t nend = ts_node_end_byte(name_node);
  uint32_t nlen = nend > nstart ? nend - nstart : 0;
  out_u8(rectype);
  out_u32(sb);
  out_u32(eb);
  out_u32(sp.row);
  out_u32(ep.row);
  out_u8(kind);
  out_u32(nlen);
  if (nlen) out_bytes(src + nstart, nlen);
}

// kind_for maps a "definition.<suffix>" capture name to a one-byte kind code.
static uint8_t kind_for(const char *cap, uint32_t len) {
  const char *dot = NULL;
  for (uint32_t i = 0; i < len; i++) {
    if (cap[i] == '.') {
      dot = cap + i + 1;
      len = len - i - 1;
      break;
    }
  }
  if (!dot) return 't';
  if (!strncmp(dot, "function", 8)) return 'f';
  if (!strncmp(dot, "method", 6)) return 'm';
  if (!strncmp(dot, "class", 5)) return 'c';
  if (!strncmp(dot, "struct", 6)) return 'c';
  if (!strncmp(dot, "enum", 4)) return 'c';
  if (!strncmp(dot, "interface", 9)) return 'i';
  if (!strncmp(dot, "trait", 5)) return 'i';
  if (!strncmp(dot, "module", 6)) return 'o';
  if (!strncmp(dot, "macro", 5)) return 'M';
  if (!strncmp(dot, "type", 4)) return 't';
  return 't';
}

static int cap_is(const char *cap, uint32_t len, const char *lit) {
  uint32_t n = (uint32_t)strlen(lit);
  return len == n && !strncmp(cap, lit, n);
}
static int cap_has_prefix(const char *cap, uint32_t len, const char *pre) {
  uint32_t n = (uint32_t)strlen(pre);
  return len >= n && !strncmp(cap, pre, n);
}

// find_capture_node returns the first node captured under capture index idx in
// this match, for predicate evaluation.
static int find_capture_node(const TSQueryMatch *m, uint32_t idx, TSNode *out) {
  for (uint16_t i = 0; i < m->capture_count; i++) {
    if (m->captures[i].index == idx) {
      *out = m->captures[i].node;
      return 1;
    }
  }
  return 0;
}

// op_text resolves one predicate operand (a String literal or a @capture) to a
// byte range. Returns 0 if a @capture operand is absent from this match.
static int op_text(const TSQuery *q, const TSQueryMatch *m, const char *src,
                   TSQueryPredicateStep step, const char **p, uint32_t *len) {
  if (step.type == TSQueryPredicateStepTypeString) {
    *p = ts_query_string_value_for_id(q, step.value_id, len);
    return 1;
  }
  if (step.type == TSQueryPredicateStepTypeCapture) {
    TSNode node;
    if (!find_capture_node(m, step.value_id, &node)) return 0;
    uint32_t a = ts_node_start_byte(node), b = ts_node_end_byte(node);
    *p = src + a;
    *len = b > a ? b - a : 0;
    return 1;
  }
  return 0;
}

static int bytes_eq(const char *a, uint32_t alen, const char *b, uint32_t blen) {
  return alen == blen && (alen == 0 || !memcmp(a, b, alen));
}

// predicates_ok evaluates the standard text predicates on a match — #eq?,
// #not-eq?, #any-of?, #not-any-of? (the ones the low-level query cursor does
// NOT apply itself). Unknown predicates/directives (#match?, #set!, #strip!,
// …) are treated as non-filtering (pass), matching tree-sitter convention. A
// match is rejected only when a recognised predicate is present AND fails.
static int predicates_ok(const TSQuery *q, const TSQueryMatch *m,
                         const char *src) {
  uint32_t n = 0;
  const TSQueryPredicateStep *st =
      ts_query_predicates_for_pattern(q, m->pattern_index, &n);
  uint32_t i = 0;
  while (i < n) {
    if (st[i].type != TSQueryPredicateStepTypeString) { // malformed; skip group
      while (i < n && st[i].type != TSQueryPredicateStepTypeDone) i++;
      i++;
      continue;
    }
    uint32_t namelen = 0;
    const char *name = ts_query_string_value_for_id(q, st[i].value_id, &namelen);
    uint32_t op = i + 1, end = op;
    while (end < n && st[end].type != TSQueryPredicateStepTypeDone) end++;
    uint32_t nops = end - op;

    if ((bytes_eq(name, namelen, "eq?", 3) ||
         bytes_eq(name, namelen, "not-eq?", 7)) &&
        nops == 2) {
      const char *a, *b;
      uint32_t al, bl;
      if (op_text(q, m, src, st[op], &a, &al) &&
          op_text(q, m, src, st[op + 1], &b, &bl)) {
        int eq = bytes_eq(a, al, b, bl);
        int want_eq = bytes_eq(name, namelen, "eq?", 3);
        if (eq != want_eq) return 0;
      }
    } else if ((bytes_eq(name, namelen, "any-of?", 7) ||
                bytes_eq(name, namelen, "not-any-of?", 11)) &&
               nops >= 1) {
      const char *a;
      uint32_t al;
      if (op_text(q, m, src, st[op], &a, &al)) {
        int found = 0;
        for (uint32_t k = op + 1; k < end; k++) {
          const char *s;
          uint32_t sl;
          if (op_text(q, m, src, st[k], &s, &sl) && bytes_eq(a, al, s, sl)) {
            found = 1;
            break;
          }
        }
        int want = bytes_eq(name, namelen, "any-of?", 7);
        if (found != want) return 0;
      }
    }
    i = end + 1; // skip the Done sentinel
  }
  return 1;
}

__attribute__((export_name("ci_run"))) int32_t ci_run(const uint8_t *src,
                                                      uint32_t src_len,
                                                      const uint8_t *query,
                                                      uint32_t query_len) {
  g_out_len = 0; // reuse buffer across calls

  TSParser *parser = ts_parser_new();
  if (!parser) return -1;
  if (!ts_parser_set_language(parser, TS_LANGUAGE_FN())) {
    ts_parser_delete(parser);
    return -2; // core/grammar ABI mismatch
  }
  TSTree *tree =
      ts_parser_parse_string(parser, NULL, (const char *)src, src_len);
  if (!tree) {
    ts_parser_delete(parser);
    return -3;
  }

  uint32_t err_off = 0;
  TSQueryError err_type = TSQueryErrorNone;
  TSQuery *q = ts_query_new(TS_LANGUAGE_FN(), (const char *)query, query_len,
                            &err_off, &err_type);
  if (!q) {
    ts_tree_delete(tree);
    ts_parser_delete(parser);
    return -(100 + (int32_t)err_type); // query compile error
  }

  TSQueryCursor *cur = ts_query_cursor_new();
  ts_query_cursor_exec(cur, q, ts_tree_root_node(tree));

  TSQueryMatch match;
  while (ts_query_cursor_next_match(cur, &match)) {
    if (!predicates_ok(q, &match, (const char *)src)) continue;
    TSNode name_node;
    int have_name = 0;
    TSNode def_node;
    int have_def = 0;
    uint8_t def_kind = 't';
    int is_call = 0;
    TSNode import_node;
    int have_import = 0;

    for (uint16_t i = 0; i < match.capture_count; i++) {
      uint32_t clen = 0;
      const char *cap =
          ts_query_capture_name_for_id(q, match.captures[i].index, &clen);
      TSNode node = match.captures[i].node;
      if (cap_is(cap, clen, "name")) {
        name_node = node;
        have_name = 1;
      } else if (cap_has_prefix(cap, clen, "definition.")) {
        def_node = node;
        have_def = 1;
        def_kind = kind_for(cap, clen);
      } else if (cap_has_prefix(cap, clen, "reference.call")) {
        is_call = 1;
      } else if (cap_is(cap, clen, "_import.path")) {
        import_node = node;
        have_import = 1;
      }
    }

    if (have_import) {
      emit('I', import_node, 0, (const char *)src, import_node);
    } else if (have_def && have_name) {
      emit('D', def_node, def_kind, (const char *)src, name_node);
    } else if (is_call && have_name) {
      emit('C', name_node, 0, (const char *)src, name_node);
    }
  }

  ts_query_cursor_delete(cur);
  ts_query_delete(q);
  ts_tree_delete(tree);
  ts_parser_delete(parser);
  return 0;
}

__attribute__((export_name("ci_out_ptr"))) uint32_t ci_out_ptr(void) {
  return (uint32_t)(uintptr_t)g_out;
}
__attribute__((export_name("ci_out_len"))) uint32_t ci_out_len(void) {
  return g_out_len;
}
