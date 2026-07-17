; Python symbol + call + import captures. See rust.scm for the capture
; vocabulary. Methods (function_definition inside a class block) are also
; matched by the plain function_definition pattern; the backend dedups by
; span and keeps the more specific kind (method > function).

(function_definition name: (identifier) @name) @definition.function
(class_definition body: (block (function_definition name: (identifier) @name) @definition.method))
(class_definition name: (identifier) @name) @definition.class

(call function: (identifier) @name) @reference.call
(call function: (attribute attribute: (identifier) @name)) @reference.call

(import_statement name: (dotted_name) @_import.path)
(import_statement name: (aliased_import name: (dotted_name) @_import.path))
(import_from_statement module_name: (dotted_name) @_import.path)
(import_from_statement module_name: (relative_import) @_import.path)
