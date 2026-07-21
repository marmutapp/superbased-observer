; Swift symbol + call + import captures (Extended tier — S+I, best-effort C).
; See rust.scm for the capture vocabulary. class_declaration covers
; class/struct/enum/extension/actor (declaration_kind) so they report the
; class kind; methods parse as function_declaration (function kind).

(class_declaration name: (type_identifier) @name) @definition.class
(protocol_declaration name: (type_identifier) @name) @definition.interface
(function_declaration name: (simple_identifier) @name) @definition.function

(import_declaration (identifier) @_import.path)

(call_expression (simple_identifier) @name) @reference.call
