; TypeScript symbol + call + import captures (shared by .ts and .tsx —
; tsx.scm is identical, both grammars expose the same node types). See
; rust.scm for the capture vocabulary. Methods captured as
; method_definition; the backend dedups by span keeping the specific kind.

(function_declaration name: (identifier) @name) @definition.function
(generator_function_declaration name: (identifier) @name) @definition.function

(class_declaration name: (type_identifier) @name) @definition.class
(abstract_class_declaration name: (type_identifier) @name) @definition.class
(enum_declaration name: (identifier) @name) @definition.class
(interface_declaration name: (type_identifier) @name) @definition.interface
(type_alias_declaration name: (type_identifier) @name) @definition.type

(method_definition name: (property_identifier) @name) @definition.method

(call_expression function: (identifier) @name) @reference.call
(call_expression function: (member_expression property: (property_identifier) @name)) @reference.call

(import_statement source: (string) @_import.path)
