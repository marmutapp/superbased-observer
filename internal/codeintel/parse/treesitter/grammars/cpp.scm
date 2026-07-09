; C++ symbol + call + import captures (superset of C). See rust.scm for the
; capture vocabulary. Member definitions out of line use a qualified_identifier
; declarator; the backend dedups by span keeping the more specific kind.

(function_definition
  declarator: (function_declarator declarator: (identifier) @name)) @definition.function
(function_definition
  declarator: (function_declarator declarator: (field_identifier) @name)) @definition.method
(function_definition
  declarator: (function_declarator
    declarator: (qualified_identifier name: (identifier) @name))) @definition.method
(function_definition
  declarator: (pointer_declarator
    (function_declarator declarator: (identifier) @name))) @definition.function

(class_specifier name: (type_identifier) @name) @definition.class
(struct_specifier name: (type_identifier) @name) @definition.class
(union_specifier name: (type_identifier) @name) @definition.class
(enum_specifier name: (type_identifier) @name) @definition.class
(namespace_definition name: (namespace_identifier) @name) @definition.module
(type_definition declarator: (type_identifier) @name) @definition.type

(preproc_include path: (system_lib_string) @_import.path)
(preproc_include path: (string_literal) @_import.path)

(call_expression function: (identifier) @name) @reference.call
(call_expression function: (field_expression field: (field_identifier) @name)) @reference.call
(call_expression function: (qualified_identifier name: (identifier) @name)) @reference.call
