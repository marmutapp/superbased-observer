; JavaScript / JSX symbol + call + import captures (shared by .js, .mjs,
; .cjs, .jsx — the tree-sitter-javascript grammar parses JSX). See rust.scm
; for the capture vocabulary. The require() pattern uses the #eq? predicate
; (evaluated by the harness) so only require("…") is captured as an import,
; not every call with a string argument.

(function_declaration name: (identifier) @name) @definition.function
(generator_function_declaration name: (identifier) @name) @definition.function
(class_declaration name: (identifier) @name) @definition.class
(method_definition name: (property_identifier) @name) @definition.method
(variable_declarator name: (identifier) @name value: [(arrow_function) (function_expression)]) @definition.function

(call_expression function: (identifier) @name) @reference.call
(call_expression function: (member_expression property: (property_identifier) @name)) @reference.call

(import_statement source: (string) @_import.path)
(call_expression
  function: (identifier) @_req
  arguments: (arguments (string) @_import.path)
  (#eq? @_req "require"))
