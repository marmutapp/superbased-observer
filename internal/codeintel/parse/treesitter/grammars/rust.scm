; Rust symbol + call + import captures for the codeintel tree-sitter
; backend. Capture vocabulary the host harness understands:
;   @name             the identifier text for the symbol/callee/import
;   @definition.<k>   a definition span; <k> -> function|method|class|
;                     interface|type|module|macro (see extract.c kind_for)
;   @reference.call   a call site (name from the @name in the same match)
;   @_import.path     an import/use path
;
; Methods (function_item inside an impl/trait declaration_list) are also
; matched by the plain function_item pattern; the backend dedups by span
; and keeps the more specific kind (method > function).

(function_item name: (identifier) @name) @definition.function
(function_signature_item name: (identifier) @name) @definition.function
(declaration_list (function_item name: (identifier) @name) @definition.method)

(struct_item name: (type_identifier) @name) @definition.class
(enum_item name: (type_identifier) @name) @definition.class
(union_item name: (type_identifier) @name) @definition.class
(trait_item name: (type_identifier) @name) @definition.interface
(type_item name: (type_identifier) @name) @definition.type
(mod_item name: (identifier) @name) @definition.module
(macro_definition name: (identifier) @name) @definition.macro

(call_expression function: (identifier) @name) @reference.call
(call_expression function: (scoped_identifier name: (identifier) @name)) @reference.call
(call_expression function: (field_expression field: (field_identifier) @name)) @reference.call
(macro_invocation macro: (identifier) @name) @reference.call

(use_declaration argument: (identifier) @_import.path)
(use_declaration argument: (scoped_identifier) @_import.path)
(use_declaration argument: (use_as_clause path: (_) @_import.path))
(use_declaration argument: (scoped_use_list path: (_) @_import.path))
(use_declaration argument: (use_wildcard (scoped_identifier) @_import.path))
