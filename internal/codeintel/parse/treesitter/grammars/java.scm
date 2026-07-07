; Java symbol + call + import captures. See rust.scm for the capture
; vocabulary. Methods/constructors are captured distinctly from types; the
; backend dedups by span keeping the more specific kind.

(class_declaration name: (identifier) @name) @definition.class
(interface_declaration name: (identifier) @name) @definition.interface
(enum_declaration name: (identifier) @name) @definition.class
(record_declaration name: (identifier) @name) @definition.class
(annotation_type_declaration name: (identifier) @name) @definition.interface

(method_declaration name: (identifier) @name) @definition.method
(constructor_declaration name: (identifier) @name) @definition.method

(import_declaration (scoped_identifier) @_import.path)

(method_invocation name: (identifier) @name) @reference.call
(object_creation_expression type: (type_identifier) @name) @reference.call
