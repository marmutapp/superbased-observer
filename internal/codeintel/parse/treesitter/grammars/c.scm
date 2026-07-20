; C symbol + call + import captures. See rust.scm for the capture
; vocabulary. The function name is nested inside function_declarator (and an
; optional pointer_declarator for pointer-returning functions). #include
; paths are captured raw (system_lib_string keeps its <…>, string_literal is
; unquoted by the host).

(function_definition
  declarator: (function_declarator declarator: (identifier) @name)) @definition.function
(function_definition
  declarator: (pointer_declarator
    (function_declarator declarator: (identifier) @name))) @definition.function

(struct_specifier name: (type_identifier) @name) @definition.class
(union_specifier name: (type_identifier) @name) @definition.class
(enum_specifier name: (type_identifier) @name) @definition.class
(type_definition declarator: (type_identifier) @name) @definition.type

(preproc_include path: (system_lib_string) @_import.path)
(preproc_include path: (string_literal) @_import.path)

(call_expression function: (identifier) @name) @reference.call
