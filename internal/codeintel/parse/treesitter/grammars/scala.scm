; Scala symbol + call + import captures (Extended tier — S+I, best-effort C).
; See rust.scm for the capture vocabulary.

(class_definition name: (identifier) @name) @definition.class
(object_definition name: (identifier) @name) @definition.module
(trait_definition name: (identifier) @name) @definition.interface
(function_definition name: (identifier) @name) @definition.function
(type_definition name: (type_identifier) @name) @definition.type

; Imports are intentionally NOT captured: tree-sitter-scala flattens a dotted
; import path into per-segment `path:` fields (field("path", sep1(".", _identifier)))
; with no single node holding the full path, so any capture yields fragments.
; Scala ships S + C (symbols + calls); imports are best-effort omitted.

(call_expression function: (identifier) @name) @reference.call
