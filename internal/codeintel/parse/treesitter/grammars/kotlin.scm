; Kotlin symbol + call + import captures (Extended tier — S+I, best-effort C).
; See rust.scm for the capture vocabulary. interfaces parse as
; class_declaration (interface modifier) so they report the class kind.

(class_declaration (type_identifier) @name) @definition.class
(object_declaration (type_identifier) @name) @definition.class
(function_declaration (simple_identifier) @name) @definition.function

(import_header (identifier) @_import.path)

(call_expression (simple_identifier) @name) @reference.call
